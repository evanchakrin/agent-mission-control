// Package hookideas evaluates recorded evidence, never installs executable hooks.
package hookideas

import (
	"regexp"
	"sort"
	"strings"
)

const MinimumJavaScriptSessions = 5
const MaxExamples = 6

var javascriptPath = regexp.MustCompile(`(?i)\.(?:js|mjs|cjs)(?:["'\s]|$)`)

// JavaScriptSession consumes one session in source-event order. Full text must
// come from indexed search evidence or raw parsing, not a display preview. The
// caller owns completeness and snapshot validation before publishing the result.
// It retains counters only, so memory does not grow with transcript size.
type JavaScriptSession struct {
	edited bool
	errors int64
}

func (s *JavaScriptSession) Observe(kind, tool, fullText string) {
	if kind == "tool-call" {
		switch tool {
		case "Edit", "Write", "MultiEdit", "NotebookEdit":
			if javascriptPath.MatchString(fullText) {
				s.edited = true
			}
		}
		return
	}
	if !s.edited || kind != "tool-result" || !javascriptPath.MatchString(fullText) {
		return
	}
	for _, phrase := range []string{"SyntaxError", "Unexpected token", "Unexpected identifier", "Unexpected end of input", "Invalid or unexpected token"} {
		if strings.Contains(fullText, phrase) {
			s.errors++ // One result is one observation even if it contains several phrases.
			return
		}
	}
}

type JavaScriptExample struct {
	SessionID string `json:"sessionId"`
	Errors    int64  `json:"errors"`
}

// JavaScriptEvidence includes only complete, snapshot-validated sessions supplied
// by its caller. Examples are bounded; totals include every supplied session.
type JavaScriptEvidence struct {
	SessionsRead       int64               `json:"sessionsRead"`
	SessionsEdited     int64               `json:"sessionsEdited"`
	SessionsWithErrors int64               `json:"sessionsWithErrors"`
	Errors             int64               `json:"errors"`
	Examples           []JavaScriptExample `json:"examples"`
}

func (e *JavaScriptEvidence) Add(sessionID string, s JavaScriptSession) {
	e.SessionsRead++
	if !s.edited {
		return
	}
	e.SessionsEdited++
	if s.errors == 0 {
		return
	}
	e.SessionsWithErrors++
	e.Errors += s.errors
	e.Examples = append(e.Examples, JavaScriptExample{SessionID: sessionID, Errors: s.errors})
	sort.Slice(e.Examples, func(i, j int) bool {
		if e.Examples[i].Errors != e.Examples[j].Errors {
			return e.Examples[i].Errors > e.Examples[j].Errors
		}
		return e.Examples[i].SessionID < e.Examples[j].SessionID
	})
	if len(e.Examples) > MaxExamples {
		e.Examples = e.Examples[:MaxExamples]
	}
}

func (e JavaScriptEvidence) Proposed() bool {
	return e.SessionsEdited >= MinimumJavaScriptSessions && e.SessionsWithErrors > 0
}
