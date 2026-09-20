package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type rebuildBusyCode int

func (e rebuildBusyCode) Error() string { return "fixture sqlite result" }
func (e rebuildBusyCode) Code() int     { return int(e) }

func TestRebuildContentionRetriesPromptlyWithoutReset(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 7)); err != nil {
		t.Fatal(err)
	}
	job, err := s.BeginRebuild(ctx, "session-1", "retry-fixture", "7")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		err   error
		delay time.Duration
	}{
		{fmt.Errorf("begin: %w", rebuildBusyCode(5)), 2 * time.Second},
		{rebuildBusyCode(262), 2 * time.Second},
		{ErrWriterQueueFull, 2 * time.Second},
		{errors.New("SQLITE_BUSY as untrusted text"), 5 * time.Minute},
		{rebuildBusyCode(11), 5 * time.Minute},
	} {
		if err = s.RebuildProblem(ctx, job.Revision, tc.err); err != nil {
			t.Fatal(err)
		}
		var retry, updated, state string
		var offset int64
		if err = s.db.QueryRow(`SELECT retry_at,updated_at,state,indexed_offset FROM projection_revisions WHERE revision=?`, job.Revision).Scan(&retry, &updated, &state, &offset); err != nil {
			t.Fatal(err)
		}
		deadline, e := time.Parse(time.RFC3339Nano, retry)
		if e != nil {
			t.Fatal(e)
		}
		at, e := time.Parse(time.RFC3339Nano, updated)
		if e != nil {
			t.Fatal(e)
		}
		delay := deadline.Sub(at)
		if delay < tc.delay-time.Second || delay > tc.delay+time.Second {
			t.Fatalf("retry %v got %v want %v", tc.err, delay, tc.delay)
		}
		if state != job.State || offset != job.IndexedOffset {
			t.Fatal("retry changed accepted job progress")
		}
	}
	if err = s.RebuildProblem(ctx, job.Revision, nil); err != nil {
		t.Fatal(err)
	}
	var retry, problem string
	if err = s.db.QueryRow(`SELECT retry_at,error FROM projection_revisions WHERE revision=?`, job.Revision).Scan(&retry, &problem); err != nil || retry != "" || problem != "" {
		t.Fatal("successful progress did not clear retry", retry, problem, err)
	}
}
