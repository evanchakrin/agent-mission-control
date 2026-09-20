package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Raw generations describe collector evidence. Projection revisions describe
// independent parser interpretations of those same immutable bytes.
func (s *Store) migrateProjectionRevisions(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 4 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 2 || version == 3 {
		if version == 2 {
			if _, err = tx.ExecContext(ctx, `ALTER TABLE projection_revisions ADD COLUMN retry_at TEXT NOT NULL DEFAULT '';CREATE INDEX projection_retry ON projection_revisions(state,retry_at,updated_at,revision);`); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `CREATE TABLE baseline_projections(source_id TEXT NOT NULL,generation TEXT NOT NULL,indexed_offset INTEGER NOT NULL,parser_state BLOB NOT NULL,projection BLOB NOT NULL,recorded_at TEXT NOT NULL,PRIMARY KEY(source_id,generation));PRAGMA user_version=4;`)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
ALTER TABLE events ADD COLUMN projection_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_observations ADD COLUMN projection_revision TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS events_dedupe;
CREATE UNIQUE INDEX events_dedupe ON events(session_id,source_id,generation,projection_revision,dedupe_key) WHERE dedupe_key<>'';
CREATE INDEX events_revision ON events(source_id,generation,projection_revision,seq);
CREATE INDEX usage_revision ON usage_observations(source_id,generation,projection_revision,id);
CREATE TABLE active_projection(source_id TEXT NOT NULL,generation TEXT NOT NULL,revision TEXT NOT NULL,PRIMARY KEY(source_id,generation));
CREATE TABLE projection_revisions(
 revision TEXT PRIMARY KEY,operation_id TEXT NOT NULL UNIQUE,source_id TEXT NOT NULL,generation TEXT NOT NULL,
 parser_version TEXT NOT NULL,expected_revision TEXT NOT NULL,target_offset INTEGER NOT NULL,
 indexed_offset INTEGER NOT NULL DEFAULT 0,parser_state BLOB NOT NULL DEFAULT '{}',projection BLOB NOT NULL,
 state TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',budget_bytes INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX projection_building ON projection_revisions(source_id,generation) WHERE state IN('building','verifying','ready');
CREATE INDEX projection_work ON projection_revisions(state,updated_at,revision);
ALTER TABLE projection_revisions ADD COLUMN retry_at TEXT NOT NULL DEFAULT '';
CREATE INDEX projection_retry ON projection_revisions(state,retry_at,updated_at,revision);
CREATE TABLE baseline_projections(source_id TEXT NOT NULL,generation TEXT NOT NULL,indexed_offset INTEGER NOT NULL,parser_state BLOB NOT NULL,projection BLOB NOT NULL,recorded_at TEXT NOT NULL,PRIMARY KEY(source_id,generation));
PRAGMA user_version=4;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Revision-scoped storage IDs keep global event sequences stable while allowing
// native message IDs and deduplication keys to recur in a different parser run.
func revisionID(revision, id string) string {
	if revision == "" {
		return id
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(revision), revision, len(id), id)))
	return hex.EncodeToString(h[:])
}

type ProjectionRevision struct {
	Revision         string               `json:"revision"`
	OperationID      string               `json:"operationId"`
	SourceID         string               `json:"sourceId"`
	Generation       string               `json:"generation"`
	ParserVersion    string               `json:"parserVersion"`
	ExpectedRevision string               `json:"expectedRevision"`
	TargetOffset     int64                `json:"targetOffset"`
	IndexedOffset    int64                `json:"indexedOffset"`
	ParserState      json.RawMessage      `json:"-"`
	Session          Session              `json:"session"`
	State            string               `json:"state"`
	Error            string               `json:"error,omitempty"`
	BudgetBytes      int64                `json:"budgetBytes"`
	CreatedAt        time.Time            `json:"createdAt"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	Verification     VerificationProgress `json:"verification"`
}

const revisionColumns = `revision,operation_id,source_id,generation,parser_version,expected_revision,target_offset,indexed_offset,parser_state,projection,state,error,budget_bytes,created_at,updated_at,verification`

type rowScanner interface{ Scan(...any) error }

func scanRevision(row rowScanner) (ProjectionRevision, error) {
	var r ProjectionRevision
	var checkpoint, projection, verification []byte
	var created, updated string
	err := row.Scan(&r.Revision, &r.OperationID, &r.SourceID, &r.Generation, &r.ParserVersion, &r.ExpectedRevision, &r.TargetOffset, &r.IndexedOffset, &checkpoint, &projection, &r.State, &r.Error, &r.BudgetBytes, &created, &updated, &verification)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.ParserState = checkpoint
	// Keep damaged jobs inspectable; the verifier independently fails closed.
	if json.Unmarshal(verification, &r.Verification) != nil {
		r.Verification = VerificationProgress{Phase: "damaged"}
	}
	if err = json.Unmarshal(projection, &r.Session); err != nil {
		return r, err
	}
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return r, err
	}
	r.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return r, err
}
func (s *Store) ProjectionRevision(ctx context.Context, id string) (ProjectionRevision, error) {
	return scanRevision(s.db.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM projection_revisions WHERE revision=?`, id))
}

