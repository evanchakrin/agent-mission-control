package desktop

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPlantReceiptRecoveryDoesNotDuplicateRulesOrFiles(t *testing.T) {
	for _, phase := range []string{"directive-target", "directive-plant-complete"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.m.db.Exec(`CREATE TRIGGER fail_plant BEFORE INSERT ON audit WHEN NEW.kind='` + phase + `' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"op": "plant", "operationId": "plant-once", "title": "Rule", "body": "Exact planting content", "targets": []string{idFor(f.file)}}
			if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 500 {
				t.Fatalf("fault did not interrupt: %d", code)
			}
			items, err := f.m.directiveItems()
			if err != nil || len(items) != 1 {
				t.Fatalf("missing durable intent: %+v %v", items, err)
			}
			id := items[0].ID
			before, err := os.ReadFile(f.file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(before), "Exact planting content") != 1 {
				t.Fatal("file boundary not reached")
			}
			var writes int
			if err := f.m.db.QueryRow("SELECT count(*) FROM file_operations").Scan(&writes); err != nil {
				t.Fatal(err)
			}
			if err := f.m.Close(); err != nil {
				t.Fatal(err)
			}
			m, err := New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if _, err := m.db.Exec("DROP TRIGGER fail_plant"); err != nil {
				t.Fatal(err)
			}
			first := mustRequest(t, m, "POST", "/api/directives", body)
			if first["id"] != id {
				t.Fatal("recovery created another identity")
			}
			items, err = m.directiveItems()
			if err != nil || len(items) != 1 {
				t.Fatal("duplicate rule")
			}
			after, err := os.ReadFile(f.file)
			if err != nil || string(after) != string(before) {
				t.Fatal("recovery rewrote planted file")
			}
			var afterWrites int
			if err := m.db.QueryRow("SELECT count(*) FROM file_operations").Scan(&afterWrites); err != nil || afterWrites != writes {
				t.Fatal("duplicate file operation")
			}
			if again := mustRequest(t, m, "POST", "/api/directives", body); !reflect.DeepEqual(first, again) {
				t.Fatal("final receipt not stable")
			}
			body["body"] = "different"
			if code, _ := request(t, m, "POST", "/api/directives", body); code != 409 {
				t.Fatalf("identity reused: %d", code)
			}
			body["body"] = "Exact planting content"
			mustRequest(t, m, "POST", "/api/directives", map[string]any{"op": "retire", "id": id})
			if again := mustRequest(t, m, "POST", "/api/directives", body); !reflect.DeepEqual(first, again) {
				t.Fatal("retirement lost original receipt")
			}
			items, err = m.directiveItems()
			if err != nil || len(items) != 0 {
				t.Fatal("old retry resurrected retired rule")
			}
		})
	}
}

func TestPendingPlantCannotApplyChangedRule(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_plant BEFORE INSERT ON audit WHEN NEW.kind='directive-target-intent' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"op": "plant", "operationId": "pending", "body": "original", "targets": []string{idFor(f.file)}}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 500 {
		t.Fatalf("fault: %d", code)
	}
	items, err := f.m.directiveItems()
	if err != nil || len(items) != 1 {
		t.Fatal("missing intent")
	}
	items[0].Body = "newer decision"
	if err := f.m.save("directives", items, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.db.Exec("DROP TRIGGER fail_plant"); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("changed rule applied: %d", code)
	}
	content, err := os.ReadFile(f.file)
	if err != nil || strings.Contains(string(content), "mission-control:directive:") {
		t.Fatal("conflicted retry changed file")
	}
}
