// Package desktop implements owner-only local file operations. It is hosted by
// the unelevated desktop process, never by the satellite ingestion service.
package desktop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const MaxBrainBytes = 512 * 1024

type Measurement struct {
	Body     string
	Sessions int
	Subs     int
}
type Options struct {
	LocalMachineID     string
	StateDir           string
	HomeDir            string
	Roots              []string
	ClaudeProjectsDir  string
	CSRFToken          string
	Remeasure          func(context.Context) (Measurement, error)
	RemeasureDirective func(context.Context, string) (Measurement, error)
}
type Manager struct {
	opts      Options
	db        *sql.DB
	mu        sync.Mutex
	unsavedMu sync.Mutex
	secretsMu sync.Mutex
	handler   http.Handler
}
type apiError struct {
	code    int
	message string
}

func (e *apiError) Error() string         { return e.message }
func fail(code int, message string) error { return &apiError{code, message} }

func New(opts Options) (*Manager, error) {
	if len(opts.LocalMachineID) > 1024 || clean(opts.LocalMachineID, 1024) != opts.LocalMachineID || strings.TrimSpace(opts.LocalMachineID) != opts.LocalMachineID {
		return nil, errors.New("invalid explicit local machine identity")
	}
	for name, p := range map[string]string{"owner home": opts.HomeDir, "owner state": opts.StateDir} {
		if !filepath.IsAbs(p) || strings.HasPrefix(p, `\\`) || strings.ContainsAny(p, "\x00\r\n") {
			return nil, fmt.Errorf("%s must be an explicit absolute local directory", name)
		}
	}
	if opts.CSRFToken == "" {
		return nil, errors.New("desktop CSRF token is required")
	}
	if opts.ClaudeProjectsDir != "" {
		if !under(opts.HomeDir, opts.ClaudeProjectsDir) {
			return nil, errors.New("local Claude discovery must stay under the owner's profile")
		}
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(opts.StateDir, "owner.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	m := &Manager{opts: opts, db: db}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS local_state (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY,at INTEGER NOT NULL,kind TEXT NOT NULL,path TEXT NOT NULL DEFAULT '',status TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS snapshots (stamp TEXT PRIMARY KEY,path TEXT NOT NULL,content BLOB NOT NULL,hash TEXT NOT NULL,mtime REAL NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS snapshots_path ON snapshots(path,created_at DESC);
CREATE INDEX IF NOT EXISTS snapshots_history ON snapshots(path,created_at DESC,stamp DESC);
CREATE TABLE IF NOT EXISTS file_operations (id TEXT PRIMARY KEY,path TEXT NOT NULL,before_hash TEXT NOT NULL,after_hash TEXT NOT NULL,status TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS legacy_imports (source TEXT PRIMARY KEY,digest TEXT NOT NULL,imported_at INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, root := range opts.Roots {
		if _, err = m.validateRoot(root); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = m.reconcileWrites(); err != nil {
		db.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	m.Register(mux)
	m.handler = mux
	return m, nil
}
func (m *Manager) Close() error                                     { return m.db.Close() }
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.handler.ServeHTTP(w, r) }
func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v2/local/secrets", m.handleSecrets)
	mux.HandleFunc("/api/v2/local/unsaved", m.handleUnsaved)
	mux.HandleFunc("/api/v2/local/identity", m.handleLocalIdentity)
	for _, p := range []string{"/api/brain", "/api/brain/file", "/api/brain/history", "/api/brain/snapshot", "/api/directives", "/api/playbooks", "/api/triage", "/api/audit"} {
		mux.HandleFunc(p, m.handle)
	}
}
func now() int64 { return time.Now().UnixMilli() }
func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func norm(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
func under(root, p string) bool {
	rel, err := filepath.Rel(norm(root), norm(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func clean(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	r := []rune(b.String())
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}
func (m *Manager) load(key string, target any) error {
	var raw string
	err := m.db.QueryRow("SELECT value FROM local_state WHERE key=?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("owner state %s is corrupt: %w", key, err)
	}
	return nil
}
func (m *Manager) save(key string, value any, kind string) error {
	return m.saveOperation(key, value, kind, "", nil)
}

type localOperationReceipt struct {
	RequestHash string         `json:"requestHash"`
	Result      map[string]any `json:"result"`
}

// Receipts share the backed-up local-state ledger, but use a separate key space.
// State, receipt and audit are committed together; no response is published first.
func (m *Manager) saveOperation(key string, value any, kind, receiptKey string, receipt *localOperationReceipt) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if receipt != nil {
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO local_state(key,value) VALUES(?,?)", receiptKey, string(encoded)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO local_state(key,value)VALUES(?,?) ON CONFLICT(key)DO UPDATE SET value=excluded.value", key, string(raw)); err != nil {
		return err
	}
	detail := map[string]any{"stateKey": key}
	if receipt != nil {
		detail["operationId"] = receipt.Result["operationId"]
		detail["recordId"] = receipt.Result["id"]
		detail["requestHash"] = receipt.RequestHash
		if item, ok := receipt.Result["item"].(Playbook); ok {
			detail["revision"] = item.Revision
		}
	}
	auditJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO audit(at,kind,status,detail)VALUES(?,?,?,?)", now(), kind, "applied", string(auditJSON)); err != nil {
		return err
	}
	return tx.Commit()
}
func (m *Manager) audit(kind, path, status string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = m.db.Exec("INSERT INTO audit(at,kind,path,status,detail)VALUES(?,?,?,?,?)", now(), kind, path, status, string(b))
	return err
}
func (m *Manager) reconcileWrites() error {
	rows, err := m.db.Query("SELECT id,path,before_hash,after_hash FROM file_operations WHERE status='pending'")
	if err != nil {
		return err
	}
	type pending struct{ id, path, before, after string }
	var records []pending
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.id, &p.path, &p.before, &p.after); err != nil {
			rows.Close()
			return err
		}
		records = append(records, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range records {
		status := "needs-review"
		if m.validateFile(p.path) != nil {
			continue
		}
		if b, e := readBounded(p.path); e == nil {
			switch hash(b) {
			case p.after:
				status = "applied"
			case p.before:
				status = "not-applied"
			}
		} else if os.IsNotExist(e) && p.before == hash(nil) {
			status = "not-applied"
		}
		if _, err = m.db.Exec("UPDATE file_operations SET status=? WHERE id=?", status, p.id); err != nil {
			return err
		}
		if err = m.audit("file-write-recovery", p.path, status, map[string]string{"operationId": p.id}); err != nil {
			return err
		}
	}
	return nil
}

func readBounded(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBrainBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBrainBytes {
		return nil, errors.New("file is larger than the editor limit")
	}
	return b, nil
}

// writeFile records intent and the previous bytes durably before touching a
// user's file. Recovery reconciles a crash between replacement and completion.
func (m *Manager) writeFile(item Item, expected string, next []byte, kind string) error {
	if len(next) > MaxBrainBytes {
		return fail(400, "content is too large")
	}
	if err := m.validateFile(item.Path); err != nil {
		return err
	}
	before, err := readBounded(item.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if expected == "" || hash(before) != expected {
		return fail(409, "file changed on disk — reload before saving")
	}
	mtime := float64(0)
	mode := os.FileMode(0600)
	if st, e := os.Stat(item.Path); e == nil {
		mtime = float64(st.ModTime().UnixNano()) / 1e6
		mode = st.Mode().Perm()
	}
	id := randomID("write_")
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if exists {
		if _, err = tx.Exec("INSERT INTO snapshots(stamp,path,content,hash,mtime,created_at)VALUES(?,?,?,?,?,?)", stamp, item.Path, before, hash(before), mtime, now()); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO file_operations(id,path,before_hash,after_hash,status,at)VALUES(?,?,?,?,?,?)", id, item.Path, hash(before), hash(next), "pending", now()); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO audit(at,kind,path,status,detail)VALUES(?,?,?,?,?)", now(), kind, item.Path, "pending", `{"operationId":"`+id+`"}`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = m.validateFile(item.Path); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(item.Path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(item.Path), ".amc-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(next); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	current, e := readBounded(item.Path)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if hash(current) != expected {
		return fail(409, "file changed on disk — reload before saving")
	}
	if err = m.validateFile(item.Path); err != nil {
		return err
	}
	if err = os.Rename(tmp, item.Path); err != nil {
		return err
	}
	if _, err = m.db.Exec("UPDATE file_operations SET status='applied' WHERE id=?", id); err != nil {
		return err
	}
	return m.audit(kind, item.Path, "applied", map[string]any{"operationId": id, "bytes": len(next), "expectedHash": hash(next)})
}
