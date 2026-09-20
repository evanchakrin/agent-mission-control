package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWALReclaimPreservesDataAndConnectionPolicy(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TABLE reclaim_fixture(data BLOB); INSERT INTO reclaim_fixture VALUES(zeroblob(5242880))`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	if ok, err := s.reclaimWAL(ctx); err != nil || !ok {
		t.Fatal("idle WAL was not reclaimed", ok, err)
	}
	info, err := os.Stat(filepath.Join(s.dir, "ledger.sqlite-wal"))
	if err != nil || info.Size() != 0 {
		t.Fatal("WAL remains allocated", info, err)
	}
	var size int
	if err := s.db.QueryRow(`SELECT length(data) FROM reclaim_fixture`).Scan(&size); err != nil || size != 5242880 {
		t.Fatal("committed data changed", size, err)
	}
	checkReclaimConnectionPolicy(t, s)
}

func TestWALReclaimDoesNotWaitForTransactions(t *testing.T) {
	for _, writer := range []bool{false, true} {
		t.Run(map[bool]string{false: "reader", true: "writer"}[writer], func(t *testing.T) {
			s := openTestStore(t, Options{ExternalCheckpointOwner: true})
			ctx := context.Background()
			if _, err := s.db.Exec(`CREATE TABLE reclaim_fixture(value INTEGER); INSERT INTO reclaim_fixture VALUES(1)`); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writer})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var value int
			if err := tx.QueryRow(`SELECT value FROM reclaim_fixture`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			ok, err := s.reclaimWAL(ctx)
			if err != nil || ok || time.Since(started) >= 250*time.Millisecond {
				t.Fatal("busy reclaim did not back off", ok, err, time.Since(started))
			}
			if err := tx.QueryRow(`SELECT value FROM reclaim_fixture`).Scan(&value); err != nil || value != 1 {
				t.Fatal("transaction snapshot changed", value, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.reclaimWAL(ctx); err != nil || !ok {
				t.Fatal("reclaim did not recover after transaction ended", ok, err)
			}
			checkReclaimConnectionPolicy(t, s)
		})
	}
}

func checkReclaimConnectionPolicy(t *testing.T, s *Store) {
	t.Helper()
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		var timeout, full int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
			t.Fatal("busy policy leaked", timeout, err)
		}
		if err := conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
			t.Fatal("durability changed", full, err)
		}
	}
}

func TestCheckpointLeavesLargeWALForSafeWriterReset(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	if _, err := s.db.Exec(`CREATE TABLE reclaim_large(data BLOB); INSERT INTO reclaim_large VALUES(zeroblob(68157440))`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "ledger.sqlite-wal")
	before, err := os.Stat(path)
	if err != nil || before.Size() < 64<<20 {
		t.Fatal("fixture did not reach reclamation threshold", before, err)
	}
	reader, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err := reader.QueryRow(`SELECT count(*) FROM reclaim_large`).Scan(&count); err != nil || count != 1 {
		t.Fatal("initial reader snapshot", count, err)
	}
	if _, err := s.db.Exec(`INSERT INTO reclaim_large VALUES(x'00')`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(context.Background())
	pinned, err := os.Stat(path)
	if err != nil || pinned.Size() < 64<<20 || s.CheckpointStatus().State != "reader-or-checkpointer-busy" {
		t.Fatal("active snapshot must preserve required WAL beyond retention limit", pinned, err, s.CheckpointStatus())
	}
	if err := reader.QueryRow(`SELECT count(*) FROM reclaim_large`).Scan(&count); err != nil || count != 1 {
		t.Fatal("reader snapshot changed during oversized WAL checkpoint", count, err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(context.Background())
	status := s.CheckpointStatus()
	after, err := os.Stat(path)
	if err != nil || after.Size() < 64<<20 || status.LastReclaim != "restart-ready" || status.ReclaimPolicy != "size-triggered-restart" || status.State != "caught-up" {
		t.Fatal("passive checkpoint forced reclamation or did not complete", after, err, status)
	}
	if _, err := s.db.Exec(`INSERT INTO reclaim_large VALUES(x'01')`); err != nil {
		t.Fatal("writer reset", err)
	}
	after, err = os.Stat(path)
	if err != nil || after.Size() > 64<<20 || after.Size() == 0 {
		t.Fatal("normal reset did not reclaim retained WAL", after, err)
	}
	var size int
	if err := s.db.QueryRow(`SELECT length(data) FROM reclaim_large`).Scan(&size); err != nil || size != 68157440 {
		t.Fatal("large committed value changed", size, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM reclaim_large`).Scan(&count); err != nil || count != 3 {
		t.Fatal("writer reset lost committed rows", count, err)
	}
	checkReclaimConnectionPolicy(t, s)
}

func TestCanceledWALReclaimKeepsConnectionPolicy(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := s.reclaimWAL(ctx); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled reclaim accepted", ok, err)
	}
	checkReclaimConnectionPolicy(t, s)
}
