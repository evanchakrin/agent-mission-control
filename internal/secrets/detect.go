// Package secrets recognizes the legacy sentinel's narrow credential shapes.
// It does not validate credentials, read files, contact vendors, or log input.
package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

const MaxBytes = 512 * 1024
const MaxFindings = 5

type Finding struct {
	Kind     string `json:"kind"`
	Line     int    `json:"line"`
	Fragment string `json:"fragment"`
}
type Result struct {
	State    string    `json:"state"`
	Findings []Finding `json:"findings"`
	Capped   bool      `json:"capped"`
}

type rule struct {
	kind    string
	pattern *regexp.Regexp
	confirm func([]byte) bool
}

var rules = []rule{
	{"An Amazon Web Services key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), nil},
	{"A GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36}\b`), nil},
	{"A Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9]{10,48}-[A-Za-z0-9]{10,48}(?:-[A-Za-z0-9]{10,64})?\b`), nil},
	{"A Stripe live payment key", regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{24,64}\b`), nil},
	{"A Google API key", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}\b`), nil},
	{"A private key", regexp.MustCompile(`-----BEGIN (?:RSA |DSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----[\r\n]+(?:[A-Za-z0-9+/=]{40,}[\r\n]+)+`), nil},
	{"A signed login token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), signedHeader},
}
var placeholder = regexp.MustCompile(`(?i)EXAMPLE|SAMPLE|PLACEHOLDER|REPLACE|CHANGE[-_ ]?ME|INSERT[-_ ]?YOUR|DUMMY|REDACT|NOT[-_ ]?REAL|FAKE|YOUR[-_]|<|abcdef|1234567890`)

func isPlaceholder(data []byte) bool {
	if placeholder.Match(data) {
		return true
	}
	for i := 5; i < len(data); i++ {
		if data[i] == data[i-1] && data[i] == data[i-2] && data[i] == data[i-3] && data[i] == data[i-4] && data[i] == data[i-5] {
			return true
		}
	}
	return false
}
func signedHeader(token []byte) bool {
	i := bytes.IndexByte(token, '.')
	if i < 0 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(token[:i]))
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var header struct {
		Algorithm string `json:"alg"`
	}
	return json.Unmarshal(raw, &header) == nil && strings.TrimSpace(header.Algorithm) != "" && !strings.EqualFold(header.Algorithm, "none")
}

// Scan retains only a kind, line number and four-character prefix per finding.
// Capped is conservative: reaching the bound means more findings may exist.
// No "scanned" result establishes the absence of other credential formats.
func Scan(data []byte) Result {
	out := Result{State: "scanned", Findings: []Finding{}}
	if len(data) > MaxBytes {
		out.State = "too-large"
		return out
	}
	if bytes.IndexByte(data, 0) >= 0 {
		out.State = "binary"
		return out
	}
	for _, r := range rules {
		seen := map[int]bool{}
		line, lineOffset := 1, 0
		for offset := 0; offset < len(data); {
			loc := r.pattern.FindIndex(data[offset:])
			if loc == nil {
				break
			}
			start, end := offset+loc[0], offset+loc[1]
			offset = end
			// Searching a suffix must not invent a new word boundary.
			if start > 0 && r.kind != "A private key" && wordByte(data[start-1]) {
				continue
			}
			// Bound placeholder context without repeatedly scanning a long line.
			lineStart := max(0, start-120)
			if n := bytes.LastIndexByte(data[lineStart:start], '\n'); n >= 0 {
				lineStart += n + 1
			}
			contextEnd := min(len(data), end+120)
			lineEnd := bytes.IndexByte(data[start:contextEnd], '\n')
			if lineEnd < 0 {
				lineEnd = contextEnd
			} else {
				lineEnd += start
			}
			match := data[start:end]
			around := data[max(lineStart, start-120):min(lineEnd, end+120)]
			if isPlaceholder(match) || isPlaceholder(around) || (r.confirm != nil && !r.confirm(match)) {
				continue
			}
			line += bytes.Count(data[lineOffset:start], []byte{'\n'})
			lineOffset = start
			if seen[line] {
				continue
			}
			seen[line] = true
			out.Findings = append(out.Findings, Finding{Kind: r.kind, Line: line, Fragment: string(match[:4]) + "…"})
			if len(out.Findings) == MaxFindings {
				out.Capped = true
				return out
			}
		}
	}
	return out
}
func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}
