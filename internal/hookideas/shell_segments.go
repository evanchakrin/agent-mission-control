package hookideas

import (
	"regexp"
	"strings"
)

var heredocStart = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// WalkShellSegments mirrors the legacy Graveyard's conservative shell boundary
// rules. This is evidence extraction, not a shell parser or execution facility.
// It ignores separators inside simple quotes and leaves single pipes intact.
// Callbacks are synchronous; no collection grows with the number of segments.
// False indicates an unterminated quote/heredoc or unsupported compound heredoc.
func WalkShellSegments(command string, visit func(string)) bool {
	complete := true
	// Heredoc bodies are file content, never evidence of executed Git commands.
	for {
		match := heredocStart.FindStringSubmatchIndex(command)
		if match == nil {
			break
		}
		headerEnd := strings.IndexByte(command[match[1]:], '\n')
		if headerEnd >= 0 && heredocStart.MatchString(command[match[1]:match[1]+headerEnd]) {
			// Multiple redirections on one header require a real shell parser.
			// Do not expose their body text as executable segments.
			command = command[:match[0]]
			complete = false
			break
		}
		delimiter := command[match[2]:match[3]]
		end := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(delimiter) + `\s*$`)
		after := command[match[0]:]
		if stop := end.FindStringIndex(after); stop != nil {
			command = command[:match[0]] + after[stop[1]:]
		} else {
			command = command[:match[0]]
			complete = false
			break
		}
	}
	start, quote := 0, byte(0)
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				if i+1 < len(command) {
					i++
				}
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '\\' && i+1 < len(command) {
			i++ // An escaped separator is not a new shell command.
			continue
		}
		if c == '#' && (i == start || command[i-1] == ' ' || command[i-1] == '\t') {
			if text := strings.TrimSpace(command[start:i]); text != "" {
				visit(text)
			}
			end := strings.IndexByte(command[i:], '\n')
			if end < 0 {
				start = len(command)
				break
			}
			i += end
			start = i + 1
			continue
		}
		if c == '\n' || c == ';' {
			if text := strings.TrimSpace(command[start:i]); text != "" {
				visit(text)
			}
			start = i + 1
		} else if i+1 < len(command) && ((c == '&' && command[i+1] == '&') || (c == '|' && command[i+1] == '|')) {
			if text := strings.TrimSpace(command[start:i]); text != "" {
				visit(text)
			}
			i++
			start = i + 1
		}
	}
	if text := strings.TrimSpace(command[start:]); text != "" {
		visit(text)
	}
	return complete && quote == 0
}
