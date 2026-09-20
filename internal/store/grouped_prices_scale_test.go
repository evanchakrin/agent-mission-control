package store

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"
)

func TestGroupedPrices100001Sessions(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-session published grouped-pricing workload")
	}
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 100001)
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	// Synthetic published evidence; real publication guards are exercised by
	// TestGroupedUsagePricesOnlySelectedPublishedEvidence, not bypassed in service.
	_, err := s.db.Exec(`INSERT INTO accounting_estimates SELECT 'price-'||id,id,'2026-09-05',json_object('attributionVersion',1) FROM sessions;
INSERT INTO observation_prices SELECT 'price-'||session_id,id,agent_id,model,json_object('cost',0.04,'recordedTokens',4,'pricedTokens',4) FROM query_usage;
UPDATE sessions SET projection=json_set(projection,'$.pricing.snapshotId','price-'||id);`)
	if err != nil {
		t.Fatal(err)
	}
	var observed, priced int64
	var cost float64
	cursor := ""
	groups := 0
	for {
		page, e := s.GroupedUsage(ctx, GroupQuery{Dimension: "model", SessionQuery: SessionQuery{Limit: 100, Cursor: cursor}})
		if e != nil {
			t.Fatal(e)
		}
		for _, g := range page.Groups {
			if g.CostEstimate == nil || g.UnpricedTokens != 0 {
				t.Fatalf("lost pricing: %+v", g)
			}
			observed += g.Observations
			priced += g.PricedTokens
			cost += *g.CostEstimate
			groups++
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor stuck")
		}
		cursor = page.NextCursor
	}
	if observed != 100001 || priced != 400004 || math.Abs(cost-4000.04) > 1e-5 || groups < 1000 {
		t.Fatalf("incomplete catalog: observations=%d priced=%d cost=%f groups=%d", observed, priced, cost, groups)
	}
	samples := make([]time.Duration, 10)
	for i := range samples {
		start := time.Now()
		if _, err = s.GroupedUsage(ctx, GroupQuery{Dimension: "model", SessionQuery: SessionQuery{Limit: 100}}); err != nil {
			t.Fatal(err)
		}
		samples[i] = time.Since(start)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("100001-session priced model page p50=%s p95/max=%s; ten samples, diagnostic only; all %d model groups reconciled", samples[4], samples[9], groups)
}
