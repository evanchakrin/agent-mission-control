package desktop

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlaybookReceiptSurvivesRetryBackupAndRestore(t *testing.T) {
	f := newFixture(t)
	body := map[string]any{"op": "save", "name": "Retry fixture", "body": "preserved", "expectedRevision": 0, "operationId": "create-once"}
	first := mustRequest(t, f.m, "POST", "/api/playbooks", body)
	id := first["id"].(string)
	var auditedID, auditedOperation string
	var auditedRevision int
	if err := f.m.db.QueryRow("SELECT json_extract(detail,'$.recordId'),json_extract(detail,'$.operationId'),json_extract(detail,'$.revision') FROM audit WHERE kind='playbook-save'").Scan(&auditedID, &auditedOperation, &auditedRevision); err != nil || auditedID != id || auditedOperation != "create-once" || auditedRevision != 1 {
		t.Fatalf("untraceable audit: %s %s %d %v", auditedID, auditedOperation, auditedRevision, err)
	}
	if second := mustRequest(t, f.m, "POST", "/api/playbooks", body); !reflect.DeepEqual(first, second) {
		t.Fatalf("receipt changed: %+v %+v", first, second)
	}
	var records int
	if err := f.m.db.QueryRow("SELECT count(*) FROM audit WHERE kind='playbook-save'").Scan(&records); err != nil || records != 1 {
		t.Fatalf("duplicate audit %d %v", records, err)
	}
	changed := map[string]any{"op": "save", "name": "Different", "expectedRevision": 0, "operationId": "create-once"}
	if status, _ := request(t, f.m, "POST", "/api/playbooks", changed); status != 409 {
		t.Fatalf("operation reused: %d", status)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := f.m.Backup(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreStateBackup(context.Background(), backup, restoredDir); err != nil {
		t.Fatal(err)
	}
	opts := f.opts
	opts.StateDir = restoredDir
	restored, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if replay := mustRequest(t, restored, "POST", "/api/playbooks", body); !reflect.DeepEqual(first, replay) {
		t.Fatal("backup lost receipt")
	}
	deletion := map[string]any{"op": "delete", "id": id, "expectedRevision": 1, "operationId": "delete-once"}
	deleted := mustRequest(t, restored, "POST", "/api/playbooks", deletion)
	if replay := mustRequest(t, restored, "POST", "/api/playbooks", deletion); !reflect.DeepEqual(deleted, replay) {
		t.Fatal("delete receipt changed")
	}
	if replay := mustRequest(t, restored, "POST", "/api/playbooks", body); !reflect.DeepEqual(first, replay) {
		t.Fatal("original receipt no longer available")
	}
	if items := mustRequest(t, restored, "GET", "/api/playbooks", nil)["items"].([]any); len(items) != 0 {
		t.Fatal("old creation retry resurrected deleted playbook")
	}
}

func TestPlaybookReceiptRollsBackWithFailedAudit(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_playbook_audit BEFORE INSERT ON audit WHEN NEW.kind='playbook-save' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"op": "save", "name": "Atomic", "expectedRevision": 0, "operationId": "atomic-operation"}
	if status, result := request(t, f.m, "POST", "/api/playbooks", body); status != 500 || result["error"] == "injected audit failure" {
		t.Fatalf("failed audit must preserve uncertain retry semantics: %d %+v", status, result)
	}
	var count int
	if err := f.m.db.QueryRow("SELECT count(*) FROM local_state WHERE key IN ('playbooks','playbook-operation:atomic-operation')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial commit %d %v", count, err)
	}
	if _, err := f.m.db.Exec("DROP TRIGGER fail_playbook_audit"); err != nil {
		t.Fatal(err)
	}
	mustRequest(t, f.m, "POST", "/api/playbooks", body)
}
