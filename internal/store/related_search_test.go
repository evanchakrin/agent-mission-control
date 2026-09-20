package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRelatedTasksRankBeyondFirstRecordPage(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	for i := 0; i < 1001; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("broad-%d", i), Kind: "tool-call", Text: "orchard irrigation routine", Data: json.RawMessage(`{"tool":"Task"}`), SourceLength: 13})
	}
	b.Events = append(b.Events, Event{ID: "specific", Kind: "tool-call", Text: "orchard irrigation anomaly", Data: json.RawMessage(`{"tool":"Task"}`), SourceLength: 13})
	b.Events = append(b.Events, Event{ID: "not-delegated", Kind: "assistant-text", Text: "anomaly anomaly anomaly", SourceLength: 13})
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	q := SearchQuery{Text: "orchard anomaly absentword", DelegatedOnly: true, Related: true, Limit: 5}
	p, err := s.SearchPinned(ctx, q, "")
	if err != nil || len(p.Events) != 5 || p.Events[0].ID != "specific" || p.NextSequence != 0 {
		t.Fatal(p, err)
	}
	for _, e := range p.Events {
		if e.ID == "not-delegated" {
			t.Fatal("ordinary chat ranked as task")
		}
	}
	q.Related = false
	if _, err = s.SearchPinned(ctx, q, p.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed mode accepted", err)
	}
	p, err = s.SearchPinned(ctx, q, "")
	if err != nil || len(p.Events) != 0 {
		t.Fatal("exact search changed", p, err)
	}
	q.Related = true
	q.AfterSequence = 1
	if _, err = s.Search(ctx, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("ranked sequence continuation accepted", err)
	}
	q.AfterSequence = 0
	q.DelegatedOnly = false
	if _, err = s.Search(ctx, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("unsupported ranking scope accepted", err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+eventQuerySQL("events_fts MATCH ?", true, true), "orchard OR anomaly", 6)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatal("rank order materializes sort", detail)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestDelegatedSearchUsesNarrowCallIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	where := `events_fts MATCH ? AND events_fts.rowid>? AND e.kind='tool-call' AND json_extract(e.data,'$.tool') IN ('Task','Agent','spawn_agent','functions.spawn_agent','collaboration.spawn_agent')`
	for _, ranked := range []bool{false, true} {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+eventQuerySQL(where, true, ranked, true), `"test"`, 0, 6)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "events_delegated_page") {
				found = true
			}
			if !ranked && strings.Contains(detail, "TEMP B-TREE") {
				t.Error("exact delegation page sorts", detail)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatal("missing narrow delegation index", ranked, err)
		}
	}
}
