package store

import (
	"context"
	"sort"
	"testing"
	"time"
)

func TestEconomics100001PublishedCostQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-session economics workload")
	}
	s := openTestStore(t, Options{})
	seedStarted := time.Now()
	seedAnalyticsCatalog(t, s, 100001)
	t.Logf("economics catalog seed completed in %s", time.Since(seedStarted))
	ctx := context.Background()
	// A full published-breakdown catalog exercises every numeric cost field,
	// not the much cheaper case where every historical split is missing.
	coverageStarted := time.Now()
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.04,'$.pricing',json('{"cost":0.04,"recordedTokens":4,"pricedTokens":4,"components":{"input":0.01,"cacheRead":0.01,"cacheWrite":0,"output":0.02,"pricedTokens":4}}'))`); err != nil {
		t.Fatal(err)
	}
	t.Logf("economics full-coverage fixture update completed in %s", time.Since(coverageStarted))
	samples := make([]time.Duration, 20)
	for i := range samples {
		started := time.Now()
		result, err := s.EconomicsCostTotals(ctx, SessionQuery{})
		samples[i] = time.Since(started)
		if err != nil || result.Sessions != 100001 || result.RecordedTokens != 400004 || result.Components.PricedTokens != 400004 || result.SessionsWithoutBreakdown != 0 {
			t.Fatalf("incomplete corpus result: %+v %v", result, err)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("economics100001 p50=%s p95=%s max=%s; 20 samples, full breakdown coverage; not a fleet soak", samples[9], samples[18], samples[19])
	// Record aggregation latency independently of the session-page/search gates.
	// This diagnostic does not relabel their targets or certify the full workload.
}
