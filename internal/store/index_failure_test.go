package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIndexFailureIdentifiesStageAndRollsBack(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	// Only this disposable database is damaged to exercise a search-write failure.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE events_fts"); err != nil {
		t.Fatal(err)
	}
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "failed-event", Text: "needle", SourceLength: 13}}
	err := s.CommitIndex(ctx, b)
	if err == nil || !strings.Contains(err.Error(), "index transaction search text:") {
		t.Fatalf("missing failing stage: %v", err)
	}
	if !strings.Contains(err.Error(), "transaction elapsed ") {
		t.Fatalf("missing transaction timing: %v", err)
	}
	state, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || state.IndexedOffset != 0 {
		t.Fatalf("checkpoint advanced: %+v %v", state, err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial events published: %d %v", count, err)
	}
	if err = s.CommitIndex(ctx, IndexBatch{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrapping lost error identity: %v", err)
	}
}
