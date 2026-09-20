package parser

import (
	"encoding/json"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestUserTitleSkipsOnlyLeadingKnownContext(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"<recommended_plugins>setup</recommended_plugins>", ""},
		{"<codex_internal_context source=\"goal\">continue</codex_internal_context>", ""},
		{"<environment_context>truncated", ""},
		{"<environment_context>x</environment_context>\n<in-app-browser-context source=\"ambient\">x</in-app-browser-context>\n## My request:\nFix the table", "Fix the table"},
		{"Explain <environment_context> tags", "Explain <environment_context> tags"},
		{"<environment_contextual>user text</environment_contextual>", "<environment_contextual>user text</environment_contextual>"},
		{"Fix the table", "Fix the table"},
		{"# AGENTS.md instructions for C:\\repo\n\n<INSTRUCTIONS>setup</INSTRUCTIONS>", ""},
		{"# AGENTS.md instructions for C:\\repo\r\n\r\n<INSTRUCTIONS>truncated", ""},
		{"# AGENTS.md instructions for /repo\n<INSTRUCTIONS>setup</INSTRUCTIONS>\n## My request:\nFix the archive", "Fix the archive"},
		{"# AGENTS.md instructions for /repo\nExplain what belongs here", "# AGENTS.md instructions for /repo\nExplain what belongs here"},
		{"Please update AGENTS.md instructions for this repo", "Please update AGENTS.md instructions for this repo"},
	} {
		if got := userTitle(tc.text); got != tc.want {
			t.Errorf("userTitle(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestTitleRepairPreservesUserEvents(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: provider}
			state, err := DecodeState(nil)
			if err != nil {
				t.Fatal(err)
			}
			state.Title = "# AGENTS.md instructions for C:\\repo\n\n<INSTRUCTIONS>old truncated preview"
			for i, text := range []string{"<environment_context>setup</environment_context>", "# AGENTS.md instructions for C:\\repo\n\n<INSTRUCTIONS>repository guidance</INSTRUCTIONS>", "Fix the archive table", "A later request"} {
				var record any
				if provider == "codex" {
					record = map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": text}}
				} else {
					record = map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
				}
				line, _ := json.Marshal(record)
				result := Record(source, &state, int64(i*1000), line)
				found := false
				for _, event := range result.Events {
					if event.Kind == "user-text" && event.SearchText == text {
						found = true
					}
				}
				if !found || len(result.Usage) != 0 {
					t.Fatal("title selection changed transcript events or usage", result)
				}
			}
			if state.Title != "Fix the archive table" {
				t.Fatalf("bad title %q", state.Title)
			}
		})
	}
}
