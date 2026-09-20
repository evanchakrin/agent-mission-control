package hookideas_test

import (
	"encoding/json"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestCodexArgvUndoUsesRecordedArguments(t *testing.T) {
	arguments, _ := json.Marshal(map[string]any{"command": []string{"git", "restore", "--", "$literal name.js"}})
	record, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "function_call", "call_id": "fixture-call", "name": "shell", "arguments": string(arguments),
	}})
	state, _ := parser.DecodeState(nil)
	result := parser.Record(protocol.Source{MachineID: "fixture", SourceID: "argv", Generation: "g", Provider: "codex"}, &state, 0, record)
	found := 0
	for _, event := range result.Events {
		if event.Kind != "tool-call" {
			continue
		}
		var data struct {
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		hit, status := hookideas.GitUndoToolCall(data.Tool, event.SearchText)
		if status != "analyzed" || hit == nil || hit.Rule != "restore" || hit.PathCount != 1 || hit.Paths[0] != "$literal name.js" {
			t.Fatal(hit, status)
		}
		found++
	}
	if found != 1 || len(result.Usage) != 0 {
		t.Fatalf("tool evidence changed usage or disappeared: calls=%d usage=%d", found, len(result.Usage))
	}
}
