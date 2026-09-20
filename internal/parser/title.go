package parser

import "strings"

// Title selection is presentation-only: source bytes, emitted events, usage,
// and checkpoint offsets are unchanged. Existing v7 checkpoints remain valid;
// an old setup-only title can be repaired when a real user message arrives.
func selectUserTitle(state *State, text string) {
	if existing := userTitle(state.Title); existing != "" {
		state.Title = existing
		return
	}
	state.Title = userTitle(text)
}

func userTitle(text string) string {
	text = strings.TrimSpace(text)
	stripped := false
	for {
		// Codex emits repository guidance as a user-role setup message. Match
		// the actual wrapper, not ordinary requests discussing AGENTS.md.
		if strings.HasPrefix(text, "# AGENTS.md instructions for ") {
			if newline := strings.IndexByte(text, '\n'); newline >= 0 {
				body := strings.TrimSpace(text[newline+1:])
				if strings.HasPrefix(body, "<INSTRUCTIONS>") {
					end := strings.Index(body, "</INSTRUCTIONS>")
					if end < 0 {
						return ""
					}
					text = strings.TrimSpace(body[end+len("</INSTRUCTIONS>"):])
					stripped = true
					continue
				}
			}
		}
		matched := false
		for _, tag := range []string{"environment_context", "recommended_plugins", "codex_internal_context", "in-app-browser-context"} {
			prefix := "<" + tag
			if !strings.HasPrefix(text, prefix) || len(text) == len(prefix) {
				continue
			}
			if !strings.ContainsRune(" >\t\r\n", rune(text[len(prefix)])) {
				continue
			}
			end := strings.Index(text, "</"+tag+">")
			if end < 0 {
				// A truncated metadata preview is not a usable chat title.
				return ""
			}
			text = strings.TrimSpace(text[end+len(tag)+3:])
			matched, stripped = true, true
			break
		}
		if !matched {
			break
		}
	}
	if stripped {
		text = strings.TrimSpace(strings.TrimPrefix(text, "## My request:"))
	}
	return preview(text)
}
