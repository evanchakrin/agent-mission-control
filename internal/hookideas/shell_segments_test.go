package hookideas

import (
	"reflect"
	"strings"
	"testing"
)

func TestShellEvidenceSegmentsExcludeQuotedSeparatorsAndHeredocBody(t *testing.T) {
	for _, tt := range []struct {
		input    string
		want     []string
		complete bool
	}{
		{`grep 'reset --hard\|git revert' .`, []string{`grep 'reset --hard\|git revert' .`}, true},
		{`echo "git revert; git reset --hard" && git status`, []string{`echo "git revert; git reset --hard"`, `git status`}, true},
		{"cd repo && git status || git diff; git log\ngit show", []string{"cd repo", "git status", "git diff", "git log", "git show"}, true},
		{"cat <<'EOF'\ngit revert abc\ngit reset --hard\nEOF\ngit status", []string{"cat", "git status"}, true},
		{"cat <<EOF\ngit reset --hard", []string{"cat"}, false},
		{"cat <<A\ngit reset --hard\nA\ncat <<B\ngit revert abc\nB\ngit status", []string{"cat", "cat", "git status"}, true},
		{"cat <<A <<B\ntext\nA\ngit reset --hard\nB", []string{"cat"}, false},
		{`echo 'unfinished; git reset --hard`, []string{`echo 'unfinished; git reset --hard`}, false},
		{`printf x | git status`, []string{`printf x | git status`}, true},
		{`echo "a\"; b" && git status`, []string{`echo "a\"; b"`, `git status`}, true},
	} {
		t.Run(tt.input, func(t *testing.T) {
			var got []string
			complete := WalkShellSegments(tt.input, func(s string) { got = append(got, s) })
			if complete != tt.complete || !reflect.DeepEqual(got, tt.want) {
				t.Fatal(got, complete)
			}
		})
	}
}

func TestShellEvidenceWalkDoesNotCollectSegments(t *testing.T) {
	count := 0
	if !WalkShellSegments(strings.Repeat("git status;", 100001), func(s string) {
		if s != "git status" {
			t.Fatal(s)
		}
		count++
	}) || count != 100001 {
		t.Fatal(count)
	}
}
