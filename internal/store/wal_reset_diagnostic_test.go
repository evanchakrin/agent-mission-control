package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in reduced-size experiment: only this fixture writer uses a 1 MiB limit;
// the application's pooled connections retain their 64 MiB reset policy.
func TestWALResetSizeLimitDiagnostic(t *testing.T) {
	if testing.Short() || os.Getenv("AMC_WAL_RESET_DIAGNOSTIC") != "1" {
		t.Skip("opt-in WAL reset reclamation experiment")
	}
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	writer, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.ExecContext(ctx, `PRAGMA journal_size_limit=1048576;
	 CREATE TABLE reset_fixture(id INTEGER PRIMARY KEY,data BLOB);
	 INSERT INTO reset_fixture(data) VALUES(zeroblob(12582912))`); err != nil {
		t.Fatal(err)
	}
	reader, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err = reader.QueryRowContext(ctx, `SELECT count(*) FROM reset_fixture`).Scan(&count); err != nil || count != 1 {
		t.Fatal("initial snapshot", count, err)
	}
	checkpoint := func() {
		t.Helper()
		var busy, frames, copied int
		if e := writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &frames, &copied); e != nil {
			t.Fatal(e)
		}
		t.Logf("passive busy=%d frames=%d copied=%d", busy, frames, copied)
	}
	if _, err = writer.ExecContext(ctx, `INSERT INTO reset_fixture(data) VALUES(x'02')`); err != nil {
		t.Fatal(err)
	}
	checkpoint()
	path := filepath.Join(s.dir, "ledger.sqlite-wal")
	before, err := os.Stat(path)
	if err != nil || before.Size() <= 1<<20 {
		t.Fatal("active WAL must exceed the reset retention limit", before, err)
	}
	if err = reader.QueryRowContext(ctx, `SELECT count(*) FROM reset_fixture`).Scan(&count); err != nil || count != 1 {
		t.Fatal("reader snapshot changed", count, err)
	}
	if err = reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	checkpoint()
	started := time.Now()
	if _, err = writer.ExecContext(ctx, `INSERT INTO reset_fixture(data) VALUES(x'03')`); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() > 1<<20 || after.Size() == 0 {
		t.Fatal("normal writer reset did not bound retained WAL", after, err)
	}
	t.Logf("WAL before=%d after=%d reset-write=%s; no forced TRUNCATE used", before.Size(), after.Size(), time.Since(started))
	var total, full int
	if err = writer.QueryRowContext(ctx, `SELECT count(*),sum(length(data)) FROM reset_fixture`).Scan(&count, &total); err != nil || count != 3 || total != (12<<20)+2 {
		t.Fatal("accepted rows changed", count, total, err)
	}
	if err = writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
		t.Fatal("durability changed", full, err)
	}
	var integrity string
	if err = writer.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}
