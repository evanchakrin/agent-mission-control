package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Exercise reads on both sides of the bounded mapping, including WAL overlays.
// This checks data correctness, not resident memory or physical I/O failures.
func TestMappedReadBeyondPrefixPreservesSnapshotAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	ctx := context.Background()
	if _, err = s.db.Exec(`CREATE TABLE mapped_fixture(id INTEGER PRIMARY KEY, data BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 12; id++ {
		if _, err = s.db.Exec(`INSERT INTO mapped_fixture VALUES(?,?)`, id, bytes.Repeat([]byte{byte(id)}, 1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	s.checkpointOnce(ctx)
	info, err := os.Stat(filepath.Join(dir, "ledger.sqlite"))
	if err != nil || info.Size() <= 8<<20 {
		t.Fatal("fixture must exceed mapped prefix", info, err)
	}
	reader, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	check := func(query func(string, ...any) *sql.Row, updated bool) {
		t.Helper()
		for id := 1; id <= 12; id++ {
			var got []byte
			if e := query(`SELECT data FROM mapped_fixture WHERE id=?`, id).Scan(&got); e != nil {
				t.Fatal(e)
			}
			value := byte(id)
			if updated && (id == 1 || id == 12) {
				value = 99
			}
			if !bytes.Equal(got, bytes.Repeat([]byte{value}, 1<<20)) {
				t.Fatalf("row %d changed or was truncated (updated=%v)", id, updated)
			}
		}
	}
	check(reader.QueryRow, false)
	if _, err = s.db.Exec(`UPDATE mapped_fixture SET data=? WHERE id IN (1,12)`, bytes.Repeat([]byte{99}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	check(s.db.QueryRow, true)
	s.checkpointOnce(ctx)
	check(reader.QueryRow, false)
	if err = reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	check(s.db.QueryRow, true)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	check(s.db.QueryRow, true)
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}
