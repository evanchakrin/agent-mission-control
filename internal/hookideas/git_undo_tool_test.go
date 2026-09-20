package hookideas

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGitUndoToolRequiresRecognizedFullInput(t *testing.T) {
	for _, tt := range []struct{ tool, input, state, rule string }{
		{"Bash", `{"command":"git reset --hard"}`, "analyzed", "reset"},
		{"exec_command", `{"cmd":"git revert abc"}`, "analyzed", "revert"},
		{"functions.exec_command", `{"cmd":"git restore x"}`, "analyzed", "restore"},
		{"functions.shell_command", `{"command":"git checkout -- x"}`, "analyzed", "checkout"},
		{"Bash", `{"command":"echo 'git reset --hard'"}`, "analyzed", ""},
		{"Read", `{"command":"git reset --hard"}`, "not-shell", ""},
		{"functions.exec", `await tools.exec_command({cmd:"git reset --hard"})`, "unsupported-input", ""},
		{"Bash", `{"command":"git restore $file"}`, "incomplete-shell", ""},
		{"Bash", `{"command":"git restore \"$file\""}`, "incomplete-shell", ""},
		{"Bash", `{"command":null}`, "unsupported-input", ""},
		{"Bash", `{"command":["git","reset","--hard"]}`, "unsupported-input", ""},
		{"shell", `{"command":["git","reset","--hard"]}`, "analyzed", "reset"},
		{"functions.shell", `{"command":["bash","-lc","git restore -- x"]}`, "analyzed", "restore"},
		{"shell", `{"command":"git reset --hard"}`, "unsupported-input", ""},
		{"shell", `{"command":["bash","-lc","git restore $file"]}`, "unsupported-input", ""},
		{"Bash", `{"cmd":"git reset --hard"}`, "unsupported-input", ""},
		{"Bash", `{"command":"git reset --hard '"}`, "incomplete-shell", ""},
	} {
		hit, state := GitUndoToolCall(tt.tool, tt.input)
		if state != tt.state || tt.rule == "" && hit != nil || tt.rule != "" && (hit == nil || hit.Rule != tt.rule) {
			t.Fatal(tt, hit, state)
		}
	}
	full, _ := json.Marshal(map[string]string{"command": strings.Repeat("git status;", 1000) + "git reset --hard"})
	hit, state := GitUndoToolCall("Bash", string(full))
	if state != "analyzed" || hit == nil || hit.Rule != "reset" {
		t.Fatal(hit, state)
	}
	if hit, state := GitUndoToolCall("Bash", string(full[:500])+"…"); hit != nil || state != "unsupported-input" {
		t.Fatal(hit, state)
	}
}
