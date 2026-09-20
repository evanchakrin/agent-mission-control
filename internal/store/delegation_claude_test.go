package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClaudeSpawnChildRequiresBoundResultEnvelope(t *testing.T) {
	raw := `{"type":"user","sessionId":"root","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"agentId: false-child"}]},"toolUseResult":{"agentId":"child"}}`
	id, state := claudeSpawnChild([]byte(raw), "call", "root", "main")
	if id != "child" || state != "known" {
		t.Fatal(id, state)
	}
	for _, tc := range []struct{ raw, call, root, caller string }{
		{raw, "other", "root", "main"}, {raw, "call", "other", "main"}, {raw, "call", "root", "wrong"},
		{strings.Replace(raw, `"agentId":"child"`, `"agentId":"child","agentId":"duplicate"`, 1), "call", "root", "main"},
		{strings.Replace(raw, `"toolUseResult"`, `"unrelated"`, 1), "call", "root", "main"},
		{strings.Replace(raw, `"content":[`, `"content":[{"type":"tool_result","tool_use_id":"call"},`, 1), "call", "root", "main"},
	} {
		if id, state := claudeSpawnChild([]byte(tc.raw), tc.call, tc.root, tc.caller); id != "" || state == "known" {
			t.Fatal(id, state, tc)
		}
	}
}

func TestClaudeDelegationChildScopeAndNativeAgent(t *testing.T) {
	for _, mode := range []string{"root", "nested", "wrong-machine", "wrong-container", "ambiguous", "cache-only"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, Options{})
			src := testSource()
			src.NativeID = "root"
			caller := "main"
			if mode == "nested" {
				caller = "nested-parent"
				src.NativeID = "unique-parent-sidechain"
			}
			call := `{"type":"assistant","sessionId":"root","message":{"content":[{"type":"tool_use","name":"Agent","id":"call","input":{"prompt":"fixture"}}]}}` + "\n"
			result := fmt.Sprintf(`{"type":"user","sessionId":"root","agentId":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"display-only"}]},"toolUseResult":{"agentId":"child-agent"}}`, caller) + "\n"
			ingest(t, s, src, 0, call+result)
			b := batch(src, 0, int64(len(call+result)))
			b.Session.NativeID = src.NativeID
			if mode == "nested" {
				b.Session.NativeAgentID = caller
				b.Session.ParentThreadID = "root"
			}
			at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			b.Events = []Event{
				{ID: "call", Kind: "tool-call", AgentID: caller, Timestamp: at, SourceLength: int64(len(call)), Data: json.RawMessage(`{"tool":"Agent","toolUseId":"call"}`)},
				{ID: "result", Kind: "tool-result", AgentID: caller, Timestamp: at.Add(time.Second), SourceOffset: int64(len(call)), SourceLength: int64(len(result)), Data: json.RawMessage(`{"toolUseId":"call"}`)},
			}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			children := 1
			if mode == "ambiguous" {
				children = 2
			}
			for i := 0; i < children; i++ {
				child := testSource()
				child.SourceID = fmt.Sprint("child-source-", i)
				if mode == "wrong-machine" {
					child.MachineID = "other-machine"
				}
				ingest(t, s, child, 0, "x\n")
				cb := batch(child, 0, 2)
				cb.Session.ID = fmt.Sprint("child-session-", i)
				cb.Session.NativeID = "unique-child-native"
				cb.Session.NativeAgentID = "child-agent"
				cb.Session.ParentThreadID = "root"
				if mode == "wrong-container" {
					cb.Session.ParentThreadID = "other-root"
				}
				if mode == "cache-only" {
					cb.Session.Completeness = "cache-only"
				}
				if err := s.CommitIndex(ctx, cb); err != nil {
					t.Fatal(err)
				}
			}
			spans, err := s.ToolSpans(ctx, b.Session.ID, 0, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ResolveDelegationChild(ctx, b.Session.ID, spans.Spans[0].Call.Sequence, spans.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "root" || mode == "nested" {
				if got.State != "resolved" || got.ChildSessionID != "child-session-0" || got.ChildAgentID != "child-agent" {
					t.Fatal(got)
				}
			} else {
				want := "child-not-indexed-or-parent-mismatch"
				if mode == "ambiguous" {
					want = "ambiguous-child"
				}
				if mode == "cache-only" {
					want = "child-evidence-incomplete"
				}
				if got.State != want || got.ChildSessionID != "" || got.ChildAgentID != "" {
					t.Fatal(got, want)
				}
			}
		})
	}
}
