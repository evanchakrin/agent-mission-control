package hookideas

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGitUndoRecordedAttempts(t *testing.T) {
	for _, tt := range []struct {
		command, rule string
		paths         []string
	}{
		{`git reset --hard HEAD~1`, "reset", []string{}},
		{`cd repo && git -C "folder with spaces" --no-pager reset --hard`, "reset", []string{}},
		{`git -c advice.detachedHead=false --git-dir=.git revert abc`, "revert", []string{}},
		{`git checkout HEAD -- "a b.js" c.js`, "checkout", []string{"a b.js", "c.js"}},
		{`git restore --source HEAD~1 "a b.js" c.js`, "restore", []string{"a b.js", "c.js"}},
		{`git restore -s HEAD -- --staged`, "restore", []string{"--staged"}},
		{`git restore --source=HEAD -- file.js`, "restore", []string{"file.js"}},
		{"git status\ngit restore file.js\ngit reset --hard", "restore", []string{"file.js"}},
		{"cat <<EOF\ngit reset --hard\nEOF\ngit revert abc", "revert", []string{}},
		{"git status # example; git reset --hard\ngit restore file.js", "restore", []string{"file.js"}},
	} {
		t.Run(tt.command, func(t *testing.T) {
			hit, complete := FindGitUndo(tt.command)
			if !complete || hit == nil || hit.Rule != tt.rule || !reflect.DeepEqual(hit.Paths, tt.paths) || hit.PathCount != len(tt.paths) {
				t.Fatal(hit, complete)
			}
		})
	}
}

func TestGitUndoDoesNotInventAttemptsFromExamplesOrRecovery(t *testing.T) {
	for _, command := range []string{
		`grep 'reset --hard\|git revert' .`, `echo "git reset --hard; git revert abc"`,
		`git revert --abort`, `git revert --continue`, `git revert --skip`, `git revert --quit`,
		`git restore --staged file.js`, `git restore -S file.js`, `git reset --soft HEAD`,
		`git checkout feature`, `git restore`, `git checkout --`, `git reset -- --hard`,
		`git --unknown reset --hard`, `echo hi | git reset --hard`,
		`git status # example; git reset --hard`, `# git status; git revert abc`,
		`echo hi\; git reset --hard`,
		"cat <<'EOF'\ngit reset --hard\ngit revert abc\nEOF",
	} {
		if hit, _ := FindGitUndo(command); hit != nil {
			t.Fatalf("invented attempt for %q: %+v", command, hit)
		}
	}
	for _, command := range []string{`git reset --hard "unfinished`, "git reset --hard; cat <<EOF\nbody"} {
		if hit, complete := FindGitUndo(command); complete || hit != nil {
			t.Fatal(hit, complete)
		}
	}
}

func TestGitUndoBoundsExamplesNotPathCounts(t *testing.T) {
	hit, complete := FindGitUndo("git restore -- " + strings.Repeat("file.js ", 100001))
	if !complete || hit == nil || hit.PathCount != 100001 || len(hit.Paths) != MaxUndoPaths || len(hit.Command) > MaxUndoCommandBytes+3 {
		t.Fatal("unbounded or lost paths", complete)
	}
	hit, complete = FindGitUndo(`git restore -- "` + strings.Repeat("界", 300) + `"`)
	if !complete || hit == nil || !utf8.ValidString(hit.Paths[0]) || len(hit.Paths[0]) > MaxUndoPathBytes+3 {
		t.Fatal(hit, complete)
	}
}
