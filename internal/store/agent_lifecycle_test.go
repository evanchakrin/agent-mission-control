package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestAgentLifecycleEvidenceTransitions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		events         []string
		retry, stalled string
		pending        bool
		uncertain      int64
	}{
		{"pending", []string{"call:a"}, "false", "true", true, 0},
		{"failed", []string{"call:a", "failed:a"}, "true", "false", false, 0},
		{"retry-issued", []string{"call:a", "failed:a", "call:b"}, "false", "true", true, 0},
		{"success", []string{"call:a", "failed:a", "call:b", "success:b"}, "false", "false", false, 0},
		{"unknown-result", []string{"call:a", "unknown:a"}, "unknown", "false", false, 0},
		{"missing-id", []string{"call:"}, "false", "unknown", false, 1},
		{"duplicate", []string{"call:a", "call:a"}, "false", "unknown", false, 1},
		{"duplicate-result", []string{"call:a", "failed:a", "failed:a"}, "unknown", "unknown", false, 1},
		{"orphan-error", []string{"failed:a"}, "unknown", "unknown", false, 1},
		{"result-before-call", []string{"success:a", "call:a"}, "false", "unknown", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			src := testSource()
			src.ModifiedAt = now.Add(-time.Minute)
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			for i, item := range tc.events {
				var kind, key string
				for j, c := range item {
					if c == ':' {
						kind, key = item[:j], item[j+1:]
						break
					}
				}
				data := map[string]any{"tool": "Edit", "toolUseId": key}
				eventKind := "tool-result"
				switch kind {
				case "call":
					eventKind = "tool-call"
				case "success":
					data["error"] = false
				case "failed":
					data["error"] = true
				}
				raw, _ := json.Marshal(data)
				b.Events = append(b.Events, Event{ID: fmt.Sprintf("event-%d", i), AgentID: "main", Kind: eventKind, Timestamp: now.Add(-5*time.Minute + time.Duration(i)*time.Second), Data: raw, SourceLength: 2})
			}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			out, err := s.AgentLifecycle(ctx, "session-1", "main", "", now)
			state := func(v *bool) string {
				if v == nil {
					return "unknown"
				}
				return fmt.Sprint(*v)
			}
			if err != nil || state(out.Retrying) != tc.retry || state(out.Stalled) != tc.stalled || (out.Pending != nil) != tc.pending || out.UncertainCalls != tc.uncertain {
				t.Fatalf("%+v retry=%s stalled=%s err=%v", out, state(out.Retrying), state(out.Stalled), err)
			}
			if tc.pending {
				quiet, err := s.AgentLifecycle(ctx, "session-1", "main", out.Snapshot, now.Add(time.Hour))
				if err != nil || quiet.Stalled == nil || *quiet.Stalled || quiet.Pending == nil {
					t.Fatal("old inactivity was labelled a current stall", quiet, err)
				}
			}
		})
	}
}

func TestAgentLifecycleKeepsAgentAndProjectionBoundaries(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Now().UTC()
	src := testSource()
	src.ModifiedAt = now
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{
		{ID: "call", AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-3 * time.Minute), SourceLength: 2, Data: json.RawMessage(`{"tool":"Edit","toolUseId":"same"}`)},
		{ID: "child-result", AgentID: "child", Kind: "tool-result", Timestamp: now.Add(-time.Minute), SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"same","error":false}`)},
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"source_id", "generation", "projection_revision"} {
		// Deliberately add an old-scope result only in this disposable fixture.
		fields := map[string]string{"source_id": "source_id", "generation": "generation", "projection_revision": "projection_revision"}
		fields[column] = "'old'"
		_, err := s.db.Exec(`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision)
 SELECT ?,session_id,`+fields["source_id"]+`,`+fields["generation"]+`,agent_id,'tool-result',timestamp,0,2,'','{"toolUseId":"same","error":false}','',`+fields["projection_revision"]+` FROM events WHERE id='call'`, "old-"+column)
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.AgentLifecycle(ctx, "session-1", "main", "", now)
	if err != nil || out.Pending == nil || out.Pending.ID != "call" || out.Stalled == nil || !*out.Stalled || out.UncertainCalls != 0 {
		t.Fatal(out, err)
	}
}
