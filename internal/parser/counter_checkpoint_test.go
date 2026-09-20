package parser

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestDamagedCounterCheckpointsAreRejected(t *testing.T) {
	for name, counter := range map[string]Counter{
		"negative input":               {Set: true, Input: -1},
		"negative output":              {Set: true, Output: -1},
		"negative cache":               {Set: true, Cache: -1},
		"negative cache write":         {Set: true, CacheWrite: -1},
		"cache exceeds input":          {Set: true, Input: 2, Cache: 3},
		"combined cache exceeds input": {Set: true, Input: 3, Cache: 2, CacheWrite: 2},
		"total overflow":               {Set: true, Input: math.MaxInt64, Output: 1},
		"negative epoch":               {Set: true, Epoch: -1},
		"unset saved counter":          {Input: 10},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(State{Version: Version, Counters: map[string]Counter{"scope": counter}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeState(raw); err == nil || !strings.Contains(err.Error(), "inconsistent usage counter") {
				t.Fatalf("damaged counter accepted: %v", err)
			}
		})
	}
}

func TestValidCounterCheckpointResumesWithoutRecount(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex"}
	state, _ := DecodeState(nil)
	line := []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10}}}}`)
	first := Record(source, &state, 0, line)
	if len(first.Usage) != 1 {
		t.Fatal(first)
	}
	raw, _ := json.Marshal(state)
	restored, err := DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if repeated := Record(source, &restored, 500, line); len(repeated.Usage) != 0 {
		t.Fatal(repeated)
	}
}

func TestCounterOverflowWithoutTotalIsPreservedAsError(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex"}
	state, _ := DecodeState(nil)
	r := Record(source, &state, 0, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9223372036854775807,"output_tokens":1}}}}`))
	if len(r.Usage) != 0 || len(r.Events) != 1 || r.Events[0].Kind != "indexing-error" || len(state.Counters) != 0 {
		t.Fatal(r, state)
	}
}

func TestCounterDiscontinuityEpochDoesNotOverflow(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex"}
	state, _ := DecodeState(nil)
	state.Counters["s"] = Counter{Set: true, Input: 100, Epoch: math.MaxInt64}
	Record(source, &state, 0, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50,"output_tokens":0}}}}`))
	raw, _ := json.Marshal(state)
	if _, err := DecodeState(raw); err != nil {
		t.Fatal(err)
	}
	if state.Counters["s"].Epoch != math.MaxInt64 || state.Counters["s"].Input != 100 {
		t.Fatal(state)
	}
}
