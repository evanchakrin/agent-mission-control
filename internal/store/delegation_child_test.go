package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCodexSpawnChildRequiresStructuredUnambiguousResult(t *testing.T) {
	wrap := func(output string) []byte {
		return []byte(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call","output":` + output + `}}`)
	}
	for _, output := range []string{`{"agent_id":"child"}`, `"{\"agent_id\":\"child\"}"`} {
		id, state := codexSpawnChild(wrap(output), "call")
		if id != "child" || state != "known" {
			t.Fatal(id, state)
		}
	}
	for _, output := range []string{`"agent_id: child"`, `{"agent_id":"one","agent_id":"two"}`, `{"agent_id":null}`, `{"agent_id":"\ud800"}`, `["child"]`} {
		id, state := codexSpawnChild(wrap(output), "call")
		if id != "" || state != "unsupported-result" {
			t.Fatal(id, state)
		}
	}
	if _, state := codexSpawnChild(wrap(`{"agent_id":"child"}`), "other"); state != "source-record-mismatch" {
		t.Fatal(state)
	}
	if _, state := codexSpawnChild([]byte(`{"type":"response_item","type":"response_item","payload":{}}`), "call"); state != "source-record-mismatch" {
		t.Fatal(state)
	}
}

func TestDelegationChildResolvesRawResultAndReciprocalIdentity(t *testing.T) {
	for _, mode := range []string{"resolved", "missing", "wrong-parent", "wrong-machine", "ambiguous", "ambiguous-parent", "duplicate-result", "cached-parent", "cached-child"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			src.Provider = "codex"
			src.NativeID = "parent-native"
			call := `{"type":"response_item","timestamp":"2026-09-07T00:00:00Z","payload":{"type":"function_call","name":"spawn_agent","call_id":"call","arguments":"{}"}}` + "\n"
			result := `{"type":"response_item","timestamp":"2026-09-07T00:00:01Z","payload":{"type":"function_call_output","call_id":"call","output":"{\"agent_id\":\"child-native\"}"}}` + "\n"
			ingest(t, s, src, 0, call+result)
			b := batch(src, 0, int64(len(call+result)))
			b.Session.NativeID = src.NativeID
			if mode == "cached-parent" {
				b.Session.Completeness = "cache-only"
			}
			at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			b.Events = []Event{
				{ID: "spawn", Kind: "tool-call", AgentID: "main", Timestamp: at, SourceLength: int64(len(call)), Data: json.RawMessage(`{"tool":"spawn_agent","toolUseId":"call"}`)},
				{ID: "result", Kind: "tool-result", AgentID: "main", Timestamp: at.Add(time.Second), SourceOffset: int64(len(call)), SourceLength: int64(len(result)), Text: "untrusted preview mentions a different child", Data: json.RawMessage(`{"toolUseId":"call"}`)},
			}
			if mode == "duplicate-result" {
				e := b.Events[1]
				e.ID = "duplicate"
				b.Events = append(b.Events, e)
			}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			children := 1
			if mode == "missing" {
				children = 0
			}
			if mode == "ambiguous" {
				children = 2
			}
			for i := 0; i < children; i++ {
				child := testSource()
				child.Provider = "codex"
				child.SourceID = fmt.Sprint("child-source-", i)
				child.NativeID = child.SourceID
				if mode == "wrong-machine" {
					child.MachineID = "other-machine"
				}
				ingest(t, s, child, 0, "x\n")
				cb := batch(child, 0, 2)
				cb.Session.ID = fmt.Sprint("child-session-", i)
				cb.Session.NativeID = "child-native"
				cb.Session.ParentThreadID = "parent-native"
				if mode == "cached-child" {
					cb.Session.Completeness = "cache-only"
				}
				if mode == "wrong-parent" {
					cb.Session.ParentThreadID = "someone-else"
				}
				if err := s.CommitIndex(ctx, cb); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ambiguous-parent" {
				copySource := testSource()
				copySource.Provider = "codex"
				copySource.SourceID = "parent-copy"
				ingest(t, s, copySource, 0, "x\n")
				copyBatch := batch(copySource, 0, 2)
				copyBatch.Session.ID = "parent-copy-session"
				copyBatch.Session.NativeID = "parent-native"
				if err := s.CommitIndex(ctx, copyBatch); err != nil {
					t.Fatal(err)
				}
			}
			var anchor int64
			if err := s.db.QueryRowContext(ctx, `SELECT seq FROM events WHERE id='spawn'`).Scan(&anchor); err != nil {
				t.Fatal(err)
			}
			link, err := s.ResolveDelegationChild(ctx, b.Session.ID, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			want := "child-not-indexed-or-parent-mismatch"
			if mode == "resolved" {
				want = "resolved"
			}
			if mode == "ambiguous" {
				want = "ambiguous-child"
			}
			if mode == "ambiguous-parent" {
				want = "ambiguous-parent"
			}
			if mode == "cached-parent" {
				want = "parent-evidence-incomplete"
			}
			if mode == "cached-child" {
				want = "child-evidence-incomplete"
			}
			if mode == "duplicate-result" {
				want = "call-result-ambiguous"
			}
			if link.State != want {
				t.Fatal(link, want)
			}
			if mode == "resolved" {
				if link.ChildSessionID != "child-session-0" || link.ChildAgentID != "main" || len(link.ChildSnapshot) != 64 {
					t.Fatal(link)
				}
			} else if link.ChildSessionID != "" || link.ChildSnapshot != "" || link.ChildAgentID != "" {
				t.Fatal("unproven child leaked", link)
			}
		})
	}
}
