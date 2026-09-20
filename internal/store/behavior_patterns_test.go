package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBehaviorPatternsCompleteCatalogAndCursor(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for i := 0; i < 65; i++ {
		src := testSource()
		src.SourceID = fmt.Sprint("behavior-", i)
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprint("behavior-session-", i)
		if i == 50 || i == 51 {
			b.Session.LastActivity = b.Session.LastActivity.Add(time.Second)
		}
		if i == 5 {
			b.Session.Completeness = "cache-only"
		}
		tool := "Read"
		if i == 1 {
			tool = "mcp__host__Read"
		}
		b.Events = []Event{{ID: fmt.Sprint("call-", i), Kind: "tool-call", AgentID: "main", SourceLength: 2, Data: json.RawMessage(`{"tool":"` + tool + `"}`)}}
		if i == 2 {
			b.Events = append(b.Events, Event{ID: "unknown-result", Kind: "tool-result", AgentID: "main", SourceLength: 2, Data: json.RawMessage(`{}`)})
		}
		if i == 7 {
			b.Events = append(b.Events, Event{ID: "reported-error", Kind: "tool-result", AgentID: "main", SourceLength: 2, Data: json.RawMessage(`{"error":true}`)})
		}
		if i == 3 {
			b.Events[0].AgentID = ""
		}
		if i == 4 {
			b.Events = nil
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		if i == 6 {
			ingest(t, s, src, 2, "y\n")
		}
	}
	page, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 1})
	if err != nil || page.TotalSessions != 65 || page.TotalPatterns != 5 || len(page.Patterns) != 1 || page.Patterns[0].Sessions != 59 || page.Patterns[0].Outcome != "no-reported-errors" || page.Patterns[0].Fanout != "solo" || page.Patterns[0].Tools[0] != "Read" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	seen := map[string]bool{}
	cursor := page.NextCursor
	for cursor != "" {
		next, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 1, Cursor: cursor})
		if err != nil || next.Snapshot != page.Snapshot || len(next.Patterns) != 1 {
			t.Fatal(next, err)
		}
		p := next.Patterns[0]
		seen[p.Fanout+"/"+p.Outcome] = true
		cursor = next.NextCursor
	}
	for _, want := range []string{"solo/reported-errors", "solo/unknown", "incomplete-attribution/no-reported-errors", "unobserved/no-indexed-events"} {
		if !seen[want] {
			t.Fatal("lost uncertainty", want, seen)
		}
	}
	yes := true
	no := false
	activeBefore, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 1, Archived: &no})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchMetadata(ctx, "behavior-session-0", MetadataPatch{Archived: &yes, OperationID: "behavior-archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 1, Cursor: activeBefore.NextCursor, Archived: &no}); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("stale cursor accepted", err)
	}
	active, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 20, Archived: &no})
	if err != nil || active.TotalSessions != 64 {
		t.Fatal(active, err)
	}
	archived, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 20, Archived: &yes})
	if err != nil || archived.TotalSessions != 1 {
		t.Fatal(archived, err)
	}
	if _, err := s.BehaviorPatterns(ctx, SessionQuery{Limit: 1, Cursor: page.NextCursor, MachineID: "another-machine"}); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cross-filter cursor accepted", err)
	}
	for _, query := range []SessionQuery{{}, {Archived: &yes}, {Archived: &no}, {MachineID: "missing"}, {Provider: "claude"}, {Project: "project"}} {
		assertGroupedPatternEvidence(t, s, query)
	}
}
