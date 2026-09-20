package desktop

import (
	"os"
	"strings"
	"testing"
)

func TestRevisionBoundRetirementPreservesChangedRulesAndFiles(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Rule", "body": "original", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	d := items[0]
	body := map[string]any{"op": "retire", "id": d.ID, "expectedHash": viewDirective(d).StateHash}
	items[0].Title = "Changed title"
	if err := f.m.save("directives", items, "fixture"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("stale retirement accepted: %d", code)
	}
	after, err := os.ReadFile(f.file)
	if err != nil || string(after) != string(before) {
		t.Fatal("stale retirement changed file")
	}
	// A fresh registry hash still cannot authorize overwriting external file edits.
	body["expectedHash"] = viewDirective(items[0]).StateHash
	damaged := strings.ReplaceAll(string(before), "original", "externally changed")
	if err := os.WriteFile(f.file, []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	partial := mustRequest(t, f.m, "POST", "/api/directives", body)
	if partial["removed"] != false || partial["results"].([]any)[0].(map[string]any)["status"] != "error" {
		t.Fatalf("hidden conflict: %+v", partial)
	}
	if _, exists := partial["items"]; exists {
		t.Fatal("compact response returned entire library")
	}
	after, err = os.ReadFile(f.file)
	if err != nil || string(after) != damaged {
		t.Fatal("external edit overwritten")
	}
	if err := os.WriteFile(f.file, before, 0600); err != nil {
		t.Fatal(err)
	}
	items, err = f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	body["expectedHash"] = viewDirective(items[0]).StateHash
	result := mustRequest(t, f.m, "POST", "/api/directives", body)
	if result["removed"] != true {
		t.Fatalf("retirement failed: %+v", result)
	}
	after, err = os.ReadFile(f.file)
	if err != nil || strings.Contains(string(after), "mission-control:directive:") || !strings.Contains(string(after), "Existing project guidance") {
		t.Fatalf("unrelated guidance not preserved: %s %v", after, err)
	}
}
