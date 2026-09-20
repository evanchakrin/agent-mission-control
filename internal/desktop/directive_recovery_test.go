package desktop

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDirectiveRemeasureRecoversFileBeforeTargetCommit(t *testing.T) {
	f := newFixture(t)
	measurementCalls := 0
	measure := func(context.Context) (Measurement, error) {
		measurementCalls++
		if measurementCalls > 1 {
			return Measurement{Body: "different later measurement", Subs: 400, Sessions: 40}, nil
		}
		return Measurement{Body: "new measured body", Subs: 200, Sessions: 20}, nil
	}
	f.m.opts.Remeasure = measure
	f.opts.Remeasure = measure
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Measured rule", "topic": "model-tiering", "body": "previous measured body", "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	id := items[0].ID
	previousHash := directiveHash(directiveBlock(items[0]))
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_remeasure_target BEFORE INSERT ON audit WHEN NEW.kind='directive-remeasure-target' BEGIN SELECT RAISE(ABORT,'injected target completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": "remeasure", "id": id})
	if status != 500 {
		t.Fatalf("expected interrupted completion, got %d", status)
	}
	items, err = f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Body != "new measured body" || items[0].PendingMeasurement == nil || items[0].Targets[0].AppliedHash != previousHash || items[0].Targets[0].State != "pending" {
		t.Fatalf("lost applied/desired distinction: %+v", items[0])
	}
	content, err := os.ReadFile(f.file)
	if err != nil || !strings.Contains(string(content), "new measured body") {
		t.Fatal("test did not reach file replacement")
	}
	f.m.Close()
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.db.Exec("DROP TRIGGER fail_remeasure_target"); err != nil {
		t.Fatal(err)
	}
	var beforeWrites int
	if err := again.db.QueryRow("SELECT count(*) FROM file_operations").Scan(&beforeWrites); err != nil {
		t.Fatal(err)
	}
	result := mustRequest(t, again, "POST", "/api/directives", map[string]any{"op": "remeasure", "id": id})
	if result["results"].([]any)[0].(map[string]any)["status"] != "already" {
		t.Fatalf("replay did not recognize exact replacement: %+v", result)
	}
	items, err = again.directiveItems()
	if err != nil || items[0].PendingMeasurement != nil || measurementCalls != 1 || items[0].Targets[0].State != "planted" || items[0].Targets[0].AppliedHash != directiveHash(directiveBlock(items[0])) {
		t.Fatalf("target completion not recovered: %+v %v", items, err)
	}
	var afterWrites int
	if err := again.db.QueryRow("SELECT count(*) FROM file_operations").Scan(&afterWrites); err != nil || beforeWrites != afterWrites {
		t.Fatalf("retry rewrote completed file: %d/%d %v", beforeWrites, afterWrites, err)
	}
	mustRequest(t, again, "POST", "/api/directives", map[string]any{"op": "retire", "id": id})
	content, err = os.ReadFile(f.file)
	if err != nil || strings.Contains(string(content), "mission-control:directive:") || !strings.Contains(string(content), "Existing project guidance") {
		t.Fatal("retirement lost unrelated text or retained marker")
	}
}

func TestDirectiveTargetIntentSurvivesFailedCompletion(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_target_complete BEFORE INSERT ON audit WHEN NEW.kind='directive-target' BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Recovery", "body": "Keep this rule", "targets": []string{idFor(f.file)}})
	if status != 500 {
		t.Fatalf("completion failure status %d", status)
	}
	items, err := f.m.directiveItems()
	if err != nil || len(items) != 1 || len(items[0].Targets) != 1 {
		t.Fatalf("target intent missing: %+v %v", items, err)
	}
	d := items[0]
	if d.Targets[0].State != "pending" || d.Targets[0].Path != f.file {
		t.Fatalf("invalid intent: %+v", d)
	}
	content, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := markers(d.ID)
	if strings.Count(string(content), start) != 1 {
		t.Fatal("file mutation was not captured by test")
	}
	f.m.Close()
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.db.Exec("DROP TRIGGER fail_target_complete"); err != nil {
		t.Fatal(err)
	}
	mustRequest(t, again, "POST", "/api/directives", map[string]any{"op": "plant-existing", "id": d.ID, "targets": []string{idFor(f.file)}})
	after, err := again.directiveItems()
	if err != nil || len(after[0].Targets) != 1 || after[0].Targets[0].State != "planted" {
		t.Fatalf("recovery failed: %+v %v", after, err)
	}
	content, err = os.ReadFile(f.file)
	if err != nil || strings.Count(string(content), start) != 1 {
		t.Fatal("recovery duplicated marker")
	}
}

func TestDirectiveFailedTargetIntentDoesNotTouchFile(t *testing.T) {
	f := newFixture(t)
	before, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_target_intent BEFORE INSERT ON audit WHEN NEW.kind='directive-target-intent' BEGIN SELECT RAISE(ABORT,'injected intent failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "body": "Do not orphan", "targets": []string{idFor(f.file)}})
	if status != 500 {
		t.Fatalf("intent failure status %d", status)
	}
	after, err := os.ReadFile(f.file)
	if err != nil || string(after) != string(before) {
		t.Fatal("file changed without durable target intent")
	}
}
