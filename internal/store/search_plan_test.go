package store

import (
	"context"
	"strings"
	"testing"
)

func TestSearchStreamsOrderedIndexWithoutSortingWholeResult(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "one", Text: "needle", SourceLength: 13}, {ID: "two", Text: "needle", SourceLength: 13}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		where := `events_fts MATCH ? AND events_fts.rowid>?`
		args := []any{`"needle"`, 0}
		if scoped {
			where += ` AND e.session_id=?`
			args = append(args, "session-1")
		}
		args = append(args, 2)
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+eventQuerySQL(where, true), args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				t.Error("search requires whole-result sort", detail)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", Limit: 1}, "")
	if err != nil || len(first.Events) != 1 || first.NextSequence == 0 {
		t.Fatal(first, err)
	}
	second, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", Limit: 1, AfterSequence: first.NextSequence}, first.Snapshot)
	if err != nil || len(second.Events) != 1 || second.Events[0].Sequence <= first.Events[0].Sequence || second.NextSequence != 0 {
		t.Fatal(second, err)
	}
}
