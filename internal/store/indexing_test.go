package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBlockedIndexRetrySurvivesRestartAndRetainsReceipt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	receipt := ingest(t, s, src, 0, "fixture\n")
	before := time.Now()
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 8, false, errors.New("SQLITE_BUSY fixture")); err != nil {
		t.Fatal(err)
	}
	var readyAt string
	if err = s.db.QueryRow(`SELECT ready_at FROM index_work`).Scan(&readyAt); err != nil {
		t.Fatal(err)
	}
	if readyAt < stamp(before.Add(5*time.Minute)) || readyAt > stamp(time.Now().Add(5*time.Minute)) {
		t.Fatal("retry must be scheduled five minutes after failure", readyAt)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err := s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 0 {
		t.Fatal("restart bypassed durable backoff", ready, err)
	}
	if retry, e := s.RetrySources(ctx, 1); e != nil || len(retry) != 0 {
		t.Fatal("priority retry bypassed durable backoff", retry, e)
	}
	p, err := s.IndexingProgress(ctx)
	if err != nil || p.Blocked != 1 || p.Problem != "SQLITE_BUSY fixture" {
		t.Fatal("restart erased failed work", p, err)
	}
	// Advance only this disposable fixture's persisted due time. No wall-clock
	// sleep or production clock override is needed to exercise queue eligibility.
	if _, err = s.db.Exec(`UPDATE index_work SET ready_at=? WHERE source_id=? AND generation=?`, stamp(time.Now().Add(-time.Second)), src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	ready, err = s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 1 || ready[0].IndexedOffset != 0 || ready[0].DurableOffset != 8 {
		t.Fatal("due retry lost original checkpoint", ready, err)
	}
	if retry, e := s.RetrySources(ctx, 1); e != nil || len(retry) != 1 || retry[0].Source.SourceID != src.SourceID {
		t.Fatal("priority retry missed due failure", retry, e)
	}
	if err = s.CommitIndex(ctx, batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 8, true, nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.IndexingProgress(ctx)
	if err != nil || p.Blocked != 0 || p.Ready != 0 || p.Problem != "" {
		t.Fatal("successful retry remained queued", p, err)
	}
	if replay := ingest(t, s, src, 0, "fixture\n"); replay.ReceiptID != receipt.ReceiptID || replay.DurableOffset != 8 {
		t.Fatal("retry changed accepted receipt", replay, receipt)
	}
}

func TestSuccessfulPartialRetryClearsOldFailureWithoutRevivingCompletedWork(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	if err := s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 13, false, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitIndex(ctx, batch(src, 0, 7)); err != nil {
		t.Fatal(err)
	}
	atomicProgress, atomicErr := s.IndexingProgress(ctx)
	if atomicErr != nil || atomicProgress.Blocked != 0 || atomicProgress.Ready != 1 || atomicProgress.Problem != "" {
		t.Fatal("committed partial progress did not atomically clear failure", atomicProgress, atomicErr)
	}
	if err := s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 13, true, nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.IndexingProgress(ctx)
	if err != nil || p.Blocked != 0 || p.Ready != 1 || p.Problem != "" {
		t.Fatal("successful partial retry stayed blocked", p, err)
	}
	if err = s.CommitIndex(ctx, batch(src, 7, 13)); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 13, true, nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.IndexingProgress(ctx)
	if err != nil || p.Ready != 0 {
		t.Fatal("completed work was revived", p, err)
	}
	ingest(t, s, src, 13, "more\n")
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 18, false, errors.New("new failure")); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 13, true, nil); err != nil {
		t.Fatal(err)
	}
	p, err = s.IndexingProgress(ctx)
	if err != nil || p.Blocked != 1 || p.Problem != "new failure" {
		t.Fatal("stale success erased new failure", p, err)
	}
}

func TestIndexQueueWaitsForBytesAndPersistsProgress(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	if err := s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, src, 0, "partial")
	ready, err := s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 1 {
		t.Fatal(ready, err)
	}
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 7, false, nil); err != nil {
		t.Fatal(err)
	}
	ready, err = s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 0 {
		t.Fatal("partial record was busy-polled", ready, err)
	}
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err = s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 0 {
		t.Fatal("restart forgot parked partial", ready, err)
	}
	ingest(t, s, src, 7, "\n")
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 7, false, errors.New("old error")); err != nil {
		t.Fatal(err)
	}
	ready, err = s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 1 {
		t.Fatal("new bytes did not wake parser", ready, err)
	}
	if err = s.CommitIndex(ctx, batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	ready, err = s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 0 {
		t.Fatal("completed work remains", ready, err)
	}
	p, err := s.IndexingProgress(ctx)
	if err != nil || p.LastProgress.IsZero() || p.AwaitingRecord != 0 || p.Blocked != 0 {
		t.Fatal(p, err)
	}
}
