package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Interleave a committed writer after costs have been read and before lifetime
// rows are opened. The production reader is the same SQL transaction in both.
type interleavedEconomicsReader struct {
	*sql.Tx
	write func()
}

func (r interleavedEconomicsReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.write()
	return r.Tx.QueryContext(ctx, query, args...)
}

func TestEconomicsMeasurementUsesOneSnapshotAcrossConcurrentRepricing(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE sessions SET provider='claude',projection=json_set(projection,'$.nativeAgentId','main','$.costEstimate',0.04,'$.pricing',json('{"snapshotId":"before","pricedTokens":4}'))`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	wrote := false
	result, err := measureEconomics(ctx, interleavedEconomicsReader{Tx: tx, write: func() {
		wrote = true
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, e := s.db.ExecContext(writeCtx, `UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.08,'$.pricing.snapshotId','after')`); e != nil {
			t.Fatal(e)
		}
	}}, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || result.Costs.KnownCost == nil || *result.Costs.KnownCost != .08 || result.Lifetimes.Buckets[0].ComparableCost == nil || *result.Lifetimes.Buckets[0].ComparableCost != .08 {
		t.Fatalf("mixed repricing generations: %+v", result)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := s.MeasureEconomics(ctx, SessionQuery{})
	if err != nil || after.Version != 1 || after.MeasuredAt.IsZero() || after.Costs.KnownCost == nil || *after.Costs.KnownCost != .16 || *after.Lifetimes.Buckets[0].ComparableCost != .16 {
		t.Fatalf("next snapshot did not see committed pricing: %+v %v", after, err)
	}
}

func TestEconomicsMeasurementFiltersUnknownCostsAndCancellation(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_remove(projection,'$.costEstimate','$.pricing')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := s.MeasureEconomics(ctx, SessionQuery{MachineID: "missing"})
	if err != nil || result.Costs.Sessions != 0 || result.Costs.KnownCost != nil || result.Lifetimes.ChildSources != 0 || len(result.Lifetimes.Buckets) != 8 {
		t.Fatalf("empty cohort: %+v %v", result, err)
	}
	result, err = s.MeasureEconomics(ctx, SessionQuery{})
	if err != nil || result.Costs.Sessions != 2 || result.Costs.KnownCost != nil || result.Costs.UnpricedOrUnmeasuredTokens != 8 {
		t.Fatalf("missing pricing: %+v %v", result, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.MeasureEconomics(cancelled, SessionQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
