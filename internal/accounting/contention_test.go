package accounting

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestPricingWorkerRetainsServiceThroughWriterContention(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	blocked := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunReporting(ctx, s, func(err error) {
			select {
			case blocked <- err:
			default:
			}
		})
	}()
	select {
	case err = <-blocked:
		if !store.IsContention(err) {
			t.Fatal(err)
		}
	case err = <-done:
		t.Fatal("pricing terminated on temporary contention", err)
	case <-ctx.Done():
		t.Fatal("pricing contention not reported")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Observe the worker's committed initialization rather than assuming its
	// two-second backoff plus all schema writes always finish within 2.5s on
	// a loaded test host. Keep the original overall recovery deadline. The
	// test must not initialize the queue itself and hide a broken retry loop.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case err = <-done:
			t.Fatal("pricing terminated instead of retrying", err)
		case <-ctx.Done():
			t.Fatal("pricing queue did not initialize before the recovery deadline")
		case <-ticker.C:
			if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='pricing_jobs')`).Scan(&ready); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = s.NextPricingJob(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("pricing queue unavailable after recovery", err)
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
