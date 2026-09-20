package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in query-only diagnostic: no Store setup, migration or copy.
func TestReadOnlyBehaviorDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_BEHAVIOR_DIAGNOSTIC_DB")
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
		t.Fatal(guard, err)
	}
	query := fmt.Sprintf(behaviorPatternsSQL, "q.archived=0")
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
	t.Logf("patterns=%d elapsed=%s; diagnostic only, not certification", count, time.Since(started))
}
