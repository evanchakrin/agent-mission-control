package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointSmallWALDoesNotAttemptRestart(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	if _, err := s.db.Exec(`CREATE TABLE small_restart_fixture(data BLOB); INSERT INTO small_restart_fixture VALUES(zeroblob(5242880))`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(context.Background())
	status := s.CheckpointStatus()
	if status.State != "caught-up" || status.LastReclaim != "" || status.ReclaimPolicy != "size-triggered-restart" {
		t.Fatal("small checkpoint attempted recycling", status)
	}
}

func TestCheckpointReportsRestartTiming(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	if _, err := s.db.Exec(`CREATE TABLE restart_timing(data BLOB); INSERT INTO restart_timing VALUES(zeroblob(68157440))`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(context.Background())
	status := s.CheckpointStatus()
	if status.State != "caught-up" || status.AttemptWALBytes <= 64<<20 || status.PassiveMilliseconds < 0 || status.RestartMilliseconds == nil || *status.RestartMilliseconds < 0 || status.LastReclaim != "restart-ready" {
		t.Fatal("large completed attempt lacks separate phase timings", status)
	}
	// A failed passive attempt must not retain a restart timing from the prior
	// successful attempt under the new LastAttempt timestamp.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.checkpointOnce(ctx)
	failed := s.CheckpointStatus()
	if failed.State != "error" || failed.RestartMilliseconds != nil || failed.PassiveMilliseconds < 0 || !failed.LastAttempt.After(*status.LastAttempt) {
		t.Fatal("failed attempt reused old timing", failed)
	}
}

func TestWALRestartPreservesBusySnapshotsAndConnectionPolicy(t *testing.T) {
	for _, writer := range []bool{false, true} {
		t.Run(map[bool]string{false: "reader", true: "writer"}[writer], func(t *testing.T) {
			s := openTestStore(t, Options{ExternalCheckpointOwner: true})
			ctx := context.Background()
			if _, err := s.db.Exec(`CREATE TABLE restart_fixture(value INTEGER); INSERT INTO restart_fixture VALUES(1)`); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writer})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var value int
			if err = tx.QueryRow(`SELECT value FROM restart_fixture`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if ok, err := s.restartWAL(ctx); err != nil || ok || time.Since(started) >= 250*time.Millisecond {
				t.Fatal("busy restart failed to back off", ok, err, time.Since(started))
			}
			if err = tx.QueryRow(`SELECT value FROM restart_fixture`).Scan(&value); err != nil || value != 1 {
				t.Fatal("reader evicted or changed", value, err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.restartWAL(ctx); err != nil || !ok {
				t.Fatal("idle restart failed", ok, err)
			}
			info, err := os.Stat(filepath.Join(s.dir, "ledger.sqlite-wal"))
			if err != nil || info.Size() == 0 {
				t.Fatal("restart unexpectedly truncated WAL", info, err)
			}
			if _, err = s.db.Exec(`INSERT INTO restart_fixture VALUES(2)`); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRow(`SELECT sum(value) FROM restart_fixture`).Scan(&value); err != nil || value != 3 {
				t.Fatal("reset lost rows", value, err)
			}
			checkReclaimConnectionPolicy(t, s)
		})
	}
}
