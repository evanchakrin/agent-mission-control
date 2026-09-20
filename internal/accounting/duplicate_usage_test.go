package accounting

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestDuplicateCodexUsageMatchesCopiedTransitionsWithoutMutatingEvidence(t *testing.T) {
	var sessions []store.Session
	var observations [][]store.UsageObservation
	for i, sourceID := range []string{"original", "replacement"} {
		source := protocol.Source{MachineID: "m", SourceID: sourceID, Generation: "g", Provider: "codex", NativeID: "thread"}
		state, _ := parser.DecodeState(nil)
		state.Model = "model-a"
		var usage []store.UsageObservation
		for j, record := range []string{
			`{"timestamp":"2026-09-01T01:02:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10},"last_token_usage":{"input_tokens":20,"cached_input_tokens":10,"output_tokens":2}}}}`,
			`{"timestamp":"2026-09-01T01:02:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"cached_input_tokens":60,"output_tokens":12}}}}`,
		} {
			result := parser.Record(source, &state, int64(100+i*20+j*400), []byte(record))
			usage = append(usage, result.Usage...)
		}
		sessions = append(sessions, store.Session{ID: parser.SessionID(source), MachineID: "m", SourceID: sourceID, Generation: "g", ProjectionRevision: sourceID + "-revision", Provider: "codex", NativeID: "thread"})
		observations = append(observations, usage)
	}
	if len(observations[0]) != 3 || len(observations[1]) != 3 {
		t.Fatal("fixture must cover residual, reported last call, and subsequent delta")
	}
	before, _ := json.Marshal(observations)
	for i := range observations[0] {
		match, ok := MatchDuplicateCodexUsage(sessions[0], sessions[1], observations[0][i], observations[1][i])
		if !ok || match.LeftOffset == match.RightOffset || match.LeftRevision != sessions[0].ProjectionRevision || match.RightRevision != sessions[1].ProjectionRevision {
			t.Fatal(i, match, ok)
		}
		reverse, ok := MatchDuplicateCodexUsage(sessions[1], sessions[0], observations[1][i], observations[0][i])
		if !ok || reverse.EvidenceHash != match.EvidenceHash {
			t.Fatal("semantic identity depends on copy selection", reverse)
		}
	}
	after, _ := json.Marshal(observations)
	if string(before) != string(after) {
		t.Fatal("source observations changed")
	}
	for _, name := range []string{"machine", "native", "provider", "source", "session", "agent", "scope", "model", "tokens", "negative", "overflow", "timestamp", "missing-time", "offset", "previous-counter", "last-counter", "future-field", "large-integer"} {
		t.Run(name, func(t *testing.T) {
			left, right := sessions[0], sessions[1]
			a, b := observations[0][2], observations[1][2]
			var evidence map[string]json.RawMessage
			if err := json.Unmarshal(b.Evidence, &evidence); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "machine":
				right.MachineID = "elsewhere"
			case "native":
				right.NativeID = "independent-child"
			case "provider":
				right.Provider = "claude"
			case "source":
				right.SourceID = left.SourceID
			case "session":
				b.SessionID = "unrelated"
			case "agent":
				b.AgentID = "child"
			case "scope":
				b.CounterScope = "thread:other"
			case "model":
				b.Model = "model-b"
			case "tokens":
				b.TokensCache++
			case "negative":
				a.TokensOut, b.TokensOut = -1, -1
			case "overflow":
				a.TokensIn, b.TokensIn = math.MaxInt64, math.MaxInt64
			case "timestamp":
				b.Timestamp = b.Timestamp.Add(time.Second)
			case "missing-time":
				a.Timestamp, b.Timestamp = time.Time{}, time.Time{}
			case "offset":
				evidence["offset"] = json.RawMessage(`-1`)
			case "previous-counter":
				evidence["previousCounter"] = json.RawMessage(`null`)
			case "last-counter":
				evidence["lastCounter"] = json.RawMessage(`{"Set":true,"Input":1}`)
			case "future-field":
				evidence["future"] = json.RawMessage(`true`)
			case "large-integer":
				var other map[string]json.RawMessage
				json.Unmarshal(a.Evidence, &other)
				other["future"] = json.RawMessage(`9007199254740992`)
				a.Evidence, _ = json.Marshal(other)
				evidence["future"] = json.RawMessage(`9007199254740993`)
			}
			b.Evidence, _ = json.Marshal(evidence)
			if match, ok := MatchDuplicateCodexUsage(left, right, a, b); ok || !reflect.DeepEqual(match, DuplicateUsageMatch{}) {
				t.Fatal("unproven repeated usage matched", name, match)
			}
		})
	}
}
