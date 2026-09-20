package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicitly selected ledger, query-only connection; never initializes Store,
// migrates schema, copies transcripts or produces test-corpus rows.
func TestReadOnlyCalendarDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_CALENDAR_DIAGNOSTIC_DB")
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
	var values []string
	var args []any
	start := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	if date := os.Getenv("AMC_CALENDAR_DIAGNOSTIC_START"); date != "" {
		start, err = time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		day := start.AddDate(0, 0, i)
		values = append(values, "(?,?,?)")
		args = append(args, day.Format("2006-01-02"), stamp(day), stamp(day.AddDate(0, 0, 1)))
	}
	query := calendarSQL(values, "q.archived=0")
	plan, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
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
	rows, err := db.QueryContext(ctx, query, args...)
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
	t.Logf("calendar rows=%d elapsed=%s; single read, not latency certification", count, time.Since(started))
	if count != 100 {
		t.Fatal("missing days", count)
	}
}
