package parser

import (
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"testing"
)

func TestIndependentCountersModelsResetsAndZeroOutput(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", NativeID: "root"}
	state, _ := DecodeState(nil)
	lines := []string{
		`{"type":"session_meta","payload":{"id":"root","model":"a"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":0},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":0}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":0}}}}`,
		`{"type":"turn_context","payload":{"model":"b"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":50,"output_tokens":10},"last_token_usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":10}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","thread_id":"child","model":"c","info":{"total_token_usage":{"input_tokens":20,"output_tokens":5},"last_token_usage":{"input_tokens":20,"output_tokens":5}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":1}}}}`,
	}
	var in, cache, out int64
	models := map[string]int64{}
	count := 0
	for i, line := range lines {
		r := Record(source, &state, int64(i*1000), []byte(line))
		for _, u := range r.Usage {
			in += u.TokensIn
			cache += u.TokensCache
			out += u.TokensOut
			models[u.Model] += u.TokensIn + u.TokensCache
			count++
		}
	}
	if in != 120 || cache != 50 || out != 15 || count != 3 {
		t.Fatalf("wrong independent totals %d/%d/%d (%d observations)", in, cache, out, count)
	}
	if models["a"] != 100 || models["b"] != 50 || models["c"] != 20 {
		t.Fatalf("usage smeared across models: %v", models)
	}
	raw, _ := json.Marshal(state)
	restored, err := DecodeState(raw)
	if err != nil || restored.Counters["root"].Epoch != 1 {
		t.Fatal("checkpoint lost reset epoch", err)
	}
}
func TestClaudeUsageRevisionAndMalformedEvidence(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "claude"}
	state, _ := DecodeState(nil)
	a := Record(source, &state, 0, []byte(`{"type":"assistant","uuid":"a","message":{"id":"reply","model":"model","usage":{"input_tokens":10,"output_tokens":0},"content":"hello"}}`))
	b := Record(source, &state, 150, []byte(`{"type":"assistant","uuid":"b","message":{"id":"reply","model":"model","usage":{"input_tokens":10,"output_tokens":20},"content":"world"}}`))
	if len(a.Usage) != 1 || len(b.Usage) != 1 || a.Usage[0].ID != b.Usage[0].ID || b.Usage[0].TokensOut != 20 {
		t.Fatal("reply revision must upsert one observation")
	}
	for _, line := range []string{`{bad}`, `null`, `{} {}`} {
		r := Record(source, &state, 300, []byte(line))
		if len(r.Events) != 1 || r.Events[0].Kind != "indexing-error" {
			t.Fatalf("malformed input not visible: %s", line)
		}
	}
}
func TestIdentityIgnoresRenameNotMachine(t *testing.T) {
	a := protocol.Source{MachineID: "m", SourceID: "s", Provider: "codex", Path: "before"}
	b := a
	b.Path = "after"
	if SessionID(a) != SessionID(b) {
		t.Fatal("rename changed session")
	}
	b.MachineID = "other"
	if SessionID(a) == SessionID(b) {
		t.Fatal("machine collision")
	}
}
