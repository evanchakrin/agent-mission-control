package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOnlyCatalogSearchMeasurement(t *testing.T) {
	path := os.Getenv("AMC_CATALOG_SEARCH_DB")
	if path == "" {
		t.Skip("existing synthetic catalog not selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute catalog path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var count int64
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM query_sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 100000 {
		t.Fatal("selected catalog is below reference session count", count)
	}
	where, args, err := catalogWhere(SessionQuery{Text: "AMC-absent-catalog-search-probe-4a6b498b"})
	if err != nil {
		t.Fatal(err)
	}
	var indexed int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='catalog_search'").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed == 0 {
		// Retain a reproducible baseline without migrating the selected ledger.
		where = "instr(" + catalogSearchText + ",amc_fold(?))>0"
		args = []any{"AMC-absent-catalog-search-probe-4a6b498b"}
	}
	t.Logf("indexed-search-projection-present=%t", indexed != 0)
	for attempt := 1; attempt <= 5; attempt++ {
		started := time.Now()
		rows, e := db.QueryContext(ctx, "SELECT q.id FROM query_sessions q WHERE "+where+" ORDER BY q.last_activity DESC,q.id LIMIT 100", args...)
		if e != nil {
			t.Fatal(e)
		}
		matches := 0
		for rows.Next() {
			matches++
		}
		e = rows.Err()
		rows.Close()
		if e != nil || matches != 0 {
			t.Fatal("unexpected probe result", e, matches)
		}
		pageMS := time.Since(started).Milliseconds()
		started = time.Now()
		if e = db.QueryRowContext(ctx, "SELECT count(*) FROM query_sessions q WHERE "+where, args...).Scan(&matches); e != nil || matches != 0 {
			t.Fatal("unexpected count probe result", e, matches)
		}
		t.Logf("catalog=%d sample=%d absent-search-page-ms=%d matching-count-ms=%d", count, attempt, pageMS, time.Since(started).Milliseconds())
	}
}
