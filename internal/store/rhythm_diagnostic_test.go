package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in, read-only diagnosis against existing evidence. No Store initialization,
// schema changes, transcript copies, or generated corpus.
func TestReadOnlyRhythmDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_RHYTHM_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no diagnostic ledger selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute ledger path required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var guard int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&guard); err != nil || guard != 1 {
		t.Fatal("read-only guard", guard, err)
	}
	query := rhythmSQL("q.archived=0")
	// Isolation only: never used by the application, and not a correctness test.
	if os.Getenv("AMC_RHYTHM_DIAGNOSTIC_OMIT_USAGE_TIME") == "1" {
		start := strings.Index(query, "(SELECT MIN(u.timestamp)")
		if start < 0 {
			t.Fatal("usage timestamp expression not found")
		}
		endOffset := strings.Index(query[start:], ") usage_first")
		if endOffset < 0 {
			t.Fatal("usage timestamp expression end not found")
		}
		end := start + endOffset
		query = query[:start] + "NULL" + query[end+1:]
	}
	plan, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		t.Fatal(err)
	}
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
	}
	err = plan.Err()
	plan.Close()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("rhythm rows=%d elapsed=%s; diagnostic only, not latency certification", count, time.Since(started))
}
