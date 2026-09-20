package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Backup creates a new, complete snapshot directory. The manifest is published
// last; its absence means an interrupted backup must not be restored. No source
// files are removed. Immutable raw blobs permit this copy to proceed while the
// live hub continues to append and process new history.
func (s *Store) Backup(ctx context.Context, destination string) (BackupManifest, error) {
	var manifest BackupManifest
	dest, err := newDestination(destination, s.dir)
	if err != nil {
		return manifest, err
	}
	dbPath := filepath.Join(dest, "ledger.sqlite")
	// SQLite produces a transactionally consistent snapshot even while WAL
	// writers continue. A plain filesystem copy of an open DB would not.
	if _, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, dbPath); err != nil {
		return manifest, err
	}
	if err = syncFile(dbPath); err != nil {
		return manifest, err
	}
	snapshot, err := openSnapshot(dbPath)
	if err != nil {
		return manifest, err
	}
	defer snapshot.Close()
	if err = integrityCheck(ctx, snapshot); err != nil {
		return manifest, err
	}
	if err = checkProjectionIntegrity(ctx, snapshot); err != nil {
		return manifest, err
	}
	manifest.Version = 2
	manifest.CreatedAt = time.Now().UTC()
	manifest.RecoveryEpoch = s.epoch
	manifest.BlobCount, manifest.RawBytes, err = copyBlobs(ctx, snapshot, s.dir, dest)
	if err != nil {
		return manifest, err
	}
	manifest.DatabaseSHA256, err = fileHash(ctx, dbPath)
	if err != nil {
		return manifest, err
	}
	manifest.LegacyAssetsSHA256, err = backupLegacyAssets(ctx, s.dir, dest)
	if err != nil {
		return manifest, err
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err = writeDurable(filepath.Join(dest, "manifest.json"), b); err != nil {
		return manifest, err
	}
	if err = syncDir(dest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

// VerifyBackup verifies the ledger, its manifest digest, every referenced blob,
// and that acknowledged ranges are contiguous and fit declared source offsets.
func VerifyBackup(ctx context.Context, dir string) (BackupManifest, error) {
	var m BackupManifest
	if err := ctx.Err(); err != nil {
		return m, err
	}
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	m, err = readBackupManifest(ctx, f)
	closeErr := f.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	h, err := fileHash(ctx, filepath.Join(dir, "ledger.sqlite"))
	if err != nil {
		return m, err
	}
	if h != m.DatabaseSHA256 {
		return m, fmt.Errorf("backup ledger checksum mismatch")
	}
	if m.LegacyAssetsSHA256 != "" {
		hash, e := fileHash(ctx, filepath.Join(dir, "legacy-assets.tar"))
		if e != nil {
			return m, e
		}
		if hash != m.LegacyAssetsSHA256 {
			return m, fmt.Errorf("backup legacy assets checksum mismatch")
		}
		if e = readLegacyAssets(ctx, dir, ""); e != nil {
			return m, e
		}
	}
	db, err := openSnapshot(filepath.Join(dir, "ledger.sqlite"))
	if err != nil {
		return m, err
	}
	defer db.Close()
	if err = integrityCheck(ctx, db); err != nil {
		return m, err
	}
	if err = checkProjectionIntegrity(ctx, db); err != nil {
		return m, err
	}
	var epoch string
	if err = db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key='recovery_epoch'`).Scan(&epoch); err != nil {
		return m, err
	}
	if epoch != m.RecoveryEpoch {
		return m, fmt.Errorf("backup recovery epoch mismatch")
	}
	count, total, err := checkBlobs(ctx, db, dir)
	if err != nil {
		return m, err
	}
	if count != m.BlobCount || total != m.RawBytes {
		return m, fmt.Errorf("backup manifest inventory mismatch")
	}
	var invalid int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources s WHERE indexed_offset>durable_offset OR durable_offset<>COALESCE((SELECT SUM(length) FROM chunks c WHERE c.source_id=s.source_id AND c.generation=s.generation),0)`).Scan(&invalid)
	if err != nil {
		return m, err
	}
	if invalid > 0 {
		return m, fmt.Errorf("backup contains invalid source offsets")
	}
	// Compare each chunk to its predecessor instead of trusting sum(length),
	// which would overlook an equal-size gap and overlapping range.
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT offset,COALESCE(LAG(offset+length) OVER(PARTITION BY source_id,generation ORDER BY offset),0) expected FROM chunks) WHERE offset<>expected`).Scan(&invalid)
	if err != nil {
		return m, err
	}
	if invalid > 0 {
		return m, fmt.Errorf("backup contains noncontiguous raw ranges")
	}
	return m, nil
}

// RestoreBackup only restores into a new directory and rotates the persistent
// recovery epoch, prompting satellites to reconcile the restored inventory.
func RestoreBackup(ctx context.Context, backup, destination string, options Options) (*Store, error) {
	manifest, err := VerifyBackup(ctx, backup)
	if err != nil {
		return nil, err
	}
	dest, err := newDestination(destination, backup)
	if err != nil {
		return nil, err
	}
	if manifest.LegacyAssetsSHA256 != "" {
		if err = readLegacyAssets(ctx, backup, dest); err != nil {
			return nil, err
		}
	}
	if err = copyDurable(ctx, filepath.Join(backup, "ledger.sqlite"), filepath.Join(dest, "ledger.sqlite")); err != nil {
		return nil, err
	}
	db, err := openSnapshot(filepath.Join(backup, "ledger.sqlite"))
	if err != nil {
		return nil, err
	}
	_, _, err = copyBlobs(ctx, db, backup, dest)
	db.Close()
	if err != nil {
		return nil, err
	}
	s, err := Open(dest, options)
	if err != nil {
		return nil, err
	}
	epoch := randomID()
	if _, err = s.db.ExecContext(ctx, `UPDATE properties SET value=? WHERE key='recovery_epoch'`, epoch); err != nil {
		s.Close()
		return nil, err
	}
	s.epoch = epoch
	return s, nil
}

func newDestination(destination, source string) (string, error) {
	dest, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	src, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(src, dest)
	if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
		return "", fmt.Errorf("backup/restore destination must be outside source directory")
	}
	if _, err = os.Stat(dest); err == nil {
		return "", fmt.Errorf("destination already exists")
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	if err = os.Mkdir(dest, 0700); err != nil {
		return "", err
	}
	return dest, nil
}

func openSnapshot(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.ToSlash(path)+"?mode=ro&immutable=1&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check: %s", result)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("SQLite foreign key check failed")
	}
	return rows.Err()
}
func blobName(dir, hash string) (string, error) {
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 32 || strings.ToLower(hash) != hash {
		return "", fmt.Errorf("invalid blob digest in ledger")
	}
	return filepath.Join(dir, "blobs", hash[:2], hash), nil
}
func copyBlobs(ctx context.Context, db *sql.DB, source, dest string) (int64, int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT sha256,MIN(length),MAX(length) FROM chunks GROUP BY sha256 ORDER BY sha256`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var count, total int64
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return count, total, err
		}
		var hash string
		var min, max int64
		if err = rows.Scan(&hash, &min, &max); err != nil {
			return count, total, err
		}
		if min != max {
			return count, total, fmt.Errorf("digest has conflicting sizes")
		}
		from, err := blobName(source, hash)
		if err != nil {
			return count, total, err
		}
		to, err := blobName(dest, hash)
		if err != nil {
			return count, total, err
		}
		if err = verifyFile(from, hash, max); err != nil {
			return count, total, err
		}
		if err = copyDurable(ctx, from, to); err != nil {
			return count, total, err
		}
		count++
		total += max
	}
	return count, total, rows.Err()
}
func checkBlobs(ctx context.Context, db *sql.DB, dir string) (int64, int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT sha256,MIN(length),MAX(length) FROM chunks GROUP BY sha256`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var count, total int64
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return count, total, err
		}
		var hash string
		var min, max int64
		if err = rows.Scan(&hash, &min, &max); err != nil {
			return count, total, err
		}
		if min != max {
			return count, total, fmt.Errorf("digest size mismatch")
		}
		name, err := blobName(dir, hash)
		if err != nil {
			return count, total, err
		}
		if err = verifyFile(name, hash, max); err != nil {
			return count, total, err
		}
		count++
		total += max
	}
	return count, total, rows.Err()
}
func fileHash(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, &contextReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readBackupManifest(ctx context.Context, input io.Reader) (BackupManifest, error) {
	var m BackupManifest
	// Check the ceiling while reading, not after allocating an arbitrary file.
	const maximum = 1 << 20
	b, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: input}, maximum+1))
	if err != nil {
		return m, err
	}
	if len(b) > maximum || json.Unmarshal(b, &m) != nil || (m.Version != 1 && m.Version != 2) || m.Version == 2 && len(m.LegacyAssetsSHA256) != 64 {
		return m, fmt.Errorf("invalid backup manifest")
	}
	return m, nil
}
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func copyDurable(ctx context.Context, source, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, &contextReader{ctx: ctx, r: in})
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncDir(filepath.Dir(dest))
}
