package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToolSpansMatchBeyondPageAndKeepAmbiguity(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(id, agent, kind, key string, sec int, errorValue any) {
		data, _ := json.Marshal(map[string]any{"toolUseId": key, "error": errorValue})
		b.Events = append(b.Events, Event{ID: id, AgentID: agent, Kind: kind, Timestamp: at.Add(time.Duration(sec) * time.Second), Data: data, SourceLength: 2})
	}
	add("call", "main", "tool-call", "a", 0, nil)
	add("other-agent", "child", "tool-result", "a", 2, true)
	add("no-id", "main", "tool-call", "", 0, nil)
	add("missing", "main", "tool-call", "missing", 0, nil)
	add("dup-call-1", "main", "tool-call", "dup-call", 0, nil)
	add("dup-call-2", "main", "tool-call", "dup-call", 0, nil)
	add("dup-call-result", "main", "tool-result", "dup-call", 1, false)
	add("dup-result-call", "main", "tool-call", "dup-result", 0, nil)
	add("dup-result-1", "main", "tool-result", "dup-result", 1, false)
	add("dup-result-2", "main", "tool-result", "dup-result", 2, false)
	add("reverse-call", "main", "tool-call", "reverse", 2, nil)
	add("reverse-result", "main", "tool-result", "reverse", 1, false)
	add("result", "main", "tool-result", "a", 5, true)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	// Copies in another source, old generation or parser revision are evidence,
	// but must not make the current correlation ambiguous.
	for _, scope := range []string{"source_id", "generation", "projection_revision"} {
		columns := []string{"session_id", "source_id", "generation", "agent_id", "kind", "timestamp", "source_offset", "raw_length", "text", "data", "dedupe_key", "projection_revision"}
		values := append([]string(nil), columns...)
		for i, column := range columns {
			if column == scope {
				values[i] = "'other-scope'"
			}
		}
		query := "INSERT INTO events(id," + strings.Join(columns, ",") + ") SELECT ?," + strings.Join(values, ",") + " FROM events WHERE id='result'"
		if _, err := s.db.ExecContext(ctx, query, "other-"+scope); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ToolSpans(ctx, "session-1", 0, 1, "")
	if err != nil || len(page.Spans) != 1 {
		t.Fatal(page, err)
	}
	span := page.Spans[0]
	if span.State != "matched" || span.DurationMillis == nil || *span.DurationMillis != 5000 || span.Error == nil || !*span.Error || span.ResultSequence == nil || *span.ResultSequence <= span.Call.Sequence {
		t.Fatal(span)
	}
	if page.NextSequence == 0 || page.Snapshot == "" {
		t.Fatal("missing bounded cursor", page)
	}
	next, err := s.ToolSpans(ctx, "session-1", page.NextSequence, 100, page.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, span := range next.Spans {
		states[span.Call.ID] = span.State
		if span.DurationMillis != nil {
			t.Fatal("invented duration", span)
		}
	}
	for id, want := range map[string]string{"no-id": "missing-id", "missing": "missing-result", "dup-call-1": "ambiguous", "dup-call-2": "ambiguous", "dup-result-call": "ambiguous", "reverse-call": "invalid-time-or-order"} {
		if states[id] != want {
			t.Fatal(id, states[id], want)
		}
	}
	if _, err = s.ToolSpans(ctx, "session-1", page.NextSequence, 100, ""); err == nil {
		t.Fatal("accepted unpinned continuation")
	}
	if _, err = s.ToolSpans(ctx, "session-1", 0, 101, ""); err == nil {
		t.Fatal("accepted oversized page")
	}
	if _, err = s.ToolSpans(ctx, "session-1", 0, 100, "stale"); err != ErrHistoryChanged {
		t.Fatal("stale snapshot", err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+toolMatchSQL, "session-1", "main", "a", "tool-result")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "events_tool_match") {
		t.Fatal("unindexed correlation", plan.String())
	}
}

func TestToolSpansUnknownOutcomeAndMissingTime(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, key := range []string{"zero", "no-time", "no-agent"} {
		agent := "main"
		if key == "no-agent" {
			agent = ""
		}
		start := at
		if key == "no-time" {
			start = time.Time{}
		}
		data, _ := json.Marshal(map[string]any{"toolUseId": key, "error": "unsupported"})
		b.Events = append(b.Events, Event{ID: key, AgentID: agent, Kind: "tool-call", Timestamp: start, Data: data, SourceLength: 2}, Event{ID: key + "-result", AgentID: agent, Kind: "tool-result", Timestamp: at, Data: data, SourceLength: 2})
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, err := s.ToolSpans(ctx, "session-1", 0, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range page.Spans {
		if span.Error != nil {
			t.Fatal("invented boolean outcome", span)
		}
		switch span.Call.ID {
		case "zero":
			if span.State != "matched" || span.DurationMillis == nil || *span.DurationMillis != 0 {
				t.Fatal(span)
			}
		case "no-time":
			if span.State != "invalid-time-or-order" || span.DurationMillis != nil {
				t.Fatal(span)
			}
		case "no-agent":
			if span.State != "missing-agent" || span.DurationMillis != nil {
				t.Fatal(span)
			}
		}
	}
}
