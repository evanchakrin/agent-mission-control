package hookideas

import "strings"

// GitUndo is a recorded command attempt, not proof that Git ran successfully or
// that files were lost. Display paths are bounded; PathCount includes all paths.
type GitUndo struct {
	Rule      string   `json:"rule"`
	Paths     []string `json:"paths"`
	PathCount int      `json:"pathCount"`
	Command   string   `json:"command"`
}

const MaxUndoPaths = 8
const MaxUndoPathBytes = 512
const MaxUndoCommandBytes = 440

// FindGitUndo recognizes the four legacy Graveyard command families. It never
// executes commands, expands variables, or infers success from a tool call.
// The boolean reports lexical completeness, not whether an undo was found.
func FindGitUndo(command string) (*GitUndo, bool) {
	if len(command) > 8<<20 {
		return nil, false
	}
	var found *GitUndo
	supported := true
	complete := WalkShellSegments(command, func(segment string) {
		hit, ok := gitUndoSegment(segment)
		supported = supported && ok
		if hit != nil && found == nil {
			found = hit
		}
	})
	if !complete || !supported {
		return nil, false
	}
	return found, true
}

func undoPreview(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && text[end]&0xc0 == 0x80 {
		end--
	}
	return text[:end] + "…"
}

func gitUndoSegment(segment string) (*GitUndo, bool) {
	return gitUndoWords(segment, func(visit func(string)) bool { return walkUndoWords(segment, visit) })
}

// Keep argv literals separate from shell lexing: dollars, spaces and operators
// inside an argv element are data, not expansions or command boundaries.
func gitUndoWords(display string, walk func(func(string)) bool) (*GitUndo, bool) {
	result := &GitUndo{Paths: []string{}}
	position := 0
	skipGlobal, skipSource, afterSep := false, false, false
	invalid, hard, recovery, staged := false, false, false, false
	addPath := func(word string) {
		result.PathCount++
		if len(result.Paths) < MaxUndoPaths {
			result.Paths = append(result.Paths, undoPreview(word, MaxUndoPathBytes))
		}
	}
	complete := walk(func(word string) {
		if invalid {
			return
		}
		if position == 0 {
			position++
			if word != "git" {
				invalid = true
			}
			return
		}
		if result.Rule == "" {
			if skipGlobal {
				skipGlobal = false
				return
			}
			switch word {
			case "-C", "-c", "--git-dir", "--work-tree", "--exec-path":
				skipGlobal = true
				return
			case "--no-pager", "--literal-pathspecs":
				return
			case "reset", "revert", "checkout", "restore":
				result.Rule = word
				return
			}
			for _, prefix := range []string{"-C=", "-c=", "--git-dir=", "--work-tree=", "--exec-path="} {
				if strings.HasPrefix(word, prefix) && len(word) > len(prefix) {
					return
				}
			}
			invalid = true
			return
		}
		if afterSep {
			addPath(word)
			return
		}
		if skipSource {
			skipSource = false
			return
		}
		if word == "--" {
			afterSep = true
			// Git's explicit path boundary takes precedence over bare arguments.
			result.Paths, result.PathCount = []string{}, 0
			return
		}
		switch word {
		case "--hard":
			hard = true
		case "--abort", "--continue", "--quit", "--skip":
			recovery = true
		case "--staged", "-S":
			staged = true
		case "--source", "-s":
			skipSource = true
			return
		}
		if result.Rule == "restore" && !strings.HasPrefix(word, "-") {
			addPath(word)
		}
	})
	if !complete {
		return nil, false
	}
	if invalid || skipGlobal || skipSource {
		return nil, true
	}
	switch result.Rule {
	case "reset":
		if !hard {
			return nil, true
		}
		result.Paths, result.PathCount = []string{}, 0
	case "revert":
		if recovery {
			return nil, true
		}
		result.Paths, result.PathCount = []string{}, 0
	case "checkout":
		if !afterSep || result.PathCount == 0 {
			return nil, true
		}
	case "restore":
		if staged || result.PathCount == 0 {
			return nil, true
		}
	default:
		return nil, true
	}
	result.Command = undoPreview(display, MaxUndoCommandBytes)
	return result, true
}

// Stream simple shell words rather than allocating an argument slice whose size
// scales with a command's path count. Complex shell operators are not inferred.
func walkUndoWords(text string, visit func(string)) bool {
	var word strings.Builder
	quote := byte(0)
	started := false
	emit := func() {
		if started {
			visit(word.String())
			word.Reset()
			started = false
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if quote == '"' && (c == '$' || c == '`') {
				return false // Do not label an expansion as a literal path.
			}
			if c == quote {
				quote = 0
				continue
			}
			if c == '\\' && quote == '"' && i+1 < len(text) && (text[i+1] == '"' || text[i+1] == '\\') {
				i++
				c = text[i]
			}
			word.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			started = true
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			emit()
			continue
		}
		if strings.ContainsRune("|&<>;`$(){}", rune(c)) {
			return false
		}
		if c == '#' && !started {
			break
		}
		started = true
		word.WriteByte(c)
	}
	emit()
	return quote == 0
}
