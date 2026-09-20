package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	_ "modernc.org/sqlite"
)

type Store struct {
	dir     string
	db      *sql.DB
	options Options
	writeMu writerMutex
	blobMu  writerMutex
	// Background discovery/progress reads share one admission slot. Acquire
	// before querying and release after rows close; never hold across job work
	// or a writer lock. This leaves room in the four-connection pool for owner
	// queries when checkpointing, ingestion and background discovery overlap.
	maintenanceMu    writerMutex
	capacityMu       sync.Mutex
	pendingBytes     int64
	epoch            string
	accountingReady  atomic.Bool
	pricingReady     atomic.Bool
	checkpointStop   context.CancelFunc
	checkpointDone   chan struct{}
	checkpointMu     sync.Mutex
	checkpointStatus CheckpointStatus
	checkpointRun    sync.Mutex
	statsOnly        bool
	pendingBlobs     map[string]int
}

func Open(dir string, options Options) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(abs, "blobs"), 0700); err != nil {
		return nil, err
	}
	// Reserve the writer before write transactions read: the parser child and
	// hub have separate mutexes, so a deferred snapshot cannot safely upgrade
	// after the other process commits. The pinned driver's ReadOnly transactions
	// still use deferred BEGIN and remain concurrent WAL readers.
	// Bound mapped reads to the first 8 MiB per connection. SQLite maps them
	// read-only; writes still use its ordinary FULL-durable WAL path. This avoids
	// some Windows ReadFile stalls during checkpoint flushing without mapping an
	// unbounded historical corpus or growing the connection pool.
	// Reclaim excess WAL allocation at SQLite's normal writer reset rather than
	// forcing a TRUNCATE checkpoint that can hold readers/writers behind disk I/O.
	// This limits retained allocation, not an active transaction's required WAL.
	dsn := filepath.ToSlash(filepath.Join(abs, "ledger.sqlite")) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=mmap_size(8388608)&_pragma=synchronous(FULL)&_pragma=wal_autocheckpoint(0)&_pragma=journal_size_limit(67108864)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{dir: abs, db: db, options: options, pendingBlobs: make(map[string]int)}
	if err = s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	var mode string
	err = db.QueryRow(`SELECT value FROM properties WHERE key='storage_mode'`).Scan(&mode)
	if err != nil && err != sql.ErrNoRows {
		db.Close()
		return nil, err
	}
	s.statsOnly = mode == "stats-only"
	if options.ExternalCheckpointOwner {
		s.checkpointStatus = CheckpointStatus{State: "managed-by-owner"}
	} else {
		checkpointContext, cancel := context.WithCancel(context.Background())
		s.checkpointStop = cancel
		s.checkpointDone = make(chan struct{})
		s.checkpointStatus = CheckpointStatus{State: "waiting"}
		go s.runCheckpoints(checkpointContext)
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.checkpointStop != nil {
		s.checkpointStop()
		<-s.checkpointDone
	}
	return s.db.Close()
}
func (s *Store) RecoveryEpoch() string { return s.epoch }

