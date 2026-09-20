package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestBackgroundCheckpointServicesDirtyWALPromptly(t *testing.T) {
	s := openTestStore(t, Options{})
	if _, err := s.db.Exec(`CREATE TABLE cadence_background(data BLOB); INSERT INTO cadence_background VALUES(zeroblob(5242880))`); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		status := s.CheckpointStatus()
		if status.State == "caught-up" && status.LastComplete != nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("dirty WAL was left waiting for the old ten-second cadence", status)
		case <-poll.C:
		}
	}
}

func TestControlledCheckpointsKeepFullDurabilityOnEveryConnection(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var full, automatic, mapped, retained int
		if err = conn.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&retained); err != nil || retained != 64<<20 {
			t.Fatal("retained WAL policy", retained, err)
		}
		if err = conn.QueryRowContext(ctx, `PRAGMA mmap_size`).Scan(&mapped); err != nil || mapped != 8<<20 {
			t.Fatal("bounded mapped-read policy", mapped, err)
		}
		if err = conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
			t.Fatal(full, err)
		}
		if err = conn.QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&automatic); err != nil || automatic != 0 {
			t.Fatal(automatic, err)
		}
	}
}

func TestManagedParserStoreDoesNotStartSecondCheckpointLoop(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	if s.checkpointStop != nil || s.checkpointDone != nil || s.CheckpointStatus().State != "managed-by-owner" {
		t.Fatal("managed follower started maintenance")
	}
	var full, automatic int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
		t.Fatal(full, err)
	}
	if err := s.db.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&automatic); err != nil || automatic != 0 {
		t.Fatal(automatic, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlledCheckpointCopiesCommittedPages(t *testing.T) {
	// Explicit checkpoint assertions need sole ownership. checkpointOnce may
	// deliberately skip while the background loop owns checkpointRun.
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TABLE checkpoint_fixture (data BLOB); INSERT INTO checkpoint_fixture VALUES(zeroblob(5242880));`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	status := s.CheckpointStatus()
	if status.State != "caught-up" || status.LastComplete == nil || status.CopiedFrames != status.LogFrames {
		t.Fatal(status)
	}
	if status.LastAttempt == nil || status.AttemptWALBytes < 5<<20 || status.PassiveMilliseconds <= 0 || status.RestartMilliseconds != nil {
		t.Fatal("checkpoint timing does not describe the completed passive-only attempt", status)
	}
	s.checkpointOnce(ctx)
	if s.CheckpointStatus().State != "caught-up" {
		t.Fatal("repeat checkpoint failed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointLoopStopsWithStore(t *testing.T) {
	s := openTestStore(t, Options{})
	if s.checkpointDone == nil {
		t.Fatal("background loop did not start")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.checkpointDone:
	default:
		t.Fatal("checkpoint loop survived store close")
	}
}

func TestControlledCheckpointRetainsReaderSnapshotAndRetries(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TABLE checkpoint_reader (data BLOB); INSERT INTO checkpoint_reader VALUES(zeroblob(5242880));`); err != nil {
		t.Fatal(err)
	}
	reader, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err = reader.QueryRow(`SELECT COUNT(*) FROM checkpoint_reader`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = s.db.Exec(`INSERT INTO checkpoint_reader VALUES(zeroblob(5242880));`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	if state := s.CheckpointStatus(); state.State != "reader-or-checkpointer-busy" || state.CopiedFrames >= state.LogFrames {
		t.Fatal(state)
	}
	if err = reader.QueryRow(`SELECT COUNT(*) FROM checkpoint_reader`).Scan(&count); err != nil || count != 1 {
		t.Fatal("reader snapshot changed", count, err)
	}
	if err = reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	if state := s.CheckpointStatus(); state.State != "caught-up" {
		t.Fatal(state)
	}
}
