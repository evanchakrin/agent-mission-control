package accounting

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestForkBaselineGroupPreservesSplitAttribution(t *testing.T) {
	var child, parent store.Session
	var first []store.UsageObservation
	var parentUsage store.UsageObservation
	for _, id := range []string{"child", "parent"} {
		src := protocol.Source{MachineID: "m", SourceID: id, Generation: "g", Provider: "codex", NativeID: id}
		state, _ := parser.DecodeState(nil)
		if id == "child" {
			state.Model = "model-a"
		}
		result := parser.Record(src, &state, 123, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10},"last_token_usage":{"input_tokens":20,"cached_input_tokens":10,"output_tokens":2}}}}`))
		session := store.Session{ID: parser.SessionID(src), MachineID: "m", SourceID: id, Generation: "g", Provider: "codex", NativeID: id}
		if id == "child" {
			child = session
			first = result.Usage
		} else {
			parent = session
			parentUsage = result.Usage[0]
		}
	}
	child.ForkedFromID = parent.NativeID
	if len(first) != 2 {
		t.Fatal("fixture did not exercise split", first)
	}
	before, _ := json.Marshal(first)
	match, ok := MatchForkBaselineGroup(child, parent, first, parentUsage)
	if !ok || len(match.ChildObservationIDs) != 2 || match.ChildObservationID != "" || match.TokensIn != 50 || match.TokensCache != 50 || match.TokensOut != 10 {
		t.Fatal(match, ok)
	}
	reverse := []store.UsageObservation{first[1], first[0]}
	other, ok := MatchForkBaselineGroup(child, parent, reverse, parentUsage)
	if !ok || !reflect.DeepEqual(match, other) {
		t.Fatal("evidence depends on hash ordering", match, other)
	}
	after, _ := json.Marshal(first)
	if string(before) != string(after) {
		t.Fatal("observation attribution mutated")
	}
	for _, name := range []string{"missing-piece", "duplicate-id", "different-offset", "wrong-model", "overcount", "negative", "overflow", "third-piece"} {
		t.Run(name, func(t *testing.T) {
			rows := append([]store.UsageObservation(nil), first...)
			switch name {
			case "missing-piece":
				rows = rows[:1]
			case "duplicate-id":
				rows[1].ID = rows[0].ID
			case "different-offset":
				var e counterEvidence
				json.Unmarshal(rows[1].Evidence, &e)
				*e.Offset++
				rows[1].Evidence, _ = json.Marshal(e)
			case "wrong-model":
				rows[1].Model = ""
			case "overcount":
				rows[1].TokensOut++
			case "negative":
				rows[1].TokensOut = -1
			case "overflow":
				rows[1].TokensIn = math.MaxInt64
			case "third-piece":
				rows = append(rows, rows[0])
			}
			if result, ok := MatchForkBaselineGroup(child, parent, rows, parentUsage); ok {
				t.Fatal("invalid group accepted", result)
			}
		})
	}
}
