package desktop

import (
	"fmt"
	"testing"
)

func TestDirectivePagesPreservePendingProvenanceAndSeparateRegistry(t *testing.T) {
	f := newFixture(t)
	items := []Directive{}
	for i := 54; i >= 0; i-- {
		items = append(items, Directive{ID: fmt.Sprintf("rule-%03d", i), Body: fmt.Sprintf("body-%d", i), Targets: []DirectiveTarget{{Path: f.file, State: "pending", AppliedHash: "fixture-hash"}}, PendingMeasurement: &Measurement{Body: "desired", Subs: 200}})
	}
	if err := f.m.save("directives", items, "fixture-import"); err != nil {
		t.Fatal(err)
	}
	after := ""
	seen := 0
	for {
		page := mustRequest(t, f.m, "GET", "/api/directives?limit=10&after="+after, nil)
		rows := page["items"].([]any)
		if len(rows) > 10 {
			t.Fatal("unbounded directive page")
		}
		for _, row := range rows {
			item := row.(map[string]any)
			if item["id"] != fmt.Sprintf("rule-%03d", seen) || item["body"] != fmt.Sprintf("body-%d", seen) || item["pendingMeasurement"] == nil {
				t.Fatalf("lost directive evidence: %+v", item)
			}
			seen++
		}
		next := page["nextCursor"].(string)
		if next == "" {
			break
		}
		if next == after {
			t.Fatal("stalled cursor")
		}
		after = next
	}
	if seen != 55 {
		t.Fatalf("only %d directives read", seen)
	}
	item := mustRequest(t, f.m, "GET", "/api/directives?id=rule-054", nil)["item"].(map[string]any)
	if item["targets"].([]any)[0].(map[string]any)["appliedHash"] != "fixture-hash" {
		t.Fatal("target provenance omitted")
	}
	registry := mustRequest(t, f.m, "GET", "/api/directives?registry=1", nil)
	if _, ok := registry["items"]; ok {
		t.Fatal("registry unnecessarily returned all rule bodies")
	}
	if registry["roots"] == nil || registry["targets"] == nil {
		t.Fatal("registry omitted roots or targets")
	}
	for _, path := range []string{"/api/directives?limit=21", "/api/directives?limit=0", "/api/directives?id="} {
		status, _ := request(t, f.m, "GET", path, nil)
		if status != 400 {
			t.Fatalf("invalid query returned %d", status)
		}
	}
}
