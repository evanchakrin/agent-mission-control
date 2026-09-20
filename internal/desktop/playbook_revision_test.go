package desktop

import "testing"

func TestPlaybookRevisionsRejectStaleWritesAndDeletes(t *testing.T) {
	f := newFixture(t)
	created := mustRequest(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "save", "name": "Original", "body": "old", "expectedRevision": 0})
	item := created["items"].([]any)[0].(map[string]any)
	id := item["id"].(string)
	if item["revision"] != float64(1) {
		t.Fatalf("new revision: %+v", item)
	}
	mustRequest(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "save", "id": id, "name": "Current", "body": "new", "expectedRevision": 1})
	var before int
	if err := f.m.db.QueryRow("SELECT count(*) FROM audit").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"save", "delete"} {
		status, _ := request(t, f.m, "POST", "/api/playbooks", map[string]any{"op": op, "id": id, "name": "Stale", "body": "lost", "expectedRevision": 1})
		if status != 409 {
			t.Fatalf("stale %s returned %d", op, status)
		}
	}
	var after int
	if err := f.m.db.QueryRow("SELECT count(*) FROM audit").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("rejected changes created applied audit entries")
	}
	current := mustRequest(t, f.m, "GET", "/api/playbooks", nil)["items"].([]any)[0].(map[string]any)
	if current["revision"] != float64(2) || current["body"] != "new" {
		t.Fatalf("stale mutation changed record: %+v", current)
	}
	mustRequest(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "delete", "id": id, "expectedRevision": 2})
	status, _ := request(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "save", "id": id, "body": "resurrect", "expectedRevision": 2})
	if status != 409 {
		t.Fatalf("deleted playbook mutation: %d", status)
	}
}

func TestImportedPlaybookStartsAtRevisionZeroAndPersistsUpdates(t *testing.T) {
	f := newFixture(t)
	if err := f.m.save("playbooks", []map[string]any{{"id": "legacy", "name": "Imported", "body": "old"}}, "fixture-import"); err != nil {
		t.Fatal(err)
	}
	mustRequest(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "save", "id": "legacy", "body": "updated", "expectedRevision": 0})
	if err := f.m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	item := mustRequest(t, reopened, "GET", "/api/playbooks", nil)["items"].([]any)[0].(map[string]any)
	if item["revision"] != float64(1) || item["body"] != "updated" {
		t.Fatalf("revision lost after restart: %+v", item)
	}
}
