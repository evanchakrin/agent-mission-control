package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// Explicitly selected, read-only disposable evidence. Use the real aggregate
// method without Store.Open, schema upgrades, checkpointing or source bodies.
func TestReadOnlyCatalogTotalsDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_CATALOG_TOTALS_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no disposable ledger selected")
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "ledger.sqlite" || filepath.Base(filepath.Dir(path)) != "hub" || !regexp.MustCompile(`^corpus-verify-[a-f0-9]{32}$`).MatchString(filepath.Base(filepath.Dir(filepath.Dir(path)))) {
		t.Fatal("explicit disposable corpus hub ledger required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(8388608)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var readOnly, cache, pageCount int
	for query, target := range map[string]*int{"PRAGMA query_only": &readOnly, "PRAGMA cache_size": &cache, "PRAGMA page_count": &pageCount} {
		if err = db.QueryRowContext(ctx, query).Scan(target); err != nil {
			cancel()
			t.Fatal(err)
		}
	}
	cancel()
	if readOnly != 1 {
		t.Fatal("connection is not query-only")
	}
	t.Logf("read-only baseline cache_size=%d databasePages=%d; not live pool or cold-cache evidence", cache, pageCount)
	s := &Store{db: db}
	var previous AggregateTotals
	for pass := 0; pass < 3; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		started := time.Now()
		result, err := s.CatalogTotals(ctx, SessionQuery{})
		elapsed := time.Since(started)
		cancel()
		if err != nil {
			t.Fatalf("pass=%d elapsed=%s: %v", pass, elapsed, err)
		}
		if pass > 0 && (result.Sessions != previous.Sessions || result.RecordedTokens != previous.RecordedTokens) {
			t.Fatal("fixture changed during baseline")
		}
		previous = result
		t.Logf("pass=%d elapsed=%s sessions=%d recordedTokens=%d; not loaded HTTP latency", pass, elapsed, result.Sessions, result.RecordedTokens)
	}
}
