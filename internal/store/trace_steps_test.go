package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func traceFixture(arguments, call string) string {
	b, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "call_id": call, "name": "Read", "arguments": arguments}})
	return string(b) + "\n"
}

func TestTraceStepsRawBudgetContinuesWithoutDroppingRecords(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	src.Provider = "codex"
	record := traceFixture(`{"value":"`+strings.Repeat("x", 6<<20)+`"}`, "one")
	raw := strings.Repeat(record, 3)
	for offset := 0; offset < len(raw); offset += 1 << 20 {
		end := min(offset+(1<<20), len(raw))
		ingest(t, s, src, int64(offset), raw[offset:end])
	}
	b := batch(src, 0, int64(len(raw)))
	for i := 0; i < 3; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprint(i), Kind: "tool-call", AgentID: "main", SourceOffset: int64(i * len(record)), SourceLength: int64(len(record)), Data: json.RawMessage(`{"tool":"Read","toolUseId":"one"}`)})
	}
	ctx := context.Background()
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, err := s.TraceSteps(ctx, b.Session.ID, "main", 0, 100, "")
	if err != nil || len(page.Steps) != 2 || page.NextSequence == 0 {
		t.Fatal(page, err)
	}
	rest, err := s.TraceSteps(ctx, b.Session.ID, "main", page.NextSequence, 100, page.Snapshot)
	if err != nil || len(rest.Steps) != 1 || rest.NextSequence != 0 {
		t.Fatal(rest, err)
	}
	for _, step := range append(page.Steps, rest.Steps...) {
		if step.State != "known" {
			t.Fatal(step)
		}
	}
}
func TestTraceSignatureCompleteEvidence(t *testing.T) {
	arguments := `{"path":"` + strings.Repeat("long-prefix", 100) + `first"}`
	state, first := traceSignature("codex", []byte(traceFixture(arguments, "one")), "Read", "one")
	if state != "known" || len(first) != 64 {
		t.Fatal(state, first)
	}
	_, second := traceSignature("codex", []byte(traceFixture(strings.Replace(arguments, "first", "second", 1), "one")), "Read", "one")
	if first == second {
		t.Fatal("preview prefix compared as complete arguments")
	}
	_, spaced := traceSignature("codex", []byte(traceFixture(" \n "+arguments, "one")), "Read", "one")
	if first != spaced {
		t.Fatal("JSON formatting whitespace changed signature")
	}
	claude := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","id":"one","input":` + arguments + `}]}}`
	_, same := traceSignature("claude", []byte(claude), "Read", "one")
	if first != same {
		t.Fatal("same full arguments differ across providers")
	}
	for _, tc := range []struct{ provider, raw, tool, id, state string }{
		{"codex", traceFixture(arguments, "other"), "Read", "one", "source-record-mismatch"},
		{"codex", traceFixture(arguments, "one"), "Write", "one", "source-record-mismatch"},
		{"codex", traceFixture("not JSON", "one"), "Read", "one", "unsupported-arguments"},
		{"codex", traceFixture(arguments, "one"), "Read", "", "missing-call-identity"},
		{"claude", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","id":"one","input":{}},{"type":"tool_use","name":"Read","id":"one","input":{}}]}}`, "Read", "one", "source-record-mismatch"},
		{"otel", `{}`, "Read", "one", "unsupported-provider"},
	} {
		state, hash := traceSignature(tc.provider, []byte(tc.raw), tc.tool, tc.id)
		if state != tc.state || hash != "" {
			t.Fatal(state, hash, tc.state)
		}
	}
}

func TestTraceStepsBoundedAgentSourceAndSnapshots(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	src.Provider = "codex"
	raw := traceFixture(`{"path":"first"}`, "one")
	second := traceFixture(`{"path":"second"}`, "two")
	ingest(t, s, src, 0, raw+second)
	b := batch(src, 0, int64(len(raw+second)))
	b.Events = []Event{
		{ID: "one", Kind: "tool-call", AgentID: "main", Text: "same preview", SourceLength: int64(len(raw)), Data: json.RawMessage(`{"tool":"Read","toolUseId":"one"}`)},
		{ID: "two", Kind: "tool-call", AgentID: "main", Text: "same preview", SourceOffset: int64(len(raw)), SourceLength: int64(len(second)), Data: json.RawMessage(`{"tool":"Read","toolUseId":"two"}`)},
		{ID: "helper", Kind: "tool-call", AgentID: "helper", Text: "same preview", SourceLength: int64(len(raw)), Data: json.RawMessage(`{"tool":"Read","toolUseId":"one"}`)},
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, err := s.TraceSteps(ctx, b.Session.ID, "main", 0, 1, "")
	if err != nil || len(first.Steps) != 1 || first.NextSequence == 0 || first.Steps[0].State != "known" {
		t.Fatal(first, err)
	}
	next, err := s.TraceSteps(ctx, b.Session.ID, "main", first.NextSequence, 1, first.Snapshot)
	if err != nil || len(next.Steps) != 1 || next.NextSequence != 0 || next.Steps[0].Signature == first.Steps[0].Signature {
		t.Fatal(next, err)
	}
	helper, err := s.TraceSteps(ctx, b.Session.ID, "helper", 0, 100, "")
	if err != nil || len(helper.Steps) != 1 {
		t.Fatal(helper, err)
	}
	for _, limit := range []int{0, 101} {
		if _, err = s.TraceSteps(ctx, b.Session.ID, "main", 0, limit, ""); err != ErrInvalid {
			t.Fatal(err)
		}
	}
	if _, err = s.TraceSteps(ctx, b.Session.ID, "main", 1, 1, ""); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, err = s.TraceSteps(ctx, b.Session.ID, "main", 0, 1, "stale"); err != ErrHistoryChanged {
		t.Fatal(err)
	}
}
