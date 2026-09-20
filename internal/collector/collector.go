// Package collector captures explicit transcript roots into a durable bounded
// spool. Collection and network progress are independent of the heartbeat.
package collector

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	_ "modernc.org/sqlite"
)

const defaultSpoolBytes int64 = 20 << 30

type Root struct {
	Path     string `json:"path"`
	Provider string `json:"provider"`
}

type Config struct {
	DataDir, MachineID, Name, Version, HubURL, Token                                string
	Roots                                                                           []Root
	SpoolMaxBytes, BytesPerSecond                                                   int64
	Debounce, ReconcileInterval, HeartbeatInterval, RequestTimeout, ProgressTimeout time.Duration
	HTTPClient                                                                      *http.Client
}

type Status struct {
	MachineID       string     `json:"machineId"`
	CollectionState string     `json:"collectionState"`
	ConnectionState string     `json:"connectionState"`
	UploadRetryAt   *time.Time `json:"uploadRetryAt,omitempty"`
	Error           string     `json:"error,omitempty"`
	CollectionError string     `json:"collectionError,omitempty"`
	ConnectionError string     `json:"connectionError,omitempty"`
	Sources         int64      `json:"sources"`
	CapturedBytes   int64      `json:"capturedBytes"`
	UploadedBytes   int64      `json:"uploadedBytes"`
	BacklogBytes    int64      `json:"backlogBytes"`
	SpoolBytes      int64      `json:"spoolBytes"`
	Gaps            int64      `json:"gaps"`
	LastHeartbeatAt *time.Time `json:"lastHeartbeatAt,omitempty"`
	LastCaptureAt   *time.Time `json:"lastCaptureAt,omitempty"`
	LastUploadAt    *time.Time `json:"lastUploadAt,omitempty"`
}

type Collector struct {
	cfg           Config
	db            *sql.DB
	client        *http.Client
	bootID        string
	started       time.Time
	mu            sync.Mutex
	state         Status
	networkReport *protocol.NetworkReport // immutable after publication under mu
	captureMu     sync.Mutex
	leaseMu       sync.Mutex
	rateMu        sync.Mutex
	nextSend      time.Time
	notify        chan struct{}
	uploadNotify  [2]chan struct{}
	recover       chan struct{}
	freeSpace     func(string) (uint64, uint64, error)
}

func Open(cfg Config) (*Collector, error) {
	if cfg.DataDir == "" || len(cfg.Roots) == 0 {
		return nil, errors.New("collector requires a data directory and explicit transcript roots")
	}
	u, err := url.Parse(cfg.HubURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("collector hub URL must be an HTTP(S) address without credentials")
	}
	if cfg.SpoolMaxBytes == 0 {
		cfg.SpoolMaxBytes = defaultSpoolBytes
	}
	if cfg.BytesPerSecond == 0 {
		cfg.BytesPerSecond = 2 << 20
	}
	if cfg.SpoolMaxBytes < protocol.MaxChunkBytes || cfg.BytesPerSecond < 1 {
		return nil, errors.New("spool must hold at least one chunk and bandwidth must be positive")
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 2 * time.Second
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 15 * time.Minute
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.ProgressTimeout <= 0 {
		cfg.ProgressTimeout = 10 * time.Second
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	for i := range cfg.Roots {
		cfg.Roots[i].Path, err = filepath.Abs(cfg.Roots[i].Path)
		if err != nil {
			return nil, err
		}
		if cfg.Roots[i].Provider != "claude" && cfg.Roots[i].Provider != "codex" {
			return nil, fmt.Errorf("unsupported root provider %q", cfg.Roots[i].Provider)
		}
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(cfg.DataDir, "chunks"), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "spool.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Collector, error) { db.Close(); return nil, err }
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA cache_size=-8192", "PRAGMA wal_autocheckpoint=1000", spoolSchema} {
		if _, err = db.Exec(q); err != nil {
			return fail(err)
		}
	}
	var hasSequence int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('generations') WHERE name='generation_sequence'").Scan(&hasSequence); err != nil {
		return fail(err)
	}
	if hasSequence == 0 {
		if _, err = db.Exec("ALTER TABLE generations ADD COLUMN generation_sequence INTEGER NOT NULL DEFAULT 1"); err != nil {
			return fail(err)
		}
		// The original candidate had no ordinal. Current is authoritative; old
		// generations remain zero until explicitly reconciled, never superseding it.
		if _, err = db.Exec(`UPDATE generations SET generation_sequence=CASE WHEN generation=(SELECT current_generation FROM sources WHERE id=source_id) THEN 1 ELSE 0 END`); err != nil {
			return fail(err)
		}
	}
	var hasUploadOrder int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('sources') WHERE name='last_upload_attempt'").Scan(&hasUploadOrder); err != nil {
		return fail(err)
	}
	if hasUploadOrder == 0 {
		if _, err = db.Exec("ALTER TABLE sources ADD COLUMN last_upload_attempt INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fail(err)
		}
	}
	var storedID string
	if err = setupUploadQueue(db); err != nil {
		return fail(err)
	}
	err = db.QueryRow("SELECT value FROM settings WHERE key='machine_id'").Scan(&storedID)
	if errors.Is(err, sql.ErrNoRows) {
		storedID = cfg.MachineID
		if storedID == "" {
			storedID = identifier()
		}
		if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('machine_id',?)", storedID); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	}
	if cfg.MachineID != "" && cfg.MachineID != storedID {
		return fail(errors.New("configured machine ID differs from the durable collector identity"))
	}
	cfg.MachineID = storedID
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: cfg.ProgressTimeout, DisableCompression: true}}
	}
	c := &Collector{cfg: cfg, db: db, client: client, bootID: identifier(), started: time.Now(), notify: make(chan struct{}, 1), recover: make(chan struct{}, 1), freeSpace: diskSpace}
	for i := range c.uploadNotify {
		c.uploadNotify[i] = make(chan struct{}, 1)
	}
	c.state = Status{MachineID: storedID, CollectionState: "starting", ConnectionState: "connecting"}
	// A dead process cannot own leases. All payloads remain available for retry.
	if _, err = db.Exec("UPDATE chunks SET lease_until=0 WHERE ack_at IS NULL"); err != nil {
		return fail(err)
	}
	if err = c.removeOrphanTemps(); err != nil {
		return fail(err)
	}
	var recoveryPending int
	if err = db.QueryRow("SELECT count(*) FROM settings WHERE key='recovery_pending'").Scan(&recoveryPending); err != nil {
		return fail(err)
	}
	if recoveryPending != 0 {
		c.recover <- struct{}{}
	}
	return c, nil
}

