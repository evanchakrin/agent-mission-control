package desktop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StateBackupManifest describes the complete owner database, including Brain
// snapshots, standing orders, playbooks, triage and the local mutation journal.
// Role configuration is deliberately not inferred from this directory: the
// caller must explicitly snapshot the configuration files it selected.
type StateBackupManifest struct {
	Version        int              `json:"version"`
	CreatedAt      time.Time        `json:"createdAt"`
	DatabaseSHA256 string           `json:"databaseSHA256"`
	Rows           map[string]int64 `json:"rows"`
}

var stateTables = []string{"local_state", "audit", "snapshots", "file_operations", "legacy_imports"}

func (m *Manager) Backup(ctx context.Context, destination string) (StateBackupManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return SnapshotState(ctx, m.opts.StateDir, destination)
}

// SnapshotState reads an existing owner database without initializing schemas,
// reconciling pending writes or touching repositories. VACUUM INTO captures
// committed WAL state consistently; copying owner.db alone would lose it.
// Destination must be new and outside the source. A manifest is published last,
// so interrupted copies remain visibly incomplete and cannot be restored.
func SnapshotState(ctx context.Context, stateDir, destination string) (StateBackupManifest, error) {
	var manifest StateBackupManifest
	if err := backupPath(stateDir); err != nil {
		return manifest, err
	}
	source := filepath.Join(stateDir, "owner.db")
	if err := noLinks(source); err != nil {
		return manifest, err
	}
	st, err := os.Stat(source)
	if err != nil {
		return manifest, err
	}
	if !st.Mode().IsRegular() {
		return manifest, errors.New("owner database must be a regular file")
	}
	db, err := readOnlyState(source, false)
	if err != nil {
		return manifest, err
	}
	defer db.Close()
	// Validate before creating any destination artifacts.
	if _, err = stateCounts(ctx, db); err != nil {
		return manifest, err
	}
	dest, err := newBackupDirectory(stateDir, destination)
	if err != nil {
		return manifest, err
	}
	output := filepath.Join(dest, "owner.db")
	if _, err = db.ExecContext(ctx, "VACUUM INTO ?", output); err != nil {
		return manifest, err
	}
	f, err := os.OpenFile(output, os.O_RDWR, 0600)
	if err != nil {
		return manifest, err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return manifest, err
	}
	if closeErr != nil {
		return manifest, closeErr
	}
	snapshot, err := readOnlyState(output, true)
	if err != nil {
		return manifest, err
	}
	defer snapshot.Close()
	if err = checkState(ctx, snapshot); err != nil {
		return manifest, err
	}
	manifest.Rows, err = stateCounts(ctx, snapshot)
	if err != nil {
		return manifest, err
	}
	manifest.DatabaseSHA256, err = stateFileHash(ctx, output)
	if err != nil {
		return manifest, err
	}
	manifest.Version = 1
	manifest.CreatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err = writeNewDurable(filepath.Join(dest, "owner-manifest.json"), b); err != nil {
		return manifest, err
	}
	return manifest, nil
}

// VerifyStateBackup verifies the manifest, complete database integrity and each
// Brain snapshot digest. It does not open or mutate any referenced local paths.
func VerifyStateBackup(ctx context.Context, directory string) (StateBackupManifest, error) {
	var manifest StateBackupManifest
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := backupPath(directory); err != nil {
		return manifest, err
	}
	path := filepath.Join(directory, "owner-manifest.json")
	if err := noLinks(path); err != nil {
		return manifest, err
	}
	f, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	f.Close()
	if err != nil {
		return manifest, err
	}
	if len(b) > 1024*1024 || json.Unmarshal(b, &manifest) != nil || manifest.Version != 1 || len(manifest.Rows) != len(stateTables) {
		return manifest, errors.New("invalid owner backup manifest")
	}
	dbPath := filepath.Join(directory, "owner.db")
	if err = noLinks(dbPath); err != nil {
		return manifest, err
	}
	digest, err := stateFileHash(ctx, dbPath)
	if err != nil {
		return manifest, err
	}
	if digest != manifest.DatabaseSHA256 {
		return manifest, errors.New("owner database checksum mismatch")
	}
	db, err := readOnlyState(dbPath, true)
	if err != nil {
		return manifest, err
	}
	defer db.Close()
	if err = checkState(ctx, db); err != nil {
		return manifest, err
	}
	counts, err := stateCounts(ctx, db)
	if err != nil {
		return manifest, err
	}
	for table, count := range counts {
		if got, ok := manifest.Rows[table]; !ok || got != count {
			return manifest, fmt.Errorf("owner backup inventory mismatch: %s", table)
		}
	}
	return manifest, nil
}

// RestoreStateBackup publishes only the verified owner database into a new
// directory. It deliberately does not run recovery, write guidance files,
// register a process or grant imported paths any authority.
func RestoreStateBackup(ctx context.Context, backupDir, newStateDir string) (StateBackupManifest, error) {
	manifest, err := VerifyStateBackup(ctx, backupDir)
	if err != nil {
		return manifest, err
	}
	dest, err := newBackupDirectory(backupDir, newStateDir)
	if err != nil {
		return manifest, err
	}
	in, err := os.Open(filepath.Join(backupDir, "owner.db"))
	if err != nil {
		return manifest, err
	}
	defer in.Close()
	// The incomplete filename is never treated as a live owner database.
	partial := filepath.Join(dest, "owner.db.incomplete")
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return manifest, err
	}
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = in.Read(buf)
		if n > 0 {
			if _, e := out.Write(buf[:n]); e != nil {
				err = e
				break
			}
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return manifest, err
	}
	if closeErr != nil {
		return manifest, closeErr
	}
	if hex.EncodeToString(h.Sum(nil)) != manifest.DatabaseSHA256 {
		return manifest, errors.New("owner backup changed while restoring")
	}
	if err = os.Link(partial, filepath.Join(dest, "owner.db")); err != nil {
		return manifest, err
	}
	// This removes only the private staging link, never source/backup/user data.
	if err = os.Remove(partial); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func backupPath(path string) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\x00\r\n") || filepath.Dir(filepath.Clean(path)) == filepath.Clean(path) {
		return errors.New("owner backup paths must be explicit local directories, not filesystem roots")
	}
	return noLinks(path)
}
func newBackupDirectory(source, destination string) (string, error) {
	if err := backupPath(destination); err != nil {
		return "", err
	}
	dest := filepath.Clean(destination)
	if under(source, dest) || under(dest, source) {
		return "", errors.New("owner backup destination must be outside source directory")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	if err := noLinks(filepath.Dir(dest)); err != nil {
		return "", err
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return "", err
	}
	return dest, nil
}
func readOnlyState(path string, immutable bool) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // Windows drive is a path, never a URI authority.
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}
	if immutable {
		q.Set("immutable", "1")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func stateCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, table := range stateTables {
		var count int64
		// Identifiers come exclusively from the compile-time list above.
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return nil, err
		}
		counts[table] = count
	}
	return counts, nil
}
func checkState(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("owner database integrity check failed: %s", result)
	}
	rows, err := db.QueryContext(ctx, "SELECT content,hash FROM snapshots")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var content []byte
		var expected string
		if err = rows.Scan(&content, &expected); err != nil {
			return err
		}
		if hash(content) != expected {
			return errors.New("owner snapshot digest mismatch")
		}
	}
	return rows.Err()
}
func stateFileHash(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, &backupContextReader{ctx: ctx, input: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type backupContextReader struct {
	ctx   context.Context
	input io.Reader
}

func (r *backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}
func writeNewDurable(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
