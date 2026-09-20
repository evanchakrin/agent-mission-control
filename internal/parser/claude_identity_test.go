package parser

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// Sanitized structural fixture from local Claude child headers: sessionId is
// shared with the parent, agentId differs, isSidechain=true, no parentSessionId.
func TestClaudeChildPayloadIdentitySeparatesSiblingsAndSurvivesRename(t *testing.T) {
	ids := map[string]bool{}
	for _, agent := range []string{"child-a", "child-b"} {
		source := protocol.Source{MachineID: "m", SourceID: agent, Generation: "g", Provider: "claude", Path: "old/subagents/file.jsonl"}
		state, _ := DecodeState(nil)
		line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "parent-session", "agentId": agent, "isSidechain": true, "message": map[string]any{"content": "Sanitized fixture"}})
		Record(source, &state, 0, line)
		if state.NativeID == "parent-session" || state.ParentThreadID != "parent-session" || state.ClaudeAgentID != agent || state.ClaudeIdentityScope != "agent" {
			t.Fatalf("child collapsed into parent: %+v", state)
		}
		if ids[state.NativeID] {
			t.Fatal("sibling identity collision")
		}
		ids[state.NativeID] = true
		oldNative, oldSession := state.NativeID, SessionID(source)
		line, _ = json.Marshal(map[string]any{"type": "attachment", "sessionId": "parent-session", "agentId": agent})
		Record(source, &state, 200, line)
		if state.NativeID != oldNative {
			t.Fatal("omitted sidechain flag retracted established identity")
		}
		raw, _ := json.Marshal(state)
		restored, err := DecodeState(raw)
		if err != nil {
			t.Fatal(err)
		}
		source.Path = "moved/renamed.jsonl"
		line, _ = json.Marshal(map[string]any{"type": "assistant", "sessionId": "parent-session", "agentId": agent, "isSidechain": true, "message": map[string]any{"id": "reply", "usage": map[string]any{"input_tokens": 10, "output_tokens": 3}}})
		result := Record(source, &restored, 500, line)
		if restored.NativeID != oldNative || SessionID(source) != oldSession || len(result.Usage) != 1 || result.Usage[0].AgentID != agent || result.Usage[0].TokensIn != 10 || result.Usage[0].TokensOut != 3 {
			t.Fatalf("rename/restart changed identity or usage: %+v %+v", restored, result)
		}
	}
}

func TestClaudeMixedIdentityIsVisibleAndDoesNotReadoptSourceHint(t *testing.T) {
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "claude", NativeID: "untrusted-hint"}
	state, _ := DecodeState(nil)
	Record(source, &state, 0, []byte(`{"type":"user","sessionId":"parent","agentId":"child-a","isSidechain":true}`))
	result := Record(source, &state, 100, []byte(`{"type":"user","sessionId":"parent","agentId":"child-b","isSidechain":true}`))
	if state.ClaudeIdentityScope != "ambiguous" || state.NativeID != "" || state.ParentThreadID != "" || len(result.Events) == 0 || result.Events[0].Kind != "indexing-error" {
		t.Fatalf("mixed child identity accepted: %+v %+v", state, result)
	}
	Record(source, &state, 200, []byte(`{"type":"user","sessionId":"parent","agentId":"child-a","isSidechain":true}`))
	if state.NativeID != "" {
		t.Fatal("source hint reintroduced an ambiguous alias")
	}
	root, _ := DecodeState(nil)
	Record(source, &root, 0, []byte(`{"type":"user","sessionId":"parent","isSidechain":false}`))
	Record(source, &root, 100, []byte(`{"type":"user","sessionId":"parent","agentId":"embedded-child","isSidechain":true}`))
	if root.NativeID != "parent" || root.ParentThreadID != "" || root.ClaudeIdentityScope != "root" {
		t.Fatalf("embedded agent reidentified the root: %+v", root)
	}
}

func TestClaudeIdentityChangeRequiresRebuildOfVersionTwo(t *testing.T) {
	if _, err := DecodeState(json.RawMessage(`{"version":"2","nativeId":"shared-parent"}`)); !errors.Is(err, ErrRebuildRequired) {
		t.Fatalf("old identity checkpoint continued without reindex: %v", err)
	}
}

func TestClaudeIdentityCheckpointRejectsContradictoryFields(t *testing.T) {
	for _, state := range []State{
		{Version: Version, ClaudeIdentityScope: "unknown"},
		{Version: Version, ClaudeIdentityScope: "agent", ClaudeSessionID: "parent", ClaudeAgentID: "child", NativeID: "parent"},
		{Version: Version, ClaudeIdentityScope: "root", ClaudeSessionID: "root", NativeID: "other"},
		{Version: Version, ClaudeIdentityScope: "ambiguous", ClaudeSessionID: "root", NativeID: "root"},
		{Version: Version, ClaudeAgentID: "orphan"},
	} {
		raw, _ := json.Marshal(state)
		if _, err := DecodeState(raw); err == nil {
			t.Fatalf("damaged identity accepted: %+v", state)
		}
	}
}