func identifier() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (c *Collector) Close() error { c.client.CloseIdleConnections(); return c.db.Close() }

func (c *Collector) Status(ctx context.Context) (Status, error) {
	c.mu.Lock()
	result := c.state
	c.mu.Unlock()
	pause, err := readUploadPause(ctx, c.db)
	if err != nil {
		return result, err
	}
	if pause.Until > time.Now().UnixNano() {
		retry := time.Unix(0, pause.Until).UTC()
		result.UploadRetryAt = &retry
		result.ConnectionState = pause.State
		result.ConnectionError = "uploads paused: " + pause.State
	}
	result.Error = strings.TrimSpace(strings.Join([]string{result.CollectionError, result.ConnectionError}, " "))
	err = c.db.QueryRowContext(ctx, `SELECT sources,captured,uploaded,backlog,spool,gaps FROM catalog_stats WHERE id=1`).Scan(&result.Sources, &result.CapturedBytes, &result.UploadedBytes, &result.BacklogBytes, &result.SpoolBytes, &result.Gaps)
	return result, err
}

func (c *Collector) setCollection(state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.CollectionState = state
	if err != nil {
		c.state.CollectionError = err.Error()
	} else if state == "ready" {
		c.state.CollectionError = ""
	}
}
func (c *Collector) setConnection(state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.ConnectionState = state
	if err != nil {
		c.state.ConnectionError = err.Error()
	} else if state == "connected" {
		c.state.ConnectionError = ""
	}
}
func (c *Collector) wake() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// Independent coalescing signals wake both upload workers without allowing one
// worker to consume the other's notification. Retry probes remain bounded.
func (c *Collector) wakeUploads() {
	for _, signal := range c.uploadNotify {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}

// Run blocks until cancellation or a fatal local ledger error. Network and
// storage availability problems are visible states, not reasons to lose data.
func (c *Collector) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	start := func(fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errs <- err:
				default:
				}
			}
		}()
	}
	start(c.heartbeatLoop)
	start(c.discoveryLoop)
	start(c.captureLoop)
	for _, signal := range c.uploadNotify {
		start(func(ctx context.Context) error { return c.uploadLoop(ctx, signal) })
	}
	start(c.recoveryLoop)
	start(c.reclaimLoop)
	select {
	case <-ctx.Done():
		cancel()
		wg.Wait()
		return ctx.Err()
	case err := <-errs:
		cancel()
		wg.Wait()
		return err
	}
}

