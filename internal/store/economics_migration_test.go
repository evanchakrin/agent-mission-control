package store

import (
	"context"
	"strings"
	"testing"
)

func TestEconomicsV6MigrationBackfillsAndKeepsTransactionalUpdates(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.costEstimate',1,'$.pricing',json('{"cost":1,"recordedTokens":4,"pricedTokens":4,"components":{"input":0.25,"cacheRead":0,"cacheWrite":0,"output":0.75,"pricedTokens":4}}'))`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow("SELECT projection FROM sessions").Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the prior narrow read-model shape; authoritative rows stay intact.
	for _, table := range []string{"session", "metadata"} {
		for _, event := range []string{"insert", "update", "delete"} {
			if _, err := s.db.Exec("DROP TRIGGER query_" + table + "_" + event); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, column := range []string{"component_known", "component_tokens", "cost_input", "cost_cache_read", "cost_cache_write", "cost_output"} {
		if _, err := s.db.Exec("ALTER TABLE query_sessions DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"session", "metadata"} {
		actual := "sessions"
		if table == "metadata" {
			actual = "session_metadata"
		}
		for _, event := range []string{"insert", "update", "delete"} {
			if _, err := s.db.Exec("CREATE TRIGGER query_" + table + "_" + event + " AFTER " + strings.ToUpper(event) + " ON " + actual + " BEGIN SELECT 1; END"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.db.Exec("UPDATE properties SET value='6' WHERE key='analytics_schema'"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := s.db.QueryRow("SELECT projection FROM sessions").Scan(&after); err != nil || before != after {
		t.Fatal("migration rewrote authoritative projection")
	}
	result, err := s.EconomicsCostTotals(ctx, SessionQuery{})
	if err != nil || result.Components.Input != .25 || result.Components.Output != .75 || result.Components.PricedTokens != 4 {
		t.Fatalf("backfill: %+v %v", result, err)
	}
	if _, err := s.db.Exec(`INSERT INTO session_metadata VALUES('s-000000',1,'{"archived":true,"revision":1}')`); err != nil {
		t.Fatal(err)
	}
	archived := true
	result, err = s.EconomicsCostTotals(ctx, SessionQuery{Archived: &archived})
	if err != nil || result.Sessions != 1 || result.Components.Output != .75 {
		t.Fatalf("organization changed evidence: %+v %v", result, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_remove(projection,'$.pricing','$.costEstimate')`); err != nil {
		t.Fatal(err)
	}
	result, err = s.EconomicsCostTotals(ctx, SessionQuery{})
	if err != nil || result.Components.PricedTokens != 0 || result.SessionsWithBreakdown != 0 || result.KnownCost != nil {
		t.Fatalf("projection replacement left stale prices: %+v %v", result, err)
	}
}
