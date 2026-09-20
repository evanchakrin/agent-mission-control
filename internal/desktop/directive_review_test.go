package desktop

import (
	"reflect"
	"testing"
)

func TestDirectiveReviewReceiptAndStaleContent(t *testing.T) {
	f := newFixture(t)
	d := Directive{ID: "review-rule", Body: "Read this", LastReviewedAt: 1, Targets: []DirectiveTarget{}}
	if err := f.m.save("directives", []Directive{d}, "fixture"); err != nil {
		t.Fatal(err)
	}
	item := mustRequest(t, f.m, "GET", "/api/directives?id=review-rule", nil)["item"].(map[string]any)
	body := map[string]any{"op": "reviewed", "id": d.ID, "operationId": "review-once", "expectedHash": item["stateHash"]}
	first := mustRequest(t, f.m, "POST", "/api/directives", body)
	if second := mustRequest(t, f.m, "POST", "/api/directives", body); !reflect.DeepEqual(first, second) {
		t.Fatal("review receipt changed")
	}
	if err := f.m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	f.m = restarted
	if second := mustRequest(t, f.m, "POST", "/api/directives", body); !reflect.DeepEqual(first, second) {
		t.Fatal("restart lost review receipt")
	}
	var count int
	if err := f.m.db.QueryRow("SELECT count(*) FROM audit WHERE kind='directive-reviewed'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count %d: %v", count, err)
	}
	body["operationId"] = "stale-review"
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("stale review accepted: %d", code)
	}
	d.Body = "Changed while reviewing"
	if err := f.m.save("directives", []Directive{d}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("changed body accepted: %d", code)
	}
	body["operationId"] = "review-once"
	if second := mustRequest(t, f.m, "POST", "/api/directives", body); !reflect.DeepEqual(first, second) {
		t.Fatal("new state invalidated old receipt")
	}
	body["expectedHash"] = viewDirective(d).StateHash
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("identity reused: %d", code)
	}
}

func TestDirectiveReviewAuditFailureAndPendingMeasurement(t *testing.T) {
	f := newFixture(t)
	d := Directive{ID: "rule", Body: "body", LastReviewedAt: 1}
	if err := f.m.save("directives", []Directive{d}, "fixture"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"op": "reviewed", "id": d.ID, "operationId": "atomic-review", "expectedHash": viewDirective(d).StateHash}
	if _, err := f.m.db.Exec(`CREATE TRIGGER fail_review BEFORE INSERT ON audit WHEN NEW.kind='directive-reviewed' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 500 {
		t.Fatalf("failed audit: %d", code)
	}
	items, err := f.m.directiveItems()
	if err != nil || items[0].LastReviewedAt != 1 {
		t.Fatalf("partial review: %+v %v", items, err)
	}
	var count int
	if err := f.m.db.QueryRow("SELECT count(*) FROM local_state WHERE key='directive-review-operation:atomic-review'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial receipt")
	}
	if _, err := f.m.db.Exec("DROP TRIGGER fail_review"); err != nil {
		t.Fatal(err)
	}
	mustRequest(t, f.m, "POST", "/api/directives", body)
	d.PendingMeasurement = &Measurement{Body: "new body", Subs: 200}
	if err := f.m.save("directives", []Directive{d}, "fixture"); err != nil {
		t.Fatal(err)
	}
	body["operationId"] = "pending-review"
	body["expectedHash"] = viewDirective(d).StateHash
	if code, _ := request(t, f.m, "POST", "/api/directives", body); code != 409 {
		t.Fatalf("pending measurement approved: %d", code)
	}
}
