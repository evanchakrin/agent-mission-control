package store

import (
	"context"
	"testing"
	"time"
)

// A stalled durable writer must not make diagnostic/catalog reads wait for its
// commit. WAL readers must see the last committed state, not staged progress.
func TestDashboardReadsDuringUncommittedWriter(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	ingest(t, s, src, 0, "fixture\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE sources SET durable_offset=999,indexed_offset=999"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"diagnostics", "totals", "sessions", "machines"} {
		t.Run(name, func(t *testing.T) {
			readCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			started := time.Now()
			switch name {
			case "diagnostics":
				d, err := s.Diagnostics(readCtx)
				if err != nil || d.DurableBytes != 8 || d.IndexedBytes != 8 {
					t.Fatalf("committed diagnostics: %+v, %v", d, err)
				}
			case "totals":
				d, err := s.CatalogTotals(readCtx, SessionQuery{})
				if err != nil || d.Sessions != 1 {
					t.Fatalf("committed totals: %+v, %v", d, err)
				}
			case "sessions":
				d, err := s.QuerySessions(readCtx, SessionQuery{Limit: 100})
				if err != nil || len(d.Sessions) != 1 {
					t.Fatalf("committed page: %+v, %v", d, err)
				}
			case "machines":
				if _, err := s.ListMachines(readCtx); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("read completed with writer still uncommitted in %s", time.Since(started))
		})
	}
}