func (c *Collector) captureLoop(ctx context.Context) error {
	// Reconciliation and every committed capture signal immediate work. This
	// fallback handles lost notifications without polling an idle ledger at 10 Hz.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-c.notify:
		}
		if err := c.CaptureOnce(ctx); err != nil {
			c.setCollection("blocked_storage", err)
			if !wait(ctx, 2*time.Second) {
				return ctx.Err()
			}
		}
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Collector) removeOrphanTemps() error {
	dir, err := os.Open(filepath.Join(c.cfg.DataDir, "chunks"))
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				if err = os.Remove(filepath.Join(c.cfg.DataDir, "chunks", e.Name())); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

const spoolSchema = `
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources(
 id TEXT PRIMARY KEY,identity TEXT NOT NULL UNIQUE,path TEXT NOT NULL,provider TEXT NOT NULL,native_id TEXT NOT NULL,
 current_generation TEXT NOT NULL,last_seen INTEGER NOT NULL,missing INTEGER NOT NULL DEFAULT 0,last_upload_attempt INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS generations(
 source_id TEXT NOT NULL,generation TEXT NOT NULL,generation_sequence INTEGER NOT NULL DEFAULT 1,path TEXT NOT NULL,size INTEGER NOT NULL,modified_ns INTEGER NOT NULL,
 captured INTEGER NOT NULL DEFAULT 0,uploaded INTEGER NOT NULL DEFAULT 0,last_capture INTEGER NOT NULL DEFAULT 0,
 verified_ns INTEGER NOT NULL DEFAULT 0,remote_checked INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(source_id,generation),FOREIGN KEY(source_id) REFERENCES sources(id));
CREATE TABLE IF NOT EXISTS chunks(
 source_id TEXT NOT NULL,generation TEXT NOT NULL,offset INTEGER NOT NULL,length INTEGER NOT NULL,sha256 TEXT NOT NULL,
 filename TEXT NOT NULL,payload_present INTEGER NOT NULL DEFAULT 1,ack_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,last_attempt INTEGER NOT NULL DEFAULT 0,
 lease_until INTEGER NOT NULL DEFAULT 0,receipt_id TEXT,PRIMARY KEY(source_id,generation,offset),
 FOREIGN KEY(source_id,generation) REFERENCES generations(source_id,generation));
CREATE INDEX IF NOT EXISTS pending_chunks ON chunks(ack_at,next_attempt,lease_until,last_attempt);
CREATE INDEX IF NOT EXISTS source_missing ON sources(last_seen,missing);
CREATE INDEX IF NOT EXISTS source_path_provider ON sources(path,provider);
CREATE INDEX IF NOT EXISTS generation_capture ON generations(last_capture,captured);
CREATE INDEX IF NOT EXISTS capture_pending ON generations(last_capture,source_id) WHERE captured<size;
CREATE TABLE IF NOT EXISTS gaps(id INTEGER PRIMARY KEY,source_id TEXT NOT NULL,generation TEXT NOT NULL,
 offset INTEGER NOT NULL,length INTEGER NOT NULL,reason TEXT NOT NULL,created_at INTEGER NOT NULL,
 UNIQUE(source_id,generation,offset,reason));
CREATE TABLE IF NOT EXISTS catalog_stats(id INTEGER PRIMARY KEY CHECK(id=1),sources INTEGER NOT NULL,captured INTEGER NOT NULL,uploaded INTEGER NOT NULL,backlog INTEGER NOT NULL,spool INTEGER NOT NULL,gaps INTEGER NOT NULL);
INSERT OR IGNORE INTO catalog_stats SELECT 1,(SELECT count(*) FROM sources),
 COALESCE((SELECT sum(captured) FROM generations),0),COALESCE((SELECT sum(uploaded) FROM generations),0),
 COALESCE((SELECT sum(length) FROM chunks WHERE ack_at IS NULL),0),
 COALESCE((SELECT sum(length) FROM chunks WHERE payload_present=1),0),(SELECT count(*) FROM gaps);
CREATE TRIGGER IF NOT EXISTS source_added AFTER INSERT ON sources BEGIN UPDATE catalog_stats SET sources=sources+1 WHERE id=1;END;
CREATE TRIGGER IF NOT EXISTS generation_added AFTER INSERT ON generations BEGIN UPDATE catalog_stats SET captured=captured+NEW.captured,uploaded=uploaded+NEW.uploaded WHERE id=1;END;
CREATE TRIGGER IF NOT EXISTS generation_progress AFTER UPDATE OF captured,uploaded ON generations BEGIN UPDATE catalog_stats SET captured=captured+NEW.captured-OLD.captured,uploaded=uploaded+NEW.uploaded-OLD.uploaded WHERE id=1;END;
CREATE TRIGGER IF NOT EXISTS chunk_added AFTER INSERT ON chunks BEGIN UPDATE catalog_stats SET backlog=backlog+CASE WHEN NEW.ack_at IS NULL THEN NEW.length ELSE 0 END,spool=spool+CASE WHEN NEW.payload_present=1 THEN NEW.length ELSE 0 END WHERE id=1;END;
CREATE TRIGGER IF NOT EXISTS chunk_progress AFTER UPDATE OF ack_at,payload_present ON chunks BEGIN UPDATE catalog_stats SET backlog=backlog+(CASE WHEN NEW.ack_at IS NULL THEN NEW.length ELSE 0 END)-(CASE WHEN OLD.ack_at IS NULL THEN OLD.length ELSE 0 END),spool=spool+(CASE WHEN NEW.payload_present=1 THEN NEW.length ELSE 0 END)-(CASE WHEN OLD.payload_present=1 THEN OLD.length ELSE 0 END) WHERE id=1;END;
CREATE TRIGGER IF NOT EXISTS gap_added AFTER INSERT ON gaps BEGIN UPDATE catalog_stats SET gaps=gaps+1 WHERE id=1;END;
`
