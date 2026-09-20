package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDelegatedResultRefreshTracksLateAndDuplicateResultsWithoutChangingOrganization(t *testing.T) {
	for _, related := range []bool{false, true} {
		name := "exact"
		if related {
			name = "ranked"
		}
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx, src := context.Background(), testSource()
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			appendEvent := func(id, kind string, offset int64) {
				t.Helper()
				ingest(t, s, src, offset, "x\n")
				b := batch(src, offset, offset+2)
				b.Events = []Event{{ID: id, AgentID: "main", Kind: kind, Text: "orchard", Timestamp: at.Add(time.Duration(offset) * time.Second), SourceOffset: offset, SourceLength: 2, Data: json.RawMessage(`{"tool":"Task","toolUseId":"call-1","error":true}`)}}
				if err := s.CommitIndex(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent("call", "tool-call", 0)
			yes, ownerName := true, "Owner archived this"
			before, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "archive-call", Archived: &yes, Name: &ownerName})
			if err != nil {
				t.Fatal(err)
			}
			q := SearchQuery{Text: "orchard", DelegatedOnly: true, Related: related, Limit: 5}
			p, err := s.SearchPinned(ctx, q, "")
			if err != nil || len(p.Events) != 1 || p.Events[0].ToolResult == nil || p.Events[0].ToolResult.State != "missing-result" {
				t.Fatal(p, err)
			}
			original := p.Events[0]
			for i, want := range []string{"matched", "ambiguous"} {
				appendEvent([]string{"late-result", "duplicate-result"}[i], "tool-result", int64(2*(i+1)))
				next, err := s.SearchPinned(ctx, q, p.Snapshot)
				if err != nil || len(next.Events) != 1 {
					t.Fatal(next, err)
				}
				e := next.Events[0]
				if e.ID != original.ID || e.Sequence != original.Sequence || e.HistorySnapshot != original.HistorySnapshot || e.ToolResult == nil || e.ToolResult.State != want {
					t.Fatal("refresh lost identity or result evidence", e)
				}
				if want == "matched" && (e.ToolResult.Error == nil || !*e.ToolResult.Error || e.ToolResult.ResultSequence == nil) {
					t.Fatal("late failure was hidden", e.ToolResult)
				}
				if want == "ambiguous" && (e.ToolResult.Error != nil || e.ToolResult.ResultSequence != nil) {
					t.Fatal("ambiguous result retained stale certainty", e.ToolResult)
				}
				if e.SearchContext == nil || !e.SearchContext.Archived || e.SearchContext.SessionTitle != ownerName {
					t.Fatal("refresh changed organization", e.SearchContext)
				}
			}
			after, err := s.GetSession(ctx, "session-1")
			if err != nil || !reflect.DeepEqual(before, after.Metadata) {
				t.Fatal("indexing changed owner metadata", before, after.Metadata, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(s.dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restored, err := reopened.SearchPinned(ctx, q, p.Snapshot)
			if err != nil || len(restored.Events) != 1 {
				t.Fatal(restored, err)
			}
			e := restored.Events[0]
			if e.ToolResult == nil || e.ToolResult.State != "ambiguous" || e.ToolResult.Error != nil || e.ToolResult.ResultSequence != nil || e.SearchContext == nil || !e.SearchContext.Archived || e.SearchContext.SessionTitle != ownerName {
				t.Fatal("restart lost ambiguity or organization", e)
			}
		})
	}
}

func TestDelegatedSearchAttachesConservativeResultEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(id, agent, kind, key string, failed any) {
		data, _ := json.Marshal(map[string]any{"tool": "Task", "toolUseId": key, "error": failed})
		b.Events = append(b.Events, Event{ID: id, AgentID: agent, Kind: kind, Text: "needle", Data: data, SourceLength: 2, Timestamp: at.Add(time.Duration(len(b.Events)) * time.Second)})
	}
	add("failed", "main", "tool-call", "f", nil)
	add("failed-result", "main", "tool-result", "f", true)
	add("returned", "main", "tool-call", "r", nil)
	add("returned-result", "main", "tool-result", "r", false)
	add("unknown", "main", "tool-call", "u", nil)
	add("unknown-result", "main", "tool-result", "u", nil)
	add("missing", "main", "tool-call", "m", nil)
	add("wrong-agent", "other", "tool-result", "m", true)
	add("ambiguous", "main", "tool-call", "a", nil)
	add("ambiguous-again", "main", "tool-call", "a", nil)
	add("ambiguous-result", "main", "tool-result", "a", false)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	p, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", DelegatedOnly: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*ToolResultEvidence{}
	for _, e := range p.Events {
		seen[e.ID] = e.ToolResult
	}
	for _, id := range []string{"failed", "returned", "unknown"} {
		if seen[id] == nil || seen[id].State != "matched" || seen[id].ResultSequence == nil {
			t.Fatal(id, seen[id])
		}
	}
	if seen["failed"].Error == nil || !*seen["failed"].Error || seen["returned"].Error == nil || *seen["returned"].Error || seen["unknown"].Error != nil {
		t.Fatal("failure evidence changed", seen)
	}
	if seen["missing"] == nil || seen["missing"].State != "missing-result" || seen["missing"].Error != nil {
		t.Fatal("cross-agent correlation", seen)
	}
	if seen["ambiguous"] == nil || seen["ambiguous"].State != "ambiguous" || seen["ambiguous"].ResultSequence != nil {
		t.Fatal("duplicate guessed", seen)
	}
	general, err := s.SearchPinned(ctx, SearchQuery{Text: "needle"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range general.Events {
		if e.ToolResult != nil {
			t.Fatal("general search performs unsolicited correlation")
		}
	}
}
