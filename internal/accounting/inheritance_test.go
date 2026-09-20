package accounting

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestForkBaselineMatchRequiresExplicitRelationshipAndExactEvidence(t *testing.T) {
	makeObservation := func(id string) (store.Session, store.UsageObservation) {
		source := protocol.Source{MachineID: "machine", SourceID: id, Generation: "g", Provider: "codex", NativeID: id}
		state, _ := parser.DecodeState(nil)
		row := parser.Record(source, &state, 123, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":4037031565,"cached_input_tokens":3964972452,"cache_write_input_tokens":959413,"output_tokens":8997712,"total_tokens":4046029277}}}}`))
		if len(row.Usage) != 1 {
			t.Fatal(row)
		}
		return store.Session{ID: parser.SessionID(source), SourceID: id, Generation: "g", MachineID: "machine", Provider: "codex", NativeID: id}, row.Usage[0]
	}
	child, first := makeObservation("child")
	parent, parentUsage := makeObservation("parent")
	child.ForkedFromID = parent.NativeID
	// A different spawning parent must not overwrite the explicit history origin.
	child.ParentThreadID = "spawning-agent"
	beforeFirst, beforeParent := first, parentUsage
	match, ok := MatchForkBaseline(child, parent, first, parentUsage)
	if !ok || match.TokensIn+match.TokensCache+match.TokensCacheWrite+match.TokensOut != 4046029277 || match.ChildObservationID != first.ID || match.ParentObservationID != parentUsage.ID || match.ChildOffset != 123 {
		t.Fatal("exact shared baseline not identified", match, ok)
	}
	if !reflect.DeepEqual(first, beforeFirst) || !reflect.DeepEqual(parentUsage, beforeParent) {
		t.Fatal("evidence mutated")
	}
	for _, name := range []string{"no-fork", "different-fork", "different-machine", "same-source", "same-native", "wrong-provider", "wrong-session", "wrong-scope", "wrong-agent", "different-cache", "delta", "split", "missing-evidence", "missing-previous", "negative-offset", "invalid-counter", "noninitial"} {
		t.Run(name, func(t *testing.T) {
			c, p, a, b := child, parent, first, parentUsage
			var evidence counterEvidence
			if err := json.Unmarshal(a.Evidence, &evidence); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "no-fork":
				c.ForkedFromID = ""
				c.ParentThreadID = p.NativeID
			case "different-fork":
				c.ForkedFromID = "other"
			case "different-machine":
				p.MachineID = "other"
			case "same-source":
				p.SourceID = c.SourceID
			case "same-native":
				c.NativeID = p.NativeID
			case "wrong-provider":
				p.Provider = "claude"
			case "wrong-session":
				a.SessionID = p.ID
			case "wrong-scope":
				b.CounterScope = "thread:other"
			case "wrong-agent":
				b.AgentID = "child-agent"
			case "different-cache":
				evidence.Counter.Cache--
			case "delta":
				a.Kind = "cumulative-delta"
			case "split":
				a.TokensIn--
			case "missing-evidence":
				a.Evidence = nil
			case "missing-previous":
				evidence.Previous = nil
			case "negative-offset":
				*evidence.Offset = -1
			case "invalid-counter":
				evidence.Counter.Output = -1
			case "noninitial":
				evidence.Previous.Set = true
			}
			if name != "missing-evidence" {
				a.Evidence, _ = json.Marshal(evidence)
			}
			if match, ok := MatchForkBaseline(c, p, a, b); ok {
				t.Fatal("unverified baseline accepted", match)
			}
		})
	}
}
