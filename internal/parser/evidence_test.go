package parser

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func parseFixtureLine(t *testing.T, provider, line string) Result {
	t.Helper()
	state, err := DecodeState(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Record(protocol.Source{MachineID: "machine", SourceID: "source", Generation: "g", Provider: provider, NativeID: "root"}, &state, 123, []byte(line))
}

func TestClaudePresenceDistinguishesOmittedInvalidAndZero(t *testing.T) {
	r := parseFixtureLine(t, "claude", `{"type":"assistant","agentId":"child","message":{"id":"m","usage":{"input_tokens":0,"output_tokens":7,"cache_read_input_tokens":null,"cache_creation_input_tokens":-3}}}`)
	if len(r.Usage) != 1 || r.Usage[0].TokensIn != 0 || !reflect.DeepEqual(r.Usage[0].Present, []string{"tokensIn", "tokensOut"}) || r.Usage[0].Kind != "message-partial" {
		t.Fatalf("presence lost: %+v", r.Usage)
	}
	if len(r.Events) != 2 {
		t.Fatalf("invalid fields not diagnosed: %+v", r.Events)
	}
	for _, line := range []string{
		`{"type":"assistant","message":{"id":"m","usage":{"input_tokens":9223372036854775808}}}`,
		`{"type":"assistant","message":{"id":"m","usage":{"input_tokens":1.2}}}`,
		`{"type":"assistant","message":{"id":"m","usage":{"input_tokens":"17"}}}`,
	} {
		if r := parseFixtureLine(t, "claude", line); len(r.Usage) != 0 || len(r.Events) != 1 {
			t.Fatal("invented numeric usage", r)
		}
	}
}

func TestCodexFirstCounterAndModelTransitionDoNotSmear(t *testing.T) {
	s := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", NativeID: "root"}
	state, _ := DecodeState(nil)
	state.Model = "a"
	a := Record(s, &state, 0, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":500,"output_tokens":100},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}}}`))
	if len(a.Usage) != 2 || a.Usage[0].Model != "" || a.Usage[0].TokensIn != 440 || a.Usage[0].TokensCache != 460 || a.Usage[0].TokensOut != 90 || a.Usage[1].Model != "a" || a.Usage[1].TokensIn != 60 {
		t.Fatalf("first total attributed to current model: %+v", a.Usage)
	}
	state.Model = "b"
	b := Record(s, &state, 500, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":550,"output_tokens":150},"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":20}}}}`))
	if len(b.Usage) != 2 || b.Usage[0].Model != "" || b.Usage[1].Model != "b" || b.Usage[1].TokensIn != 80 {
		t.Fatalf("transition residual smeared: %+v", b.Usage)
	}
	child := Record(s, &state, 1000, []byte(`{"type":"event_msg","payload":{"type":"token_count","thread_id":"child","info":{"total_token_usage":{"input_tokens":200,"output_tokens":10},"last_token_usage":{"input_tokens":200,"output_tokens":10}}}}`))
	if len(child.Usage) != 1 || child.Usage[0].Model != "" || child.Usage[0].AgentID != "child" {
		t.Fatalf("parent model used for child: %+v", child.Usage)
	}
}

func TestCodexDecreaseReplayAndContextEstimateDoNotInflate(t *testing.T) {
	s := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", NativeID: "root"}
	state, _ := DecodeState(nil)
	state.Model = "a"
	first := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10}}}}`
	Record(s, &state, 0, []byte(first))
	for i, line := range []string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50,"output_tokens":5}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":200000}}}}`,
	} {
		r := Record(s, &state, int64(i+1)*100, []byte(line))
		if len(r.Usage) != 0 || len(r.Events) != 1 || r.Events[0].Kind != "indexing-error" {
			t.Fatalf("decrease inflated usage: %+v", r)
		}
	}
	if r := Record(s, &state, 500, []byte(first)); len(r.Usage) != 0 {
		t.Fatal("replay counted prior interval twice", r)
	}
	if state.Counters["root"].Input != 100 {
		t.Fatal("invalid counter replaced high water")
	}
	r := parseFixtureLine(t, "codex", `{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{}}}`)
	if len(r.Events) != 1 || r.Events[0].Kind != "rate-limit-update" {
		t.Fatal("legitimate quota event called broken")
	}
}

