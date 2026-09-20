package hookideas

import (
	"reflect"
	"strings"
	"testing"
)

func TestGitUndoArgvPreservesLiteralPaths(t *testing.T) {
	paths := []string{"space name.js", "$literal", "a;b", "quote'file", `a\b`, "--staged"}
	argv := append([]string{"git.exe", "restore", "--"}, paths...)
	hit, ok := FindGitUndoArgv(argv)
	if !ok || hit == nil || hit.Rule != "restore" || hit.PathCount != len(paths) || !reflect.DeepEqual(hit.Paths, paths) {
		t.Fatal(hit, ok)
	}
	if !strings.HasPrefix(hit.Command, `["git.exe","restore"`) {
		t.Fatal("argv display must not impersonate shell text", hit.Command)
	}
}

func TestGitUndoArgvScope(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		ok   bool
		rule string
	}{
		{[]string{"git", "reset", "--hard"}, true, "reset"},
		{[]string{"git", "-C", "repo with spaces", "checkout", "--", "x"}, true, "checkout"},
		{[]string{"git", "revert", "abc"}, true, "revert"},
		{[]string{"git", "revert", "--abort"}, true, ""},
		{[]string{"git", "restore", "--staged", "x"}, true, ""},
		{[]string{"git", "status"}, true, ""},
		{[]string{"/bin/bash", "-lc", "git status; git restore -- x"}, true, "restore"},
		{[]string{"sh", "-c", "echo 'git reset --hard'"}, true, ""},
		{[]string{"bash", "-lc", "git restore $file"}, false, ""},
		{[]string{"bash", "-lc", "git reset --hard", "extra"}, false, ""},
		{[]string{"bash", "script.sh"}, false, ""},
		{[]string{"powershell.exe", "-Command", "git reset --hard"}, false, ""},
		{[]string{"cmd.exe", "/c", "git reset --hard"}, false, ""},
		{[]string{"env", "git", "reset", "--hard"}, false, ""},
		{[]string{"git", "restore", "x\x00y"}, false, ""},
		{nil, false, ""},
	} {
		hit, ok := FindGitUndoArgv(tc.argv)
		if ok != tc.ok || tc.rule == "" && hit != nil || tc.rule != "" && (hit == nil || hit.Rule != tc.rule) {
			t.Fatalf("%q: hit=%+v complete=%v", tc.argv, hit, ok)
		}
	}
}

func TestGitUndoArgvBounds(t *testing.T) {
	argv := []string{"git", "restore", "--"}
	for range 10001 {
		argv = append(argv, "file.js")
	}
	hit, ok := FindGitUndoArgv(argv)
	if !ok || hit == nil || hit.PathCount != 10001 || len(hit.Paths) != MaxUndoPaths || len(hit.Command) > MaxUndoCommandBytes+3 {
		t.Fatal(hit, ok)
	}
	if hit, ok := FindGitUndoArgv([]string{"git", "restore", strings.Repeat("x", 8<<20)}); ok || hit != nil {
		t.Fatal("oversized argv accepted")
	}
}
