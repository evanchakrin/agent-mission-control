package store

import (
	"context"
	"strings"
	"testing"
)

func TestPendingSourceDispatchDoesNotSortWholeCatalog(t *testing.T) {
	s := openTestStore(t, Options{})
	if err := s.SetupIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+pendingSourcesSQL, "now", "source", "generation", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seek := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(detail), "TEMP B-TREE") {
			t.Errorf("per-dispatch catalog sort: %s", detail)
		}
		if strings.Contains(detail, "SEARCH w USING") && strings.Contains(detail, "index_work_catalog") {
			seek = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seek {
		t.Fatal("pending-source cursor did not seek the ordered catalog index")
	}
}
