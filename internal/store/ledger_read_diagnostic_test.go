package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit query-only inspection of a stopped failed fixture, without Open's
// migrations, background checkpoints, or any transcript/configuration output.
func TestReadOnlyLedgerReadsDiagnostic(t *testing.T) {
	runReadOnlyLedgerReadsDiagnostic(t, false)
}

func TestWarmedReadOnlyLedgerReadsDiagnostic(t *testing.T) {
	runReadOnlyLedgerReadsDiagnostic(t, true)
}

func runReadOnlyLedgerReadsDiagnostic(t *testing.T, warm bool) {
	path := os.Getenv("AMC_LEDGER_READ_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no diagnostic ledger selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute ledger path required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if warm {
		// Separate cold connection/recovery cost from already-open query cost.
		// This is not a relaxation or pass for the cold five-second check.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		started := time.Now()
		err := db.PingContext(ctx)
		cancel()
		if err != nil {
			t.Fatal("bounded connection preparation", err)
		}
		t.Logf("connection preparation=%s", time.Since(started))
	}
	s := &Store{db: db, dir: filepath.Dir(path)}
	for pass := 0; pass < 3; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		started, previous, name := time.Now(), time.Now(), "starting"
		d, err := s.DiagnosticsWithStage(ctx, func(next string) {
			t.Logf("pass=%d completed=%s duration=%s", pass, name, time.Since(previous))
			name, previous = next, time.Now()
		})
		cancel()
		if err != nil {
			t.Fatalf("health stage=%s: %v", name, err)
		}
		t.Logf("pass=%d diagnostics=%s sources=%d durable=%d indexed=%d", pass, time.Since(started), d.Sources, d.DurableBytes, d.IndexedBytes)
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		started = time.Now()
		totals, err := s.CatalogTotals(ctx, SessionQuery{})
		cancel()
		if err != nil {
			t.Fatal("totals", err)
		}
		t.Logf("pass=%d totals=%s sessions=%d tokens=%d pool=%+v", pass, time.Since(started), totals.Sessions, totals.RecordedTokens, s.ConnectionStats())
	}
}
