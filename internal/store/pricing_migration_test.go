package store

import (
	"context"
	"testing"
	"time"
)

func TestPricingPolicyComparisonSideTableUpgrade(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.InitializePricingQueue(ctx); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-comparison schema in this disposable database only.
	if _, err = s.db.ExecContext(ctx, `DROP TRIGGER pricing_default_new_session; DROP TRIGGER pricing_policy_changed; DROP TABLE pricing_policy_comparisons`); err != nil {
		t.Fatal(err)
	}
	seedAnalyticsCatalog(t, s, 1)
	if err = s.PutRateCatalog(ctx, "fixture-catalog", []byte(`{"id":"fixture-catalog","rates":[]}`)); err != nil {
		t.Fatal(err)
	}
	// The old executable uses this exact three-value insert shape.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO pricing_policies VALUES('s-000000','fixture-catalog','fixture-context'); INSERT INTO pricing_jobs(session_id) VALUES('s-000000')`); err != nil {
		t.Fatal(err)
	}
	s.pricingReady.Store(false)
	for range 2 {
		if err = s.InitializePricingQueue(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('pricing_policies')`).Scan(&count); err != nil || count != 3 {
			t.Fatal(count, err)
		}
		s.pricingReady.Store(false)
	}
	p, err := s.PricingPolicy(ctx, "s-000000")
	if err != nil || p.CatalogID != "fixture-catalog" || p.Context != "fixture-context" || p.ComparisonAt != nil || p.ID == 0 {
		t.Fatal("legacy policy changed", p, err)
	}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err = s.SetPricingPolicyAt(ctx, "s-000000", "fixture-catalog", "fixture-context", &at); err != nil {
		t.Fatal(err)
	}
	p, err = s.PricingPolicy(ctx, "s-000000")
	if err != nil || p.ComparisonAt == nil || !p.ComparisonAt.Equal(at) {
		t.Fatal(p, err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO pricing_policies VALUES('s-000000','fixture-catalog','legacy-updated') ON CONFLICT(session_id) DO UPDATE SET catalog_id=excluded.catalog_id,billing_context=excluded.billing_context`); err != nil {
		t.Fatal("old write shape broke", err)
	}
	p, err = s.PricingPolicy(ctx, "s-000000")
	if err != nil || p.ComparisonAt != nil || p.Context != "legacy-updated" {
		t.Fatal("old writer inherited a stale comparison date", p, err)
	}
	if err = s.DisablePricingPolicy(ctx, "s-000000"); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM pricing_policy_comparisons`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("orphaned comparison policy", remaining, err)
	}
}
