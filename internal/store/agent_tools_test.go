package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAgentToolsFullHistoryAndStableRanking(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "evidence\n")
	b := batch(src, 0, 9)
	for i, name := range []string{"B", "A", "B", "A", "B", ""} {
		data, _ := json.Marshal(map[string]string{"tool": name})
		b.Events = append(b.Events, Event{ID: fmt.Sprint("tool-", i), AgentID: "main", Kind: "tool-call", Data: data, SourceLength: 9})
	}
	b.Events = append(b.Events, Event{ID: "other-agent", AgentID: "child", Kind: "tool-call", Data: json.RawMessage(`{"tool":"other"}`), SourceLength: 9})
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, err := s.AgentTools(ctx, b.Session.ID, "main", "", 1)
	if err != nil || len(first.Tools) != 1 || first.Tools[0].Name != "B" || first.Tools[0].Calls != 3 || first.NextCursor == "" {
		t.Fatalf("full tool counts: %+v %v", first, err)
	}
	// Later calls would change ranking unless continued pages retain their head.
	ingest(t, s, src, 9, "evidence\n")
	more := batch(src, 9, 18)
	for i := 0; i < 5; i++ {
		more.Events = append(more.Events, Event{ID: fmt.Sprint("append-", i), AgentID: "main", Kind: "tool-call", Data: json.RawMessage(`{"tool":"A"}`), SourceOffset: 9, SourceLength: 9})
	}
	if err = s.CommitIndex(ctx, more); err != nil {
		t.Fatal(err)
	}
	second, err := s.AgentTools(ctx, b.Session.ID, "main", first.NextCursor, 100)
	if err != nil || len(second.Tools) != 2 || second.Tools[0].Name != "A" || second.Tools[0].Calls != 2 || second.Tools[1].Name != "" || second.ThroughSequence != first.ThroughSequence {
		t.Fatalf("append changed continued ranking: %+v %v", second, err)
	}
	fresh, err := s.AgentTools(ctx, b.Session.ID, "main", "", 100)
	if err != nil || fresh.Tools[0].Name != "A" || fresh.Tools[0].Calls != 7 {
		t.Fatalf("refresh omitted new calls: %+v %v", fresh, err)
	}
	if _, err = s.AgentTools(ctx, b.Session.ID, "child", first.NextCursor, 100); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("cross-agent cursor accepted: %v", err)
	}
	if _, err = s.AgentTools(ctx, b.Session.ID, "main", "not-a-cursor", 100); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+agentToolsSQL, b.Session.ID, "main", first.ThroughSequence, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "query_events_scope") && strings.Contains(detail, "agent_id=?") {
			found = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("tool aggregation does not constrain its index scan to the selected agent")
	}
}
