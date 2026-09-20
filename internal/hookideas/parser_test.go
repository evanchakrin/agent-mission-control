package hookideas_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestClaudeHookEvidenceConsumesFullNormalizedText(t *testing.T) {
	state, err := parser.DecodeState(nil)
	if err != nil {
		t.Fatal(err)
	}
	src := protocol.Source{MachineID: "fixture", SourceID: "fixture", Generation: "g", Provider: "claude", NativeID: "root"}
	resultText, _ := json.Marshal(strings.Repeat("ordinary output ", 2000) + "app.js SyntaxError")
	lines := []string{
		`{"type":"assistant","uuid":"edit","message":{"id":"edit","content":[{"type":"tool_use","id":"edit-tool","name":"Edit","input":{"file_path":"app.js"}}]}}`,
		`{"type":"user","uuid":"result","message":{"content":[{"type":"tool_result","tool_use_id":"edit-tool","content":` + string(resultText) + `}]}}`,
	}
	var full, preview hookideas.JavaScriptSession
	var offset int64
	for _, line := range lines {
		r := parser.Record(src, &state, offset, []byte(line))
		offset += int64(len(line))
		for _, event := range r.Events {
			var data struct {
				Tool string `json:"tool"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			full.Observe(event.Kind, data.Tool, event.SearchText)
			preview.Observe(event.Kind, data.Tool, event.Text)
		}
	}
	var completeEvidence, truncatedEvidence hookideas.JavaScriptEvidence
	completeEvidence.Add("fixture", full)
	truncatedEvidence.Add("fixture", preview)
	if completeEvidence.SessionsEdited != 1 || completeEvidence.Errors != 1 {
		t.Fatal(completeEvidence)
	}
	if truncatedEvidence.Errors != 0 {
		t.Fatal("fixture no longer exercises preview truncation", truncatedEvidence)
	}
}

func TestGitUndoAdaptersConsumeFullProviderToolCalls(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			command := strings.Repeat("git status;", 1000) + "git restore -- source.js"
			var record any
			if provider == "claude" {
				record = map[string]any{"type": "assistant", "uuid": "fixture", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tool", "name": "Bash", "input": map[string]string{"command": command}}}}}
			} else {
				arguments, _ := json.Marshal(map[string]string{"cmd": command})
				record = map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "call_id": "tool", "name": "exec_command", "arguments": string(arguments)}}
			}
			line, _ := json.Marshal(record)
			state, _ := parser.DecodeState(nil)
			result := parser.Record(protocol.Source{MachineID: "fixture", SourceID: "source", Generation: "g", Provider: provider}, &state, 0, line)
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
				if status != "analyzed" || hit == nil || hit.Rule != "restore" || hit.PathCount != 1 || hit.Paths[0] != "source.js" {
					t.Fatal(hit, status)
				}
				if hit, status := hookideas.GitUndoToolCall(data.Tool, event.Text); hit != nil || status != "unsupported-input" {
					t.Fatal("preview treated as full evidence", hit, status)
				}
				found++
			}
			if found != 1 {
				t.Fatal("fixture did not produce exactly one tool call", found)
			}
		})
	}
}
