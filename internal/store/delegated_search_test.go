package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestSearchContextUsesCurrentNamesWithoutChangingEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "request", Kind: "tool-call", Text: "needle", SourceLength: 13, Data: json.RawMessage(`{"tool":"Task"}`)}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	name, yes := "Owner session name", true
	if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: "context-name", Name: &name, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: src.MachineID, Name: "Friendly machine"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", DelegatedOnly: true}, "")
	if err != nil || len(p.Events) != 1 {
		t.Fatal(p, err)
	}
	c := p.Events[0].SearchContext
	if c == nil || c.SessionTitle != name || c.MachineID != src.MachineID || c.MachineName != "Friendly machine" || c.Provider != "claude" || !c.Archived {
		t.Fatal(c)
	}
	if p.Events[0].Text != "needle" || p.Events[0].SourceOffset != 0 || p.Events[0].SourceLength != 13 {
		t.Fatal("source evidence changed", p.Events)
	}
	if err := s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: src.MachineID, Name: "Renamed machine"}); err != nil {
		t.Fatal(err)
	}
	next, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", DelegatedOnly: true}, p.Snapshot)
	if err != nil || next.Events[0].SearchContext.MachineName != "Renamed machine" {
		t.Fatal(next, err)
	}
	if _, err = s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: src.MachineID, DisplayName: "Owner override", OperationID: "search-machine-label"}); err != nil {
		t.Fatal(err)
	}
	next, err = s.SearchPinned(ctx, SearchQuery{Text: "needle", DelegatedOnly: true}, p.Snapshot)
	if err != nil || len(next.Events) != 1 || next.Events[0].SearchContext.MachineName != "Owner override" || next.Events[0].SearchContext.MachineID != src.MachineID || next.Events[0].Text != "needle" || next.Events[0].SourceLength != 13 {
		t.Fatal("label changed evidence context", next, err)
	}
}

func TestDelegatedSearchFiltersBeforePagingAndBindsCursor(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	for i := 0; i < 1001; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("ordinary-%d", i), Kind: "assistant-text", Text: "needle", SourceLength: 13})
	}
	for i, tool := range []string{"Task", "Agent", "spawn_agent", "functions.spawn_agent", "collaboration.spawn_agent", "Bash"} {
		data, _ := json.Marshal(map[string]string{"tool": tool})
		b.Events = append(b.Events, Event{ID: tool, Kind: "tool-call", Text: "needle", Data: data, SourceLength: 13})
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("result-%d", i), Kind: "tool-result", Text: "needle", Data: data, SourceLength: 13})
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	q := SearchQuery{Text: "needle", DelegatedOnly: true, Limit: 2}
	page, err := s.SearchPinned(ctx, q, "")
	if err != nil || len(page.Events) != 2 || page.NextSequence == 0 {
		t.Fatal(page, err)
	}
	first := page.Snapshot
	var ids []string
	for {
		for _, e := range page.Events {
			ids = append(ids, e.ID)
		}
		if page.NextSequence == 0 {
			break
		}
		q.AfterSequence = page.NextSequence
		page, err = s.SearchPinned(ctx, q, page.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(ids) != "[Task Agent spawn_agent functions.spawn_agent collaboration.spawn_agent]" {
		t.Fatal(ids)
	}
	q.DelegatedOnly = false
	if _, err = s.SearchPinned(ctx, q, first); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed scope accepted", err)
	}
}
