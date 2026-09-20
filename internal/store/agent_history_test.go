package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAgentHistoryPaginationAndScope(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "evidence\n")
	b := batch(src, 0, 9)
	for i := 0; i < 205; i++ {
		agent := "main"
		if i%2 == 1 {
			agent = "child"
		}
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("agent-event-%d", i), AgentID: agent, Text: "recorded text", SourceLength: 9})
	}
	b.Events = append(b.Events, Event{ID: "unknown-agent", AgentID: "", Text: "unattributed", SourceLength: 9})
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListAgentEventsPinned(ctx, b.Session.ID, "main", 0, 100, "")
	if err != nil || len(first.Events) != 100 || first.NextSequence == 0 {
		t.Fatalf("first page: %d %v", len(first.Events), err)
	}
	for _, e := range first.Events {
		if e.AgentID != "main" || e.HistorySnapshot == "" || e.HistorySnapshot == first.Snapshot {
			t.Fatal("wrong scope or drilldown snapshot")
		}
	}
	second, err := s.ListAgentEventsPinned(ctx, b.Session.ID, "main", first.NextSequence, 100, first.Snapshot)
	if err != nil || len(second.Events) != 3 || second.NextSequence != 0 {
		t.Fatalf("remaining history missing: %d %v", len(second.Events), err)
	}
	if _, err = s.ListAgentEventsPinned(ctx, b.Session.ID, "child", first.NextSequence, 100, first.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("cross-agent cursor accepted: %v", err)
	}
	if _, err = s.ListAgentEventsPinned(ctx, b.Session.ID, "main", first.NextSequence, 100, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing snapshot accepted: %v", err)
	}
	unknown, err := s.ListAgentEventsPinned(ctx, b.Session.ID, "", 0, 100, "")
	if err != nil || len(unknown.Events) != 1 || unknown.Events[0].AgentID != "" {
		t.Fatalf("unattributed filter broadened: %v", err)
	}
	if _, err = s.EventsAroundPinned(ctx, b.Session.ID, second.Events[0].Sequence, 1, 1, second.Events[0].HistorySnapshot); err != nil {
		t.Fatal("record cannot open in session history", err)
	}
	// Simulate a different interpretation without altering raw bytes.
	if _, err = s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.projectionRevision','replaced') WHERE id=?`, b.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListAgentEventsPinned(ctx, b.Session.ID, "main", first.NextSequence, 100, first.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatalf("stale projection accepted: %v", err)
	}
}
