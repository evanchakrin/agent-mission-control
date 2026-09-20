package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerBackupPreCancelledVerification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyStateBackup(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled verification touched filesystem", err)
	}
}

func TestOwnerBackupIncludesCommittedWALWithoutReconciliation(t *testing.T) {
	f := newFixture(t)
	if err := f.m.save("playbooks", []map[string]any{{"id": "kept"}}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.db.Exec("INSERT INTO snapshots(stamp,path,content,hash,mtime,created_at)VALUES(?,?,?,?,?,?)", "before", f.file, []byte("original"), hash([]byte("original")), 0, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.db.Exec("INSERT INTO file_operations(id,path,before_hash,after_hash,status,at)VALUES(?,?,?,?,?,?)", "pending", f.file, hash([]byte("before")), hash([]byte("after")), "pending", now()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(f.opts.StateDir, "owner.db-wal")); err != nil || st.Size() == 0 {
		t.Fatalf("test needs live WAL: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "owner-snapshot")
	manifest, err := SnapshotState(context.Background(), f.opts.StateDir, dest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Rows["local_state"] != 1 || manifest.Rows["snapshots"] != 1 || manifest.Rows["file_operations"] != 1 {
		t.Fatalf("missing state: %+v", manifest)
	}
	if _, err = VerifyStateBackup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err = RestoreStateBackup(context.Background(), dest, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = RestoreStateBackup(context.Background(), dest, restored); err == nil {
		t.Fatal("restore replaced an existing destination")
	}
	restoredDB, err := readOnlyState(filepath.Join(restored, "owner.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	var restoredStatus string
	err = restoredDB.QueryRow("SELECT status FROM file_operations WHERE id='pending'").Scan(&restoredStatus)
	restoredDB.Close()
	if err != nil || restoredStatus != "pending" {
		t.Fatalf("restore reconciled pending work: %q %v", restoredStatus, err)
	}
	var status string
	if err = f.m.db.QueryRow("SELECT status FROM file_operations WHERE id='pending'").Scan(&status); err != nil || status != "pending" {
		t.Fatalf("backup reconciled live state: %q %v", status, err)
	}
	if _, err = SnapshotState(context.Background(), f.opts.StateDir, dest); err == nil {
		t.Fatal("overwrote backup")
	}
	if _, err = SnapshotState(context.Background(), f.opts.StateDir, filepath.Join(f.opts.StateDir, "nested")); err == nil {
		t.Fatal("accepted backup inside source")
	}
	if err = f.m.save("playbooks", []map[string]any{{"id": "later"}, {"id": "other"}}, "test"); err != nil {
		t.Fatal(err)
	}
	db, err := readOnlyState(filepath.Join(dest, "owner.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var saved string
	if err = db.QueryRow("SELECT value FROM local_state WHERE key='playbooks'").Scan(&saved); err != nil || saved != `[{"id":"kept"}]` {
		t.Fatalf("snapshot changed with live DB: %s %v", saved, err)
	}
}

func TestOwnerBackupDoesNotInitializeMissingStateAndRejectsCorruption(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "missing")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotState(context.Background(), source, filepath.Join(base, "backup")); err == nil {
		t.Fatal("accepted missing database")
	}
	if _, err := os.Stat(filepath.Join(source, "owner.db")); !os.IsNotExist(err) {
		t.Fatal("initialized source database")
	}
	f := newFixture(t)
	dest := filepath.Join(base, "backup")
	if _, err := f.m.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dest, "owner.db"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write([]byte("damage"))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyStateBackup(context.Background(), dest); err == nil {
		t.Fatal("accepted checksum corruption")
	}
}