func TestRawRangesSearchTextAndSafeDedup(t *testing.T) {
	s := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "claude"}
	state, _ := DecodeState(nil)
	text := strings.Repeat("界", 700) + " searchable-tail"
	line, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "uuid", "parentUuid": "parent", "isSidechain": true, "message": map[string]any{"id": "reply", "content": text}})
	a := Record(s, &state, 0, line)
	b := Record(s, &state, 9000, line)
	if len(a.Events) != 1 || a.Events[0].DedupeKey == "" || a.Events[0].DedupeKey != b.Events[0].DedupeKey || a.Events[0].ID == b.Events[0].ID {
		t.Fatal("raw location confused with native dedup")
	}
	if a.Events[0].SearchText != text || len([]rune(a.Events[0].Text)) != 501 || b.Events[0].SourceOffset != 9000 || b.Events[0].SourceLength != int64(len(line)) {
		t.Fatal("full search/raw record truncated")
	}
	if a.Events[0].AgentID == "main" || !strings.Contains(string(a.Events[0].Data), `"parentMessageId":"parent"`) {
		t.Fatal("sidechain ancestry discarded", a.Events[0])
	}
	changed := Record(s, &state, 12000, []byte(`{"type":"assistant","uuid":"uuid","message":{"content":"changed revision"}}`))
	if a.Events[0].DedupeKey == changed.Events[0].DedupeKey {
		t.Fatal("changed content hidden")
	}
	for _, provider := range []string{"codex", "claude"} {
		line := `{"type":"response_item","payload":{"type":"function_call","call_id":"call","name":"tool","arguments":"{\"n\":1}"}}`
		if provider == "claude" {
			line = `{"type":"assistant","uuid":"u","message":{"content":[{"type":"tool_use","id":"call","name":"tool","input":{"n":1}}]}}`
		}
		x := parseFixtureLine(t, provider, line)
		if len(x.Events) != 1 || x.Events[0].Kind != "tool-call" || x.Events[0].DedupeKey == "" || !strings.Contains(x.Events[0].SearchText, "n") {
			t.Fatal("tool lost", x)
		}
	}
	for _, line := range [][]byte{[]byte("null"), []byte("[]"), []byte("{} {}"), []byte("{broken"), []byte(strings.Repeat("x", MaxRecordBytes+1))} {
		r := Record(s, &state, 15000, line)
		if len(r.Events) != 1 || r.Events[0].Kind != "indexing-error" || r.Events[0].SourceLength != int64(len(line)) {
			t.Fatal("unparseable raw evidence missing", r)
		}
	}
}

func TestExistingSanitizedFixturesGoldenTotals(t *testing.T) {
	fixtures := []struct {
		pattern, provider           string
		input, cache, write, output int64
	}{
		{"../../tests/fixtures/claude/testproj/*.jsonl", "claude", 110, 1500, 70, 300},
		{"../../tests/fixtures/codex/*2026-01-01*.jsonl", "codex", 400, 1600, 0, 100},
		{"../../tests/fixtures/codex/*2026-01-02*.jsonl", "codex", 600000, 400000, 0, 100000},
		{"../../tests/fixtures/codex/*2026-01-03*.jsonl", "codex", 600000, 400000, 0, 100000},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.provider+fixture.pattern, func(t *testing.T) {
			paths, err := filepath.Glob(fixture.pattern)
			if err != nil || len(paths) != 1 {
				t.Fatal(paths, err)
			}
			f, err := os.Open(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			state, _ := DecodeState(nil)
			s := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: fixture.provider}
			observations := map[string]store.UsageObservation{}
			scan := bufio.NewScanner(f)
			offset := int64(0)
			for scan.Scan() {
				line := scan.Bytes()
				r := Record(s, &state, offset, line)
				offset += int64(len(line) + 1)
				for _, u := range r.Usage {
					observations[u.ID] = u
				}
			}
			if err := scan.Err(); err != nil {
				t.Fatal(err)
			}
			var input, cache, write, output int64
			for _, u := range observations {
				input += u.TokensIn
				cache += u.TokensCache
				write += u.TokensCacheWrite
				output += u.TokensOut
			}
			if input != fixture.input || cache != fixture.cache || write != fixture.write || output != fixture.output {
				t.Fatalf("golden mismatch: %d/%d/%d/%d", input, cache, write, output)
			}
		})
	}
}
