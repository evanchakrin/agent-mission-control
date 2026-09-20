package desktop

import (
	"fmt"
	"net/url"
	"testing"
)

func TestPlaybookPagesAndSingleReadsPreserveImportedLibrary(t *testing.T) {
	f := newFixture(t)
	items := []Playbook{}
	for i := 350; i >= 0; i-- {
		items = append(items, Playbook{ID: fmt.Sprintf("book-%04d", i), Name: "Imported", Body: fmt.Sprintf("body-%d", i), Revision: int64(i)})
	}
	if err := f.m.save("playbooks", items, "fixture-import"); err != nil {
		t.Fatal(err)
	}
	after := ""
	seen := 0
	for {
		page := mustRequest(t, f.m, "GET", "/api/playbooks?limit=25&after="+url.QueryEscape(after), nil)
		rows := page["items"].([]any)
		if len(rows) > 25 {
			t.Fatal("unbounded page")
		}
		for _, row := range rows {
			item := row.(map[string]any)
			if item["id"] != fmt.Sprintf("book-%04d", seen) || item["body"] != fmt.Sprintf("body-%d", seen) {
				t.Fatalf("missing or reordered item %+v at %d", item, seen)
			}
			seen++
		}
		next := page["nextCursor"].(string)
		if next == "" {
			break
		}
		if next == after {
			t.Fatal("cursor stalled")
		}
		after = next
	}
	if seen != 351 {
		t.Fatalf("lost imported library: %d", seen)
	}
	one := mustRequest(t, f.m, "GET", "/api/playbooks?id=book-0349", nil)["item"].(map[string]any)
	if one["revision"] != float64(349) || one["body"] != "body-349" {
		t.Fatalf("single read %+v", one)
	}
	for _, path := range []string{"/api/playbooks?limit=0", "/api/playbooks?limit=51", "/api/playbooks?id="} {
		status, _ := request(t, f.m, "GET", path, nil)
		if status != 400 {
			t.Fatalf("invalid request %s: %d", path, status)
		}
	}
	if status, _ := request(t, f.m, "GET", "/api/playbooks?id=missing", nil); status != 404 {
		t.Fatalf("missing identity: %d", status)
	}
}
