package hookideas

import "encoding/json"

// GitUndoToolCall consumes a full normalized tool-call input. Unknown wrappers
// and unsupported input shapes remain explicit, never interpreted as zero undo
// evidence. Callers must not pass display previews or assistant/tool-result prose.
func GitUndoToolCall(tool, fullText string) (*GitUndo, string) {
	field := ""
	switch tool {
	case "Bash", "shell_command", "functions.shell_command":
		field = "command"
	case "exec_command", "functions.exec_command":
		field = "cmd"
	case "shell", "functions.shell":
		field = "command"
	case "functions.exec", "exec":
		return nil, "unsupported-input" // JavaScript wrappers need their own adapter
	default:
		return nil, "not-shell"
	}
	if len(fullText) > 8<<20 {
		return nil, "unsupported-input"
	}
	var input map[string]json.RawMessage
	if json.Unmarshal([]byte(fullText), &input) != nil {
		return nil, "unsupported-input"
	}
	var command string
	value, ok := input[field]
	if tool == "shell" || tool == "functions.shell" {
		var argv []string
		if !ok || json.Unmarshal(value, &argv) != nil {
			return nil, "unsupported-input"
		}
		hit, complete := FindGitUndoArgv(argv)
		if !complete {
			return nil, "unsupported-input"
		}
		return hit, "analyzed"
	}
	if !ok || string(value) == "null" || json.Unmarshal(value, &command) != nil {
		return nil, "unsupported-input"
	}
	hit, complete := FindGitUndo(command)
	if !complete {
		return nil, "incomplete-shell"
	}
	return hit, "analyzed"
}
