package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"
)

// Read-only diagnostic connections to an explicitly selected disposable ledger.
// No Store.Open, migration, checkpoint, application option or default is changed.
func TestMappedReadDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_MAPPED_READ_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no disposable diagnostic ledger selected")
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "ledger.sqlite" || filepath.Base(filepath.Dir(path)) != "hub" || !regexp.MustCompile(`^corpus-verify-[a-f0-9]{32}$`).MatchString(filepath.Base(filepath.Dir(filepath.Dir(path)))) {
		t.Fatal("explicit disposable corpus hub ledger required")
	}
	type mode struct {
		db      *sql.DB
		bytes   int
		samples []float64
	}
	modes := []mode{{bytes: 0}, {bytes: 8 << 20}}
	for i := range modes {
		m := &modes[i]
		dsn := "file:" + filepath.ToSlash(path) + fmt.Sprintf("?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(%d)", m.bytes)
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		m.db = db
		defer db.Close()
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var actual, readOnly int
		err = db.QueryRowContext(ctx, `PRAGMA mmap_size`).Scan(&actual)
		if err == nil {
			err = db.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&readOnly)
		}
		cancel()
		if err != nil || actual != m.bytes || readOnly != 1 {
			t.Fatal("diagnostic connection policy", actual, readOnly, err)
		}
	}
	var expected *AggregateTotals
	for pass := 0; pass < 60; pass++ {
		// Alternate first-reader order to reduce a consistent warm-cache advantage.
		for step := 0; step < 2; step++ {
			m := &modes[(pass+step)%2]
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			started := time.Now()
			// Exercise both the readiness check and aggregate that timed out,
			// without Store.Open or any migration on preserved evidence.
			s := &Store{db: m.db}
			totals, err := s.CatalogTotals(ctx, SessionQuery{})
			elapsed := float64(time.Since(started)) / float64(time.Millisecond)
			cancel()
			if err != nil {
				t.Fatalf("map=%d pass=%d latencyMS=%.3f totals failed: %v", m.bytes, pass, elapsed, err)
			}
			if expected == nil {
				expected = &totals
			} else if !reflect.DeepEqual(*expected, totals) {
				t.Fatal("totals differ between diagnostic reads; use a stopped fixture ledger")
			}
			m.samples = append(m.samples, elapsed)
			if elapsed >= 200 {
				t.Logf("slow read map=%d pass=%d latencyMS=%.3f at=%s", m.bytes, pass, elapsed, started.UTC().Format(time.RFC3339Nano))
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, m := range modes {
		sort.Float64s(m.samples)
		t.Logf("map=%d samples=%d p95MS=%.3f maxMS=%.3f; diagnostic only, not API or resource certification", m.bytes, len(m.samples), m.samples[56], m.samples[59])
	}
}
