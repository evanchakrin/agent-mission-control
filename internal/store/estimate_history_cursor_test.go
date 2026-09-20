package store

import (
	"context"
	"strings"
	"testing"
)

func TestEstimateHistoryUsesSessionCursorIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT estimate FROM accounting_estimates WHERE session_id=? AND id>? ORDER BY id LIMIT ?`, "fixture", "cursor", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatal("history page requires sorting", detail)
		}
		found = found || strings.Contains(detail, "accounting_session_cursor")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("estimate history did not use cursor index")
	}
}
