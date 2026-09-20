package desktop

import (
	"fmt"
	"net/url"
	"testing"
)

func TestBrainHistoryTraversesAllSnapshotsWithTimestampTies(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 251; i++ {
		if _, err := f.m.db.Exec("INSERT INTO snapshots VALUES(?,?,?,?,?,?)", fmt.Sprintf("stamp-%04d", i), f.file, []byte("old"), "hash", 0, i/10); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/brain/history?id=" + url.QueryEscape(idFor(f.file))
	before := ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		result := mustRequest(t, f.m, "GET", base+"&before="+url.QueryEscape(before), nil)
		history := result["history"].([]any)
		want := 100
		if page == 2 {
			want = 51
		}
		if len(history) != want {
			t.Fatalf("page %d: got %d", page, len(history))
		}
		for _, item := range history {
			stamp := item.(map[string]any)["stamp"].(string)
			if seen[stamp] {
				t.Fatalf("repeated snapshot %s", stamp)
			}
			seen[stamp] = true
		}
		before = result["nextCursor"].(string)
		if (before == "") != (page == 2) {
			t.Fatalf("unexpected continuation %q", before)
		}
		// Newer snapshots inserted between pages must not shift older boundaries.
		if page == 0 {
			if _, err := f.m.db.Exec("INSERT INTO snapshots VALUES(?,?,?,?,?,?)", "newer", f.file, []byte("new"), "hash", 0, 999); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != 251 {
		t.Fatalf("missing history: %d", len(seen))
	}
	status, _ := request(t, f.m, "GET", base+"&before=nonexistent", nil)
	if status != 400 {
		t.Fatalf("invalid cursor status %d", status)
	}
}

func TestAuditHistoryBeyondLegacyLimit(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 501; i++ {
		if _, err := f.m.db.Exec("INSERT INTO audit(at,kind,status) VALUES(?,?,?)", i, "fixture", "ok"); err != nil {
			t.Fatal(err)
		}
	}
	var expected int
	if err := f.m.db.QueryRow("SELECT count(*) FROM audit").Scan(&expected); err != nil {
		t.Fatal(err)
	}
	before := ""
	seen := map[float64]bool{}
	for {
		page := mustRequest(t, f.m, "GET", "/api/audit?limit=100&before="+before, nil)
		rows := page["entries"].([]any)
		if len(rows) > 100 {
			t.Fatal("unbounded page")
		}
		for _, row := range rows {
			id := row.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatal("duplicate audit record")
			}
			seen[id] = true
		}
		next := page["nextCursor"].(string)
		if next == "" {
			break
		}
		if next == before {
			t.Fatal("cursor stalled")
		}
		before = next
	}
	if len(seen) != expected {
		t.Fatalf("lost audit history: %d/%d", len(seen), expected)
	}
	for _, query := range []string{"before=-1", "before=abc", "before=9223372036854775808", "limit=0", "limit=401"} {
		status, _ := request(t, f.m, "GET", "/api/audit?"+query, nil)
		if status != 400 {
			t.Fatalf("%s status=%d", query, status)
		}
	}
}
