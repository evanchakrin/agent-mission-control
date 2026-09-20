package desktop

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestMeasurementPreviewPreservesFilesAndAppliesReviewedEvidenceAfterRestart(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "topic": "model-tiering", "body": "original", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	d := items[0]
	calls := 0
	f.m.opts.Remeasure = func(context.Context) (Measurement, error) {
		calls++
		return Measurement{Body: "reviewed evidence", Sessions: 25, Subs: 250}, nil
	}
	before, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	preview := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "remeasure-preview", "id": d.ID, "expectedHash": viewDirective(d).StateHash})
	after, err := os.ReadFile(f.file)
	if err != nil || string(after) != string(before) {
		t.Fatal("preview edited file")
	}
	items, err = f.m.directiveItems()
	if err != nil || viewDirective(items[0]).StateHash != viewDirective(d).StateHash {
		t.Fatal("preview changed rule state")
	}
	if calls != 1 || preview["body"] != "reviewed evidence" {
		t.Fatal("wrong preview")
	}
	if err := f.m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// No provider is configured after restart. Applying must use durable evidence.
	body := map[string]any{"op": "remeasure", "id": d.ID, "key": preview["previewId"], "expectedHash": preview["stateHash"], "body": "untrusted replacement"}
	result := mustRequest(t, m, "POST", "/api/directives", body)
	if result["complete"] != true || result["body"] != "reviewed evidence" {
		t.Fatalf("wrong applied evidence: %+v", result)
	}
	after, err = os.ReadFile(f.file)
	if err != nil || !strings.Contains(string(after), "reviewed evidence") || strings.Contains(string(after), "untrusted replacement") {
		t.Fatal("reviewed evidence not applied")
	}
	if code, _ := request(t, m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("stale preview applied again: %d", code)
	}
	items, err = m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	body["expectedHash"] = viewDirective(items[0]).StateHash
	if code, _ := request(t, m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("preview rebound to different state: %d", code)
	}
}

func TestMeasurementPreviewRefusesUnavailableOrInvalidEvidence(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "topic": "model-tiering", "body": "original", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"op": "remeasure-preview", "id": items[0].ID}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 503 {
		t.Fatalf("missing provider hidden: %d", code)
	}
	for _, measurement := range []Measurement{{Body: "", Subs: 200}, {Body: "x", Sessions: -1, Subs: 200}, {Body: "x", Subs: 199}} {
		f.m.opts.Remeasure = func(context.Context) (Measurement, error) { return measurement, nil }
		if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 400 {
			t.Fatalf("invalid evidence accepted: %d", code)
		}
	}
}