func (s *Store) initialize() error {
	var engineVersion string
	if err := s.db.QueryRow(`SELECT sqlite_version()`).Scan(&engineVersion); err != nil {
		return err
	}
	if !supportedSQLite(engineVersion) {
		return fmt.Errorf("SQLite %s is below required WAL-fixed version 3.51.3", engineVersion)
	}
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 6 {
		return fmt.Errorf("ledger schema %d is newer than this binary", version)
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS properties (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS source_identity (
 source_id TEXT PRIMARY KEY, machine_id TEXT NOT NULL, provider TEXT NOT NULL,
 native_id TEXT NOT NULL, active_generation TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS source_identity_machine ON source_identity(machine_id,source_id);
CREATE TABLE IF NOT EXISTS sources (
 source_id TEXT NOT NULL, generation TEXT NOT NULL, meta_json BLOB NOT NULL,
 durable_offset INTEGER NOT NULL DEFAULT 0, indexed_offset INTEGER NOT NULL DEFAULT 0,
 parser_state BLOB NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY(source_id,generation), FOREIGN KEY(source_id) REFERENCES source_identity(source_id));
CREATE INDEX IF NOT EXISTS sources_offsets ON sources(durable_offset,indexed_offset);
CREATE TABLE IF NOT EXISTS chunks (
 source_id TEXT NOT NULL, generation TEXT NOT NULL, offset INTEGER NOT NULL,
 length INTEGER NOT NULL, sha256 TEXT NOT NULL, receipt_id TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL, PRIMARY KEY(source_id,generation,offset),
 FOREIGN KEY(source_id,generation) REFERENCES sources(source_id,generation));
CREATE INDEX IF NOT EXISTS chunks_sha256 ON chunks(sha256);
CREATE TABLE IF NOT EXISTS reclaimed_blobs (hash TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS sessions (
 id TEXT PRIMARY KEY, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 machine_id TEXT NOT NULL, provider TEXT NOT NULL, project TEXT NOT NULL,
 last_activity TEXT NOT NULL, projection BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_activity ON sessions(last_activity DESC,id);
CREATE INDEX IF NOT EXISTS sessions_machine ON sessions(machine_id,last_activity DESC,id);
CREATE INDEX IF NOT EXISTS sessions_source ON sessions(source_id,id);
CREATE INDEX IF NOT EXISTS sessions_source_generation ON sessions(source_id,generation,id);
CREATE TABLE IF NOT EXISTS events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
 session_id TEXT NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 agent_id TEXT NOT NULL, kind TEXT NOT NULL, timestamp TEXT NOT NULL,
 source_offset INTEGER NOT NULL, raw_length INTEGER NOT NULL, text TEXT NOT NULL, data BLOB NOT NULL, dedupe_key TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id,seq);
CREATE UNIQUE INDEX IF NOT EXISTS events_dedupe ON events(session_id,source_id,generation,dedupe_key) WHERE dedupe_key <> '';
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(text,content='');
CREATE TABLE IF NOT EXISTS usage_observations (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, source_id TEXT NOT NULL,
 generation TEXT NOT NULL, observation BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS usage_session ON usage_observations(session_id);
CREATE TABLE IF NOT EXISTS session_metadata (session_id TEXT PRIMARY KEY,revision INTEGER NOT NULL,value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS agent_names (session_id TEXT NOT NULL,agent_id TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(session_id,agent_id));
CREATE TABLE IF NOT EXISTS metadata_operations (
 operation_id TEXT PRIMARY KEY,session_id TEXT NOT NULL,request_hash TEXT NOT NULL,result BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY,name TEXT NOT NULL,color TEXT NOT NULL,revision INTEGER NOT NULL,deleted INTEGER NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS project_operations (operation_id TEXT PRIMARY KEY,request_hash TEXT NOT NULL,result BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS project_deletions (operation_id TEXT PRIMARY KEY,project_id TEXT NOT NULL UNIQUE,request_hash TEXT NOT NULL,result BLOB NOT NULL,state TEXT NOT NULL,processed INTEGER NOT NULL DEFAULT 0,updated_at TEXT NOT NULL,problem TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS project_deletions_pending ON project_deletions(state,updated_at,operation_id);
CREATE INDEX IF NOT EXISTS metadata_project_members ON session_metadata(json_extract(value,'$.project'),session_id);
CREATE TABLE IF NOT EXISTS project_audit (operation_id TEXT PRIMARY KEY,project_id TEXT NOT NULL,revision INTEGER NOT NULL,before_json BLOB NOT NULL,after_json BLOB NOT NULL,request_json BLOB NOT NULL,at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS project_audit_revision ON project_audit(project_id,revision);
CREATE TABLE IF NOT EXISTS organization_audit (
 operation_id TEXT PRIMARY KEY,session_id TEXT NOT NULL,revision INTEGER NOT NULL,
 before_json BLOB NOT NULL,after_json BLOB NOT NULL,patch_json BLOB NOT NULL,at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS organization_audit_session ON organization_audit(session_id,revision);
CREATE TABLE IF NOT EXISTS legacy_aliases (legacy_key TEXT PRIMARY KEY,session_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS machines (machine_id TEXT PRIMARY KEY,heartbeat BLOB NOT NULL,last_seen TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS machine_labels (machine_id TEXT PRIMARY KEY,display_name TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS machine_label_operations (operation_id TEXT PRIMARY KEY,request BLOB NOT NULL,result BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS machine_label_audit (machine_id TEXT NOT NULL,revision INTEGER NOT NULL,operation_id TEXT NOT NULL UNIQUE,before_json BLOB NOT NULL,after_json BLOB NOT NULL,at TEXT NOT NULL,PRIMARY KEY(machine_id,revision));
CREATE TABLE IF NOT EXISTS changes (seq INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,session_id TEXT NOT NULL,at TEXT NOT NULL);
`)
	if err != nil {
		return err
	}
	if err = s.migrateProjectionRevisions(context.Background()); err != nil {
		return err
	}
	if err = s.migrateRebuildVerification(context.Background()); err != nil {
		return err
	}
	if err = s.migrateLedgerTotals(context.Background()); err != nil {
		return err
	}
	if err = s.setupSearchCursors(); err != nil {
		return err
	}
	if _, err = s.db.Exec(delegationTaskSchema); err != nil {
		return err
	}
	if _, err = s.db.Exec(hookEvidenceSchema); err != nil {
		return err
	}
	if err = s.setupGitUndo(); err != nil {
		return err
	}
	if _, err = s.db.Exec(`INSERT OR IGNORE INTO properties(key,value) VALUES('recovery_epoch',?)`, randomID()); err != nil {
		return err
	}
	return s.db.QueryRow(`SELECT value FROM properties WHERE key='recovery_epoch'`).Scan(&s.epoch)
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func validID(s string) bool    { return s != "" && len(s) <= 512 && !strings.ContainsRune(s, 0) }
func validJSON(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
func (s *Store) blobPath(hash string) string { return filepath.Join(s.dir, "blobs", hash[:2], hash) }
func (s *Store) capacity(incoming int64) error {
	if s.options.AvailableBytes == nil || s.options.ReserveBytes <= 0 {
		return nil
	}
	n, err := s.options.AvailableBytes(s.dir)
	if err != nil {
		return err
	}
	if n < s.options.ReserveBytes || s.pendingBytes > n-s.options.ReserveBytes || incoming > n-s.options.ReserveBytes-s.pendingBytes {
		return ErrCapacity
	}
	return nil
}

// IngestChunk acknowledges raw durability, independently of parsing. Bytes and
// exact declared length are verified even for retries; metadata is never trusted
// as proof that a retried body is identical.
func (s *Store) IngestChunk(ctx context.Context, chunk protocol.Chunk, input io.Reader) (protocol.Receipt, error) {
	var empty protocol.Receipt
	a := chunk.Source
	if !validID(a.SourceID) || !validID(a.Generation) || !validID(a.MachineID) || !validID(a.Provider) ||
		a.GenerationSequence < 0 || chunk.Offset < 0 || chunk.Length < 0 || chunk.Length > protocol.MaxChunkBytes ||
		chunk.Offset > (1<<63-1)-chunk.Length || a.Size < chunk.Offset+chunk.Length {
		return empty, fmt.Errorf("%w: source or chunk bounds", ErrInvalid)
	}
	hash, err := hex.DecodeString(chunk.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return empty, fmt.Errorf("%w: SHA256", ErrInvalid)
	}
	chunk.SHA256 = strings.ToLower(chunk.SHA256)
	if input == nil {
		return empty, fmt.Errorf("%w: missing body", ErrInvalid)
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	s.capacityMu.Lock()
	err = s.capacity(chunk.Length)
	if err == nil {
		s.pendingBytes += chunk.Length
	}
	s.capacityMu.Unlock()
	if err != nil {
		return empty, err
	}
	defer func() { s.capacityMu.Lock(); s.pendingBytes -= chunk.Length; s.capacityMu.Unlock() }()
	// Temporary and final evidence files share a filesystem, making promotion an
	// atomic rename. An orphan left before the SQL commit is harmless on retry.
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "blobs"), ".incoming-*")
	if err != nil {
		return empty, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, digest), io.LimitReader(&contextReader{ctx: ctx, r: input}, chunk.Length+1))
	if copyErr == nil && (n != chunk.Length || hex.EncodeToString(digest.Sum(nil)) != chunk.SHA256) {
		copyErr = fmt.Errorf("%w: body length or checksum mismatch", ErrInvalid)
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return empty, copyErr
	}
	if closeErr != nil {
		return empty, closeErr
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	if s.statsOnly {
		var oldLength, indexed int64
		var oldHash, receiptID, machine, provider, native string
		e := s.db.QueryRowContext(ctx, `SELECT c.length,c.sha256,c.receipt_id,s.indexed_offset,i.machine_id,i.provider,i.native_id FROM chunks c JOIN sources s USING(source_id,generation) JOIN source_identity i USING(source_id) WHERE c.source_id=? AND c.generation=? AND c.offset=?`, a.SourceID, a.Generation, chunk.Offset).Scan(&oldLength, &oldHash, &receiptID, &indexed, &machine, &provider, &native)
		if e == nil {
			if oldLength != chunk.Length || oldHash != chunk.SHA256 || machine != a.MachineID || provider != a.Provider || native != a.NativeID {
				return empty, fmt.Errorf("%w: conflicting chunk", ErrConflict)
			}
			return protocol.Receipt{ReceiptID: receiptID, SourceID: a.SourceID, Generation: a.Generation, DurableOffset: chunk.Offset + chunk.Length, IndexedOffset: indexed, RecoveryEpoch: s.epoch}, nil
		}
		if e != sql.ErrNoRows {
			return empty, e
		}
	}
	// Durable blob publication can flush or verify up to a full chunk. Keep it
	// outside both the SQL transaction and the metadata/heartbeat writer queue.
	// A rejected/cancelled transaction may leave an unreferenced immutable blob;
	// it is not acknowledged and can be reused by a subsequent valid upload.
	if chunk.Length > 0 {
		if err = s.publishBlob(ctx, tmpName, chunk.SHA256, chunk.Length); err != nil {
			return empty, err
		}
		if s.statsOnly {
			defer s.releasePendingBlob(chunk.SHA256)
		}
	}
	// Only ledger publication is serialized after all raw disk work finishes.
	if err = s.writeMu.LockContext(ctx); err != nil {
		return empty, err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var machine, provider, native string
	var activeSequence int64
	err = tx.QueryRowContext(ctx, `SELECT i.machine_id,i.provider,i.native_id,COALESCE(json_extract(s.meta_json,'$.generationSequence'),0) FROM source_identity i LEFT JOIN sources s ON s.source_id=i.source_id AND s.generation=i.active_generation WHERE i.source_id=?`, a.SourceID).Scan(&machine, &provider, &native, &activeSequence)
	if err == nil && (machine != a.MachineID || provider != a.Provider || native != a.NativeID) {
		return empty, fmt.Errorf("%w: source identity changed", ErrConflict)
	}
	if err != nil && err != sql.ErrNoRows {
		return empty, err
	}
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO source_identity VALUES(?,?,?,?,?)`, a.SourceID, a.MachineID, a.Provider, a.NativeID, a.Generation)
		if err != nil {
			return empty, err
		}
	}
	var durable, indexed int64
	var previousSequence int64
	err = tx.QueryRowContext(ctx, `SELECT durable_offset,indexed_offset,COALESCE(json_extract(meta_json,'$.generationSequence'),0) FROM sources WHERE source_id=? AND generation=?`, a.SourceID, a.Generation).Scan(&durable, &indexed, &previousSequence)
	if err == nil && previousSequence != a.GenerationSequence {
		return empty, fmt.Errorf("%w: generation ordinal changed", ErrConflict)
	}
	newSource := err == sql.ErrNoRows
	if newSource {
		if chunk.Offset != 0 {
			return empty, fmt.Errorf("%w: new generation must start at zero", ErrConflict)
		}
		if a.GenerationSequence > 0 {
			var ordinalOwner string
			ordinalErr := tx.QueryRowContext(ctx, `SELECT generation FROM sources WHERE source_id=? AND json_extract(meta_json,'$.generationSequence')=? LIMIT 1`, a.SourceID, a.GenerationSequence).Scan(&ordinalOwner)
			if ordinalErr == nil {
				return empty, fmt.Errorf("%w: generation ordinal already belongs to %s", ErrConflict, ordinalOwner)
			}
			if ordinalErr != sql.ErrNoRows {
				return empty, ordinalErr
			}
		}
		meta, _ := json.Marshal(a)
		now := stamp(time.Now())
		// Publish the accepted offset in the initial row. The chunk receipt and
		// ledger-total trigger still commit atomically below; avoid a second
		// source/index/total rewrite for every newly discovered generation.
		_, err = tx.ExecContext(ctx, `INSERT INTO sources(source_id,generation,meta_json,durable_offset,created_at,updated_at) VALUES(?,?,?,?,?,?)`, a.SourceID, a.Generation, meta, chunk.Offset+chunk.Length, now, now)
		if err == nil && (a.GenerationSequence > activeSequence || (a.GenerationSequence == 0 && activeSequence == 0)) {
			_, err = tx.ExecContext(ctx, `UPDATE source_identity SET active_generation=? WHERE source_id=?`, a.Generation, a.SourceID)
		}
	}
	if err != nil {
		return empty, err
	}
	var oldLength int64
	var oldHash, receiptID string
	err = tx.QueryRowContext(ctx, `SELECT length,sha256,receipt_id FROM chunks WHERE source_id=? AND generation=? AND offset=?`, a.SourceID, a.Generation, chunk.Offset).Scan(&oldLength, &oldHash, &receiptID)
	if err == nil {
		if oldLength != chunk.Length || oldHash != chunk.SHA256 {
			return empty, fmt.Errorf("%w: conflicting chunk", ErrConflict)
		}
		// The original committed offset, not a later received prefix, defines the
		// receipt. Lost ACK replay is stable even after subsequent appends.
		return protocol.Receipt{ReceiptID: receiptID, SourceID: a.SourceID, Generation: a.Generation, DurableOffset: chunk.Offset + chunk.Length, IndexedOffset: indexed, RecoveryEpoch: s.epoch}, nil
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	if chunk.Offset != durable {
		return empty, fmt.Errorf("%w: expected offset %d", ErrConflict, durable)
	}
	// Empty chunks register an empty source but are not immutable chunk slots:
	// later data is permitted at the same offset.
	if chunk.Length > 0 {
		receiptID = randomID()
		_, err = tx.ExecContext(ctx, `INSERT INTO chunks VALUES(?,?,?,?,?,?,?)`, a.SourceID, a.Generation, chunk.Offset, chunk.Length, chunk.SHA256, receiptID, stamp(time.Now()))
		if err != nil {
			return empty, err
		}
		if s.statsOnly {
			if _, err = tx.ExecContext(ctx, `DELETE FROM reclaimed_blobs WHERE hash=?`, chunk.SHA256); err != nil {
				return empty, err
			}
		}
	} else {
		receiptID = "empty-" + a.SourceID + "-" + a.Generation
	}
	if !newSource {
		meta, _ := json.Marshal(a)
		_, err = tx.ExecContext(ctx, `UPDATE sources SET durable_offset=?,meta_json=?,updated_at=? WHERE source_id=? AND generation=?`, chunk.Offset+chunk.Length, meta, stamp(time.Now()), a.SourceID, a.Generation)
		if err != nil {
			return empty, err
		}
	}
	if s.options.BeforeCommit != nil {
		if err = s.options.BeforeCommit(); err != nil {
			return empty, err
		}
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return protocol.Receipt{ReceiptID: receiptID, SourceID: a.SourceID, Generation: a.Generation, DurableOffset: chunk.Offset + chunk.Length, IndexedOffset: indexed, RecoveryEpoch: s.epoch}, nil
}

// Serialize same-store promotions separately from ledger writers. This retains
// verify-before-reuse semantics without allowing simultaneous identical uploads
// to race the existence check and rename. Admission is bounded and cancellable.
func (s *Store) publishBlob(ctx context.Context, tmpName, hash string, size int64) error {
	if err := s.blobMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.blobMu.Unlock()
	final := s.blobPath(hash)
	if err := os.MkdirAll(filepath.Dir(final), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(final); err == nil {
		if err := verifyFile(final, hash, size); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err := promoteBlob(tmpName, final); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(final)); err != nil {
			return err
		}
	}
	if s.statsOnly {
		s.pendingBlobs[hash]++
	}
	return nil
}

func (s *Store) releasePendingBlob(hash string) {
	s.blobMu.Lock()
	if s.pendingBlobs[hash] <= 1 {
		delete(s.pendingBlobs, hash)
	} else {
		s.pendingBlobs[hash]--
	}
	s.blobMu.Unlock()
}

func verifyFile(path, hash string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != hash {
		return fmt.Errorf("raw blob integrity failure: %s", filepath.Base(path))
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *Store) SourceState(ctx context.Context, sourceID, generation string) (SourceState, error) {
	var state SourceState
	var meta []byte
	var parserState []byte
	err := s.db.QueryRowContext(ctx, `SELECT meta_json,durable_offset,indexed_offset,parser_state,COALESCE((SELECT revision FROM active_projection p WHERE p.source_id=sources.source_id AND p.generation=sources.generation),''),COALESCE((SELECT value='true' FROM properties p WHERE p.key='external_index:'||sources.source_id),0) FROM sources WHERE source_id=? AND generation=?`, sourceID, generation).Scan(&meta, &state.DurableOffset, &state.IndexedOffset, &parserState, &state.ProjectionRevision, &state.ExternalIndex)
	if err == sql.ErrNoRows {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(meta, &state.Source)
	state.ParserState = parserState
	return state, err
}
func (s *Store) SourceOffset(ctx context.Context, sourceID, generation string) (protocol.Receipt, error) {
	st, err := s.SourceState(ctx, sourceID, generation)
	return protocol.Receipt{SourceID: sourceID, Generation: generation, DurableOffset: st.DurableOffset, IndexedOffset: st.IndexedOffset, RecoveryEpoch: s.epoch}, err
}

// SourcesAfter enumerates stable inventory pages without allocating the corpus.
func (s *Store) SourcesAfter(ctx context.Context, afterID, afterGeneration string, limit int) ([]SourceState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT meta_json,durable_offset,indexed_offset,parser_state,COALESCE((SELECT revision FROM active_projection p WHERE p.source_id=sources.source_id AND p.generation=sources.generation),''),COALESCE((SELECT value='true' FROM properties p WHERE p.key='external_index:'||sources.source_id),0) FROM sources WHERE source_id>? OR (source_id=? AND generation>?) ORDER BY source_id,generation LIMIT ?`, afterID, afterID, afterGeneration, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceState{}
	for rows.Next() {
		var st SourceState
		var meta []byte
		var parserState []byte
		if err = rows.Scan(&meta, &st.DurableOffset, &st.IndexedOffset, &parserState, &st.ProjectionRevision, &st.ExternalIndex); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(meta, &st.Source); err != nil {
			return nil, err
		}
		st.ParserState = parserState
		out = append(out, st)
	}
	return out, rows.Err()
}

// OpenSource streams a stable durable snapshot through bounded chunks. It never
// holds a SQL read transaction while the parser consumes bytes.
func (s *Store) OpenSource(ctx context.Context, sourceID, generation string, offset int64) (io.ReadCloser, error) {
	if !validID(sourceID) || !validID(generation) || offset < 0 {
		return nil, ErrInvalid
	}
	var end int64
	err := s.db.QueryRowContext(ctx, `SELECT durable_offset FROM sources WHERE source_id=? AND generation=?`, sourceID, generation).Scan(&end)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if offset > end {
		return nil, ErrInvalid
	}
	return &sourceReader{store: s, ctx: ctx, sourceID: sourceID, generation: generation, pos: offset, end: end}, nil
}

type sourceReader struct {
	store                *Store
	ctx                  context.Context
	sourceID, generation string
	pos, end             int64
	file                 *os.File
	remaining            int64
	closed               bool
}

func (r *sourceReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if r.file == nil {
		var off, length int64
		var hash string
		err := r.store.db.QueryRowContext(r.ctx, `SELECT offset,length,sha256 FROM chunks WHERE source_id=? AND generation=? AND offset<=? AND offset+length>? ORDER BY offset DESC LIMIT 1`, r.sourceID, r.generation, r.pos, r.pos).Scan(&off, &length, &hash)
		if err != nil {
			return 0, fmt.Errorf("missing committed raw range at %d: %w", r.pos, err)
		}
		r.file, err = os.Open(r.store.blobPath(hash))
		if err != nil {
			return 0, err
		}
		if _, err = r.file.Seek(r.pos-off, io.SeekStart); err != nil {
			r.Close()
			return 0, err
		}
		r.remaining = length - (r.pos - off)
	}
	nmax := int64(len(p))
	if nmax > r.remaining {
		nmax = r.remaining
	}
	if nmax > r.end-r.pos {
		nmax = r.end - r.pos
	}
	n, err := r.file.Read(p[:nmax])
	r.pos += int64(n)
	r.remaining -= int64(n)
	if r.remaining == 0 {
		r.file.Close()
		r.file = nil
		if err == io.EOF {
			err = nil
		}
	}
	if err == io.EOF && r.pos < r.end {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (r *sourceReader) Close() error {
	r.closed = true
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

func (s *Store) RecordHeartbeat(ctx context.Context, h protocol.Heartbeat) error {
	if !validID(h.MachineID) || (h.HistoryGaps != nil && *h.HistoryGaps < 0) || !h.Network.Valid() {
		return ErrInvalid
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err = s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := stamp(time.Now())
	// Machine names participate in catalog searches. Publish renames atomically
	// without invalidating historical reads for unchanged periodic heartbeats.
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) SELECT 'machine-name','',? WHERE NOT EXISTS(SELECT 1 FROM machines WHERE machine_id=? AND COALESCE(json_extract(heartbeat,'$.name'),'')=?)`, at, h.MachineID, h.Name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO machines VALUES(?,?,?) ON CONFLICT(machine_id) DO UPDATE SET heartbeat=excluded.heartbeat,last_seen=excluded.last_seen`, h.MachineID, b, at); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT heartbeat,last_seen,COALESCE(l.display_name,''),COALESCE(l.revision,0),COALESCE(l.updated_at,'') FROM machines m LEFT JOIN machine_labels l ON l.machine_id=m.machine_id ORDER BY m.machine_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		var b []byte
		var at string
		if err = rows.Scan(&b, &at, &m.Label.DisplayName, &m.Label.Revision, &m.Label.UpdatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &m.Heartbeat); err != nil {
			return nil, err
		}
		m.Label.MachineID = m.Heartbeat.MachineID
		m.LastSeen, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
