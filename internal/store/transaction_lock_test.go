package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestWriteTransactionReservesWriterBeforeReadingAcrossStoreHandles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	conn, err := other.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=50"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM properties").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO properties VALUES('concurrent-test','external')"); err == nil {
		t.Fatal("external writer committed after a write transaction took its read snapshot")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO properties VALUES('owner-test','committed')"); err != nil {
		t.Fatal("write transaction failed its read-to-write upgrade", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO properties VALUES('concurrent-test','external')"); err != nil {
		t.Fatal("writer did not resume after commit", err)
	}
	// Explicit read snapshots must still coexist with a writer in WAL mode.
	read, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	var before, after string
	if err = read.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='concurrent-test'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "UPDATE properties SET value='new' WHERE key='concurrent-test'"); err != nil {
		t.Fatal("read snapshot blocked writer", err)
	}
	if err = read.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='concurrent-test'").Scan(&after); err != nil || before != after {
		t.Fatal("read snapshot was not stable", before, after, err)
	}
}
