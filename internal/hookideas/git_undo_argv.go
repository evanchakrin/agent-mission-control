package hookideas

import (
	"encoding/json"
	"strings"
)

// FindGitUndoArgv supports direct git and explicit POSIX shell -c/-lc calls.
// Other launchers stay unsupported; it never executes or expands an argument.
func FindGitUndoArgv(argv []string) (*GitUndo, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	bytes := 0
	for _, arg := range argv {
		bytes += len(arg) + 1
		if bytes > 8<<20 || strings.ContainsRune(arg, 0) {
			return nil, false
		}
	}
	switch argv[0] {
	case "sh", "bash", "/bin/sh", "/bin/bash", "/usr/bin/sh", "/usr/bin/bash":
		if len(argv) != 3 || (argv[1] != "-c" && argv[1] != "-lc") {
			return nil, false
		}
		return FindGitUndo(argv[2])
	case "git", "git.exe":
		// Display argv as JSON rather than inventing a shell command whose
		// quoting could change the meaning of the recorded invocation.
		display, err := json.Marshal(argv)
		if err != nil {
			return nil, false
		}
		return gitUndoWords(string(display), func(visit func(string)) bool {
			visit("git")
			for _, arg := range argv[1:] {
				visit(arg)
			}
			return true
		})
	default:
		return nil, false
	}
}
