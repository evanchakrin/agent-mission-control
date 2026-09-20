package store

import (
	"context"
	"errors"
	"testing"
)

func TestNormalQueueDefersToRebuildWithoutHidingFailures(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 7)); err != nil {
		t.Fatal(err)
	}
	before, err := s.PendingSources(ctx, "", 10)
	if err != nil || len(before) != 1 {
		t.Fatal("missing initial work", before, err)
	}
	job, err := s.BeginRebuild(ctx, "session-1", "repair", "4")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"building", "verifying", "ready"} {
		if _, err = s.db.Exec(`UPDATE projection_revisions SET state=? WHERE revision=?`, state, job.Revision); err != nil {
			t.Fatal(err)
		}
		pending, e := s.PendingSources(ctx, "", 10)
		if e != nil || len(pending) != 0 {
			t.Fatal("normal work raced staged repair", state, pending, e)
		}
	}
	if err = s.RecordIndexAttempt(ctx, src.SourceID, src.Generation, 13, false, errors.New("old checkpoint")); err != nil {
		t.Fatal(err)
	}
	if err = s.RebuildProblem(ctx, job.Revision, errors.New("real rebuild failure")); err != nil {
		t.Fatal(err)
	}
	progress, err := s.IndexingProgress(ctx)
	if err != nil || progress.Rebuilding != 1 || progress.Blocked != 0 || progress.Problem != "" {
		t.Fatal("misclassified rebuild-owned normal work", progress, err)
	}
	rebuild, err := s.RebuildProgress(ctx)
	if err != nil || rebuild.Blocked != 1 || rebuild.Problem != "real rebuild failure" {
		t.Fatal("real repair failure hidden", rebuild, err)
	}
	// Ending ownership does not delete the queued source or its saved problem.
	if _, err = s.db.Exec(`UPDATE projection_revisions SET state='obsolete' WHERE revision=?`, job.Revision); err != nil {
		t.Fatal(err)
	}
	progress, err = s.IndexingProgress(ctx)
	if err != nil || progress.Rebuilding != 0 || progress.Blocked != 1 || progress.Problem != "old checkpoint" {
		t.Fatal("normal queue erased", progress, err)
	}
	if _, err = s.db.Exec(`UPDATE index_work SET ready_at=''`); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingSources(ctx, "", 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("normal work did not resume", pending, err)
	}
	for _, state := range []string{"building", "verifying", "ready", "obsolete"} {
		if _, err = s.db.Exec(`UPDATE projection_revisions SET state=? WHERE revision=?`, state, job.Revision); err != nil {
			t.Fatal(err)
		}
		retries, e := s.RetrySources(ctx, 1)
		want := 0
		if state == "obsolete" {
			want = 1
		}
		if e != nil || len(retries) != want {
			t.Fatal("priority retry ignored rebuild ownership", state, retries, e)
		}
	}
}
