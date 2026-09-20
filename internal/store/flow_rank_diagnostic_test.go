package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOnlyFlowRankDiagnostic(t *testing.T) {
	path, id := os.Getenv("AMC_FLOW_DIAGNOSTIC_DB"), os.Getenv("AMC_FLOW_DIAGNOSTIC_SESSION")
	if path == "" || id == "" {
		t.Skip("no diagnostic ledger/session selected")
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, query := range []struct{ name, sql string }{{"output", topFlowOutputSQL}, {"cost", topFlowCostSQL}} {
		started := time.Now()
		rows, err := db.QueryContext(ctx, query.sql, id)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var agent, name string
			var value float64
			if err = rows.Scan(&agent, &name, &value); err != nil {
				t.Fatal(err)
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if count > 14 {
			t.Fatal("ranking unbounded")
		}
		t.Logf("%s ranking: rows=%d elapsed=%s; single observation, not p95 certification", query.name, count, time.Since(started))
	}
}