// BeginRebuild is idempotent by operation ID. It only reserves a staging job;
// current sessions, checkpoints, search results and organization remain intact.
func (s *Store) BeginRebuild(ctx context.Context, sessionID, operationID, parserVersion string) (ProjectionRevision, error) {
	var empty ProjectionRevision
	if !validID(sessionID) || !validID(operationID) || parserVersion == "" || len(parserVersion) > 128 {
		return empty, ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	prior, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM projection_revisions WHERE operation_id=?`, operationID))
	if err == nil {
		if prior.Session.ID != sessionID || prior.ParserVersion != parserVersion {
			return empty, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return empty, err
	}
	var projection []byte
	var target int64
	var generation, activeRevision string
	var external bool
	err = tx.QueryRowContext(ctx, `SELECT x.projection,s.durable_offset,i.active_generation,COALESCE(p.revision,''),COALESCE((SELECT value='true' FROM properties WHERE key='external_index:'||s.source_id),0)
 FROM sessions x JOIN sources s ON s.source_id=x.source_id AND s.generation=x.generation JOIN source_identity i ON i.source_id=s.source_id
 LEFT JOIN active_projection p ON p.source_id=s.source_id AND p.generation=s.generation WHERE x.id=?`, sessionID).Scan(&projection, &target, &generation, &activeRevision, &external)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	var current Session
	if err = json.Unmarshal(projection, &current); err != nil {
		return empty, err
	}
	if external {
		return empty, fmt.Errorf("%w: external normalizer needs its own rebuild adapter", ErrInvalid)
	}
	if current.Generation != generation {
		return empty, fmt.Errorf("%w: a newer source generation is waiting for indexing", ErrConflict)
	}
	if target > ((1<<63-1)-(64<<20))/2 {
		return empty, ErrCapacity
	}
	budget := int64(64<<20) + 2*target
	s.capacityMu.Lock()
	err = s.capacity(budget)
	s.capacityMu.Unlock()
	if err != nil {
		return empty, err
	}
	r := ProjectionRevision{Revision: randomID(), OperationID: operationID, SourceID: current.SourceID, Generation: generation, ParserVersion: parserVersion, ExpectedRevision: activeRevision, TargetOffset: target, ParserState: json.RawMessage(`{}`), State: "building", BudgetBytes: budget, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	r.Session = Session{ID: sessionID, MachineID: current.MachineID, SourceID: r.SourceID, Generation: generation, ProjectionRevision: r.Revision, Provider: current.Provider, Completeness: "building"}
	data, err := json.Marshal(r.Session)
	if err != nil {
		return empty, err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT revision FROM projection_revisions WHERE source_id=? AND generation=? AND state IN('building','verifying','ready')`, r.SourceID, r.Generation).Scan(&existing)
	if err == nil {
		return empty, fmt.Errorf("%w: rebuild already in progress", ErrConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO projection_revisions(`+revisionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Revision, r.OperationID, r.SourceID, r.Generation, r.ParserVersion, r.ExpectedRevision, r.TargetOffset, 0, []byte(`{}`), data, r.State, "", budget, stamp(r.CreatedAt), stamp(r.UpdatedAt), []byte(`{}`))
	if err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return r, nil
}

// RebuildSource loads only one bounded parser checkpoint, never a corpus catalog.
func (s *Store) RebuildSource(ctx context.Context, revision string) (SourceState, ProjectionRevision, error) {
	r, err := s.ProjectionRevision(ctx, revision)
	if err != nil {
		return SourceState{}, r, err
	}
	src, err := s.SourceState(ctx, r.SourceID, r.Generation)
	if err != nil {
		return src, r, err
	}
	if r.State != "building" {
		return src, r, fmt.Errorf("%w: rebuild is not accepting parser batches", ErrConflict)
	}
	src.DurableOffset = r.TargetOffset
	src.IndexedOffset = r.IndexedOffset
	src.ParserState = r.ParserState
	src.ProjectionRevision = r.Revision
	return src, r, nil
}

// ReadyRebuild is the synchronous maintenance/test convenience. Service workers
// call VerifyRebuildBatch once per dispatch so verification cannot consume an
// unbounded parser request or starve other sources.
func (s *Store) ReadyRebuild(ctx context.Context, id string) error {
	for {
		done, err := s.VerifyRebuildBatch(ctx, id)
		if err != nil || done {
			return err
		}
	}
}

// PublishRebuild changes one pointer and one bounded session snapshot. All event,
// search, usage and analytics queries join that same revision before pagination.
// Organization is deliberately absent from this transaction.
func (s *Store) PublishRebuild(ctx context.Context, id string) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM projection_revisions WHERE revision=?`, id))
	if err != nil {
		return err
	}
	if r.State == "active" || r.State == "retired" {
		return nil
	} // lost publication response; do not reactivate retired history
	if r.State != "ready" {
		return fmt.Errorf("%w: rebuild has not passed verification", ErrConflict)
	}
	if err := validateHookCheckpoint(ctx, tx, r); err != nil {
		return err
	}
	var generation, revision string
	err = tx.QueryRowContext(ctx, `SELECT i.active_generation,COALESCE(p.revision,'') FROM source_identity i LEFT JOIN active_projection p ON p.source_id=i.source_id AND p.generation=? WHERE i.source_id=?`, r.Generation, r.SourceID).Scan(&generation, &revision)
	if err != nil {
		return err
	}
	if generation != r.Generation || revision != r.ExpectedRevision {
		return fmt.Errorf("%w: newer source generation or parser revision exists", ErrConflict)
	}
	if r.ExpectedRevision == "" {
		// The pre-revision projection remains available as historical evidence.
		// Capture it at publication, after its final concurrent indexing commit.
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO baseline_projections SELECT s.source_id,s.generation,p.indexed_offset,p.parser_state,s.projection,? FROM sessions s JOIN sources p ON p.source_id=s.source_id AND p.generation=s.generation WHERE s.id=? AND s.source_id=? AND s.generation=?`, stamp(time.Now()), r.Session.ID, r.SourceID, r.Generation)
		if err != nil {
			return err
		}
	}
	r.Session.Metadata = Metadata{}
	r.Session.CostEstimate = nil
	r.Session.Pricing = nil
	projection, err := json.Marshal(r.Session)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO active_projection VALUES(?,?,?) ON CONFLICT(source_id,generation) DO UPDATE SET revision=excluded.revision`, r.SourceID, r.Generation, r.Revision)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET generation=?,last_activity=?,project=?,projection=? WHERE id=? AND source_id=?`, r.Generation, stamp(r.Session.LastActivity), r.Session.Project, projection, r.Session.ID, r.SourceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sources SET indexed_offset=?,parser_state=? WHERE source_id=? AND generation=?`, r.IndexedOffset, validJSON(r.ParserState), r.SourceID, r.Generation)
	if err != nil {
		return err
	}
	var hasQueue bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='index_work')`).Scan(&hasQueue); err != nil {
		return err
	}
	if hasQueue {
		_, err = tx.ExecContext(ctx, `DELETE FROM index_work WHERE source_id=? AND generation=?;INSERT INTO index_work SELECT source_id,generation,durable_offset,'','ready','' FROM sources WHERE source_id=? AND generation=? AND indexed_offset<durable_offset`, r.SourceID, r.Generation, r.SourceID, r.Generation)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE projection_revisions SET state='retired',updated_at=? WHERE revision=? AND state='active'`, stamp(time.Now()), r.ExpectedRevision)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE projection_revisions SET state='active',projection=?,updated_at=? WHERE revision=?`, projection, stamp(time.Now()), r.Revision)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('projection',?,?)`, r.Session.ID, stamp(time.Now()))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO properties VALUES('last_index_progress',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, stamp(time.Now()))
	if err != nil {
		return err
	}
	if s.options.BeforeCommit != nil {
		if err = s.options.BeforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RebuildWork(ctx context.Context, limit int) ([]string, error) {
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.maintenanceMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT revision FROM projection_revisions WHERE state IN('building','verifying','ready') AND retry_at<=? ORDER BY updated_at,revision LIMIT ?`, stamp(time.Now()), pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type RebuildProgress struct {
	Pending int64  `json:"pending"`
	Blocked int64  `json:"blocked"`
	Problem string `json:"problem,omitempty"`
}

func (s *Store) RebuildProgress(ctx context.Context) (RebuildProgress, error) {
	var p RebuildProgress
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return p, err
	}
	defer s.maintenanceMu.Unlock()
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(error<>''),0),COALESCE(MAX(error),'') FROM projection_revisions WHERE state IN('building','verifying','ready')`).Scan(&p.Pending, &p.Blocked, &p.Problem)
	return p, err
}

func (s *Store) RebuildProblem(ctx context.Context, id string, problem error) error {
	message := ""
	retry := ""
	if problem != nil {
		message = problem.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
		delay := 5 * time.Minute
		// Contention rolled back this attempt, not the durable rebuild. Match
		// the normal indexer's bounded retry instead of pausing five minutes.
		if IsContention(problem) {
			delay = 2 * time.Second
		}
		retry = stamp(time.Now().Add(delay))
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE projection_revisions SET error=?,retry_at=?,updated_at=? WHERE revision=? AND state IN('building','verifying','ready')`, message, retry, stamp(time.Now()), id)
	return err
}
