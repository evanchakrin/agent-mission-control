package secrets

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Constructed nonfunctional fixtures, never credentials from a user's files.
func TestNarrowShapesAndRedactedResults(t *testing.T) {
	tail := strings.Repeat("Q7mZ2kP9", 8)
	values := []string{"AKIAQ7MZ2KP9R4NT6VW8", "ghp_" + tail[:36], "xoxb-" + tail[:12] + "-" + tail[:12], "sk_live_" + tail[:24], "AIza" + tail[:35], "-----BEGIN PRIVATE KEY-----\n" + tail[:48] + "\n", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + "." + tail[:16] + "." + tail[:16]}
	for i, value := range values {
		got := Scan([]byte("first line\n" + value + "\n"))
		if got.State != "scanned" || len(got.Findings) != 1 || got.Findings[0].Line != 2 || got.Findings[0].Kind != rules[i].kind {
			t.Fatalf("rule %d failed shape/line contract", i)
		}
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), value) || got.Findings[0].Fragment != value[:4]+"…" {
			t.Fatalf("rule %d exposed more than its prefix", i)
		}
		if len(Scan([]byte("EXAMPLE "+value)).Findings) != 0 {
			t.Fatalf("rule %d did not suppress contextual placeholder", i)
		}
	}
}

func TestScanBoundsAndNonClaims(t *testing.T) {
	if Scan(make([]byte, MaxBytes+1)).State != "too-large" || Scan([]byte("text\x00binary")).State != "binary" {
		t.Fatal("input bounds failed")
	}
	key := "AKIAQ7MZ2KP9R4NT6VW8"
	got := Scan([]byte(strings.Repeat(key+"\n", 20)))
	if len(got.Findings) != MaxFindings || !got.Capped {
		t.Fatal("finding bound failed")
	}
	if len(Scan([]byte(key+" "+key)).Findings) != 1 {
		t.Fatal("same rule and line duplicated")
	}
	for _, text := range []string{"x" + key, "AKIAAAAAAAAAAAAAAAAA", "-----BEGIN PRIVATE KEY-----", "password=something-random", "1.2.3", "ghp_" + strings.Repeat("a", 36)} {
		if len(Scan([]byte(text)).Findings) != 0 {
			t.Fatal("unsupported or placeholder input matched")
		}
	}
	for _, head := range []string{`{"alg":"none"}`, `{"alg":""}`, `{}`, `{"alg":42}`, `{"alg":"NONE"}`} {
		token := base64.RawURLEncoding.EncodeToString([]byte(head)) + ".Q7mZ2kP9Q7mZ2kP9.Q7mZ2kP9Q7mZ2kP9"
		if len(Scan([]byte(token)).Findings) != 0 {
			t.Fatal("invalid signed-token header accepted")
		}
	}
}

func TestRepeatedMatchesStayBoundedAndKeepLineNumbers(t *testing.T) {
	key := "AKIAQ7MZ2KP9R4NT6VW8"
	line := strings.Repeat(key+" ", 20000)
	got := Scan([]byte(line + "\n\n" + key))
	if len(got.Findings) != 2 || got.Findings[0].Line != 1 || got.Findings[1].Line != 3 {
		t.Fatal("repeated same-line matches changed line attribution")
	}
}
