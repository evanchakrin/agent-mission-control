package store

import (
	"context"
	"errors"
	"testing"
)

func TestSearchCursorAllowsAppendAndSurvivesReopen(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "first", Text: "needle", SourceLength: 13}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	q := SearchQuery{Text: "needle", Limit: 1}
	p, err := s.SearchPinned(ctx, q, "")
	if err != nil || len(p.Events) != 1 {
		t.Fatal(p, err)
	}
	src.Size = 26
	ingest(t, s, src, 13, "source bytes\n")
	b = batch(src, 13, 26)
	b.Events = []Event{{ID: "second", Text: "needle", SourceOffset: 13, SourceLength: 13}}
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	q.AfterSequence = p.Events[0].Sequence
	next, err := s.SearchPinned(ctx, q, p.Snapshot)
	if err != nil || len(next.Events) != 1 || next.Events[0].ID != "second" {
		t.Fatal("append invalidated search", next, err)
	}
	dir := s.dir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.SearchPinned(ctx, q, p.Snapshot); err != nil {
		t.Fatal("reopen invalidated search", err)
	}
	// Explicit deletion is a fixture-only mutation to exercise the durable
	// invalidation trigger, not an application retention operation.
	if _, err = s.db.Exec("DELETE FROM sessions WHERE id='session-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SearchPinned(ctx, q, p.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("deletion did not invalidate search", err)
	}
}
