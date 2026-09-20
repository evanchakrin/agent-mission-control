package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type CheckpointStatus struct {
	State          string     `json:"state"`
	LastAttempt    *time.Time `json:"lastAttempt,omitempty"`
	LastComplete   *time.Time `json:"lastComplete,omitempty"`
	LogFrames      int        `json:"logFrames"`
	CopiedFrames   int        `json:"copiedFrames"`
	Problem        string     `json:"problem,omitempty"`
	LastReclaim    string     `json:"lastReclaim,omitempty"`
	ReclaimProblem string     `json:"reclaimProblem,omitempty"`
	ReclaimPolicy  string     `json:"reclaimPolicy,omitempty"`
	// Timings belong to LastAttempt, include connection/lock/I/O waits, and
	// describe completed calls only. WAL allocation is not dirty-page volume.
	AttemptWALBytes     int64    `json:"attemptWalBytes,omitempty"`
	PassiveMilliseconds float64  `json:"passiveMilliseconds,omitempty"`
	RestartMilliseconds *float64 `json:"restartMilliseconds,omitempty"`
}

func (s *Store) CheckpointStatus() CheckpointStatus {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	return s.checkpointStatus
}

// Commits still synchronously flush the WAL (FULL); only copying committed WAL
// pages into the database moves off the writer's request path. PASSIVE does not
// evict reader snapshots or wait for their completion. Multiple processes may
// attempt a checkpoint; SQLite coordinates them and busy attempts retry later.
func (s *Store) runCheckpoints(ctx context.Context) {
	defer close(s.checkpointDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
			s.checkpointOnce(attempt)
			cancel()
		}
	}
}

func (s *Store) checkpointOnce(ctx context.Context) {
	if !s.checkpointRun.TryLock() {
		return
	}
	defer s.checkpointRun.Unlock()
	status := s.CheckpointStatus()
	status.ReclaimPolicy = "size-triggered-restart"
	defer func() { s.checkpointMu.Lock(); s.checkpointStatus = status; s.checkpointMu.Unlock() }()
	// A reused WAL can keep its size and coarse timestamp. Never skip necessary
	// checkpoints based on those fields. Small WALs need only the filesystem stat;
	// Larger files get a PASSIVE attempt per second, limiting dirty-page bursts
	// instead of accumulating ten seconds of writes before each disk flush.
	// The five-second context
	// deadline is cooperative: a low-level disk flush can finish after cancellation.
	info, err := os.Stat(filepath.Join(s.dir, "ledger.sqlite-wal"))
	if errors.Is(err, os.ErrNotExist) {
		status.State = "waiting"
		status.Problem = ""
		return
	}
	if err != nil {
		status.State = "error"
		status.Problem = err.Error()
		return
	}
	if info.Size() < 4<<20 {
		status.State = "waiting"
		status.Problem = ""
		return
	}
	now := time.Now().UTC()
	status.LastAttempt = &now
	status.AttemptWALBytes = info.Size()
	status.RestartMilliseconds = nil
	var busy, frames, copied int
	started := time.Now()
	err = s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &frames, &copied)
	status.PassiveMilliseconds = float64(time.Since(started)) / float64(time.Millisecond)
	if err != nil {
		status.State = "error"
		status.Problem = err.Error()
		return
	}
	status.Problem = ""
	status.LogFrames = frames
	status.CopiedFrames = copied
	if busy != 0 || copied < frames {
		status.State = "reader-or-checkpointer-busy"
		return
	}
	status.State = "caught-up"
	done := time.Now().UTC()
	status.LastComplete = &done
	// Continuous overlapping readers can prevent natural resets indefinitely.
	// Only after a caught-up PASSIVE checkpoint and above the retained-size
	// threshold, give recycling a small lock-wait allowance. The next writer
	// applies journal_size_limit; never force TRUNCATE here. This is not an I/O
	// deadline or a hard size cap while readers/writers require the WAL.
	if info.Size() > 64<<20 {
		started := time.Now()
		ready, err := s.restartWAL(ctx)
		elapsed := float64(time.Since(started)) / float64(time.Millisecond)
		status.RestartMilliseconds = &elapsed
		status.ReclaimProblem = ""
		switch {
		case err != nil:
			status.LastReclaim = "error"
			status.ReclaimProblem = err.Error()
		case ready:
			status.LastReclaim = "restart-ready"
		default:
			status.LastReclaim = "busy"
		}
	}
}
