package desktop

import (
	"os"
	"strings"
	"testing"
)

func TestDirectiveBoundsDistinguishesMalformedAndModifiedBlocks(t *testing.T) {
	d := Directive{ID: "fixture", Title: "Rule", Body: "original", CreatedAt: 1}
	block := directiveBlock(d)
	start, end := markers(d.ID)
	for _, tc := range []struct{ name, content, state string }{
		{"intact", "before" + block + "after", "intact"},
		{"crlf", strings.ReplaceAll(block, "\n", "\r\n"), "intact"},
		{"edited", strings.ReplaceAll(block, "original", "edited"), "modified"},
		{"missing-end", start + "text", "malformed"},
		{"orphan-end", end, "malformed"},
		{"reversed", end + start, "malformed"},
		{"duplicate", block + block, "malformed"},
		{"absent", "unrelated", "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, state := directiveBounds(tc.content, d)
			if state != tc.state {
				t.Fatalf("got %s want %s", state, tc.state)
			}
		})
	}
}

func TestMalformedDirectiveCannotBeRetiredOrReplantedSilently(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Rule", "body": "original", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	d := items[0]
	start, _ := markers(d.ID)
	damaged := "User text\n" + start + "\nIncomplete rule"
	if err := os.WriteFile(f.file, []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	checked := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "check", "id": d.ID})
	if checked["statuses"].([]any)[0].(map[string]any)["status"] != "malformed" {
		t.Fatal("incomplete block reported healthy")
	}
	for _, op := range []string{"plant-existing", "retire"} {
		result := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": op, "id": d.ID, "targets": []string{idFor(f.file)}})
		if result["results"].([]any)[0].(map[string]any)["status"] != "error" {
			t.Fatalf("%s did not expose error", op)
		}
		content, err := os.ReadFile(f.file)
		if err != nil || string(content) != damaged {
			t.Fatalf("%s modified incomplete block", op)
		}
	}
	items, err = f.m.directiveItems()
	if err != nil || len(items) != 1 || len(items[0].Targets) != 1 {
		t.Fatalf("retirement forgot unresolved target: %+v %v", items, err)
	}
}
