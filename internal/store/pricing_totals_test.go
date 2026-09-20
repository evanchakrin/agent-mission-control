package store

import (
	"context"
	"fmt"
	"testing"
)

func TestCoverageTotalsAreTokenWeightedAndPreserveUnknownAttribution(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	// Synthetic read-model fixture: 100 fully priced tokens, 900 partially
	// priced tokens, and 9000 tokens whose legacy cost has no verified coverage.
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.tokensCache',0,'$.tokensCacheWrite',0,'$.tokensOut',0,
	'$.tokensIn',CASE id WHEN 's-000000' THEN 100 WHEN 's-000001' THEN 900 ELSE 9000 END);
	UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.1,'$.pricing',json('{"snapshotId":"fixture-full","pricedTokens":100,"unattributedTokens":0}')) WHERE id='s-000000';
	UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.09,'$.pricing',json('{"snapshotId":"fixture-partial","pricedTokens":90,"unattributedTokens":20}')) WHERE id='s-000001';
	UPDATE sessions SET projection=json_set(projection,'$.costEstimate',9) WHERE id='s-000002';`)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := s.CatalogTotals(ctx, SessionQuery{Limit: 1, Cursor: "ignored-by-aggregate"})
	if err != nil {
		t.Fatal(err)
	}
	if totals.RecordedTokens != 10000 || totals.PricedTokens != 190 || totals.UnpricedTokens != 9810 || totals.TokensAwaitingPricing != 9000 || totals.KnownUnattributedTokens != 20 || totals.SessionsWithPricingCoverage != 2 || totals.TokenPricingCoverage == nil || *totals.TokenPricingCoverage != 0.019 || totals.PricingCoverage != "pending-pricing" {
		t.Fatalf("misleading coverage: %+v", totals)
	}
	legacy, err := s.SessionTotals(ctx, SessionQuery{})
	if err != nil || legacy.PricedTokens != 190 {
		t.Fatalf("compatibility totals dropped coverage: %+v %v", legacy, err)
	}
	machines, err := s.CatalogMachines(ctx, SessionQuery{Limit: 1})
	if err != nil || len(machines.Items) != 1 {
		t.Fatal(machines, err)
	}
	item := machines.Items[0]
	want, err := s.CatalogTotals(ctx, SessionQuery{MachineID: item.ID})
	if err != nil || item.Accounting == nil || item.Accounting.RecordedTokens != want.RecordedTokens || item.Accounting.PricedTokens != want.PricedTokens || item.Accounting.TokensAwaitingPricing != want.TokensAwaitingPricing || item.Accounting.UnpricedTokens != want.UnpricedTokens || item.Sessions != want.Sessions {
		t.Fatal("machine grouped coverage differs", item, want, err)
	}
	if item.CostEstimate == nil || want.CostEstimate == nil || *item.CostEstimate != *want.CostEstimate {
		t.Fatal("machine estimate differs", item, want)
	}
	filtered, err := s.CatalogTotals(ctx, SessionQuery{Provider: "claude"})
	if err != nil || filtered.RecordedTokens != 9100 || filtered.PricedTokens != 100 || filtered.TokensAwaitingPricing != 9000 {
		t.Fatalf("filtered coverage: %+v %v", filtered, err)
	}
	partial, err := s.CatalogTotals(ctx, SessionQuery{Provider: "codex"})
	if err != nil || partial.PricingCoverage != "partially-priced" || partial.KnownUnattributedTokens != 20 {
		t.Fatalf("partial coverage: %+v %v", partial, err)
	}
	empty, err := s.CatalogTotals(ctx, SessionQuery{Text: "absent"})
	if err != nil || empty.TokenPricingCoverage != nil || empty.PricingCoverage != "no-recorded-usage" {
		t.Fatalf("empty coverage: %+v %v", empty, err)
	}
	// A projection invalidation refreshes the derived catalog in the same write.
	if _, err = s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_remove(projection,'$.pricing') WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	totals, err = s.CatalogTotals(ctx, SessionQuery{})
	if err != nil || totals.PricedTokens != 90 || totals.TokensAwaitingPricing != 9100 {
		t.Fatalf("invalidated price retained: %+v %v", totals, err)
	}
}

func TestCoverageSchema4UpgradeBackfillsWithoutRebuildingHistory(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.pricing',json('{"snapshotId":"fixture","pricedTokens":3,"unattributedTokens":1}'));CREATE INDEX preserved_coverage_usage ON query_usage(id);`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"session", "metadata"} {
		for _, event := range []string{"insert", "update", "delete"} {
			if _, err := s.db.ExecContext(ctx, "DROP TRIGGER query_"+table+"_"+event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX query_machine_accounting;ALTER TABLE query_sessions DROP COLUMN pricing_known;ALTER TABLE query_sessions DROP COLUMN priced_tokens;ALTER TABLE query_sessions DROP COLUMN unattributed_tokens;UPDATE properties SET value='4' WHERE key='analytics_schema'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"session", "metadata"} {
		for _, event := range []string{"insert", "update", "delete"} {
			source := "sessions"
			if table == "metadata" {
				source = "session_metadata"
			}
			if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE TRIGGER query_%s_%s AFTER %s ON %s BEGIN SELECT 1; END", table, event, event, source)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	totals, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil || totals.PricedTokens != 3 || totals.KnownUnattributedTokens != 1 || totals.TokensAwaitingPricing != 0 {
		t.Fatalf("coverage not backfilled: %+v %v", totals, err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='preserved_coverage_usage'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history read model rebuilt: %d %v", count, err)
	}
}
