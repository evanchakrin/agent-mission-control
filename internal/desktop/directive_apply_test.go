package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyExistingRuleUsesObservedStateAndOneNewTarget(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "body": "Existing rule body", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	d := items[0]
	target := filepath.Join(f.repo, "AGENTS.md")
	if err := os.WriteFile(target, []byte("Original Codex guidance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"op": "plant-existing", "id": d.ID, "expectedHash": viewDirective(d).StateHash, "targets": []string{idFor(target)}}
	result := mustRequest(t, f.m, "POST", "/api/directives", body)
	if _, ok := result["items"]; ok {
		t.Fatal("returned entire registry")
	}
	if result["id"] != d.ID || result["results"].([]any)[0].(map[string]any)["status"] != "planted" {
		t.Fatalf("apply failed: %+v", result)
	}
	content, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(content), "Original Codex guidance") || strings.Count(string(content), "Existing rule body") != 1 {
		t.Fatalf("target damaged: %s %v", content, err)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("stale target state accepted: %d", code)
	}
	items, err = f.m.directiveItems()
	if err != nil || len(items) != 1 || len(items[0].Targets) != 2 {
		t.Fatal("lost stable rule or targets")
	}
	body["expectedHash"] = viewDirective(items[0]).StateHash
	result = mustRequest(t, f.m, "POST", "/api/directives", body)
	if result["results"].([]any)[0].(map[string]any)["status"] != "already" {
		t.Fatalf("repeat rewrote target: %+v", result)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(content) {
		t.Fatal("repeat changed file bytes")
	}
}
