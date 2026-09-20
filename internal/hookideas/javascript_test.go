package hookideas

import (
	"fmt"
	"strings"
	"testing"
)

func TestJavaScriptEvidenceRequiresEditThenReportedJavaScriptError(t *testing.T) {
	for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit"} {
		t.Run(tool, func(t *testing.T) {
			var s JavaScriptSession
			s.Observe("tool-result", "", "app.js SyntaxError")
			s.Observe("tool-call", tool, `{"file_path":"app.JS"}`)
			s.Observe("assistant-text", "", "app.js SyntaxError")
			s.Observe("tool-result", "", "main.py SyntaxError")
			s.Observe("tool-result", "", "app.json Unexpected token")
			s.Observe("tool-result", "", "app.js completed")
			s.Observe("tool-result", "", "app.js SyntaxError: Unexpected token")
			if !s.edited || s.errors != 1 {
				t.Fatal(s)
			}
		})
	}
	var s JavaScriptSession
	s.Observe("tool-call", "Read", "app.js")
	s.Observe("tool-result", "", "app.js SyntaxError")
	if s.edited || s.errors != 0 {
		t.Fatal("read was counted as edit", s)
	}
}

func TestJavaScriptEvidenceUsesFullTextAndBoundedExamples(t *testing.T) {
	var e JavaScriptEvidence
	for i := 0; i < 100001; i++ {
		var s JavaScriptSession
		s.Observe("tool-call", "Edit", "app.mjs")
		for n := 0; n < i%3; n++ {
			s.Observe("tool-result", "", "app.mjs Unexpected identifier")
		}
		e.Add(fmt.Sprintf("session-%06d", i), s)
		if len(e.Examples) > MaxExamples {
			t.Fatal("unbounded examples")
		}
	}
	if !e.Proposed() || e.SessionsRead != 100001 || e.SessionsEdited != 100001 || e.SessionsWithErrors != 66667 || e.Errors != 100000 || len(e.Examples) != 6 {
		t.Fatal(e)
	}
	if e.Examples[0].SessionID != "session-000002" || e.Examples[5].SessionID != "session-000017" {
		t.Fatal(e.Examples)
	}
	var s JavaScriptSession
	s.Observe("tool-call", "Write", strings.Repeat("x", 20000)+` "late.cjs"`)
	s.Observe("tool-result", "", strings.Repeat("x", 20000)+" late.cjs Unexpected end of input")
	if !s.edited || s.errors != 1 {
		t.Fatal("full evidence was truncated", s)
	}
}

func TestJavaScriptProposalThreshold(t *testing.T) {
	var e JavaScriptEvidence
	e.Add("no-edits", JavaScriptSession{})
	for i := 0; i < 5; i++ {
		e.Add(fmt.Sprint(i), JavaScriptSession{edited: true, errors: 1})
		if e.Proposed() != (i == 4) {
			t.Fatal("threshold changed", e)
		}
	}
	if (JavaScriptEvidence{SessionsEdited: 50}).Proposed() {
		t.Fatal("invented evidence")
	}
}
