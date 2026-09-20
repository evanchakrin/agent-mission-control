package store

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"
)

func TestEconomicsLifetime100001ChildSources(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-child-source lifetime workload")
	}
	s := openTestStore(t, Options{})
	started := time.Now()
	seedAnalyticsCatalog(t, s, 100001)
	t.Logf("lifetime catalog seed: %s", time.Since(started))
	if _, err := s.db.Exec(`UPDATE sessions SET provider='claude',projection=json_set(projection,'$.nativeAgentId','main','$.parentThreadId','parent','$.costEstimate',0.04,'$.pricing',json('{"snapshotId":"fixture","pricedTokens":4}'))`); err != nil {
		t.Fatal(err)
	}
	samples := make([]time.Duration, 10)
	for i := range samples {
		started = time.Now()
		result, err := s.EconomicsLifetimes(context.Background(), SessionQuery{})
		samples[i] = time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		b := result.Buckets[0]
		if result.ChildSources != 100001 || result.WithoutMessageObservations != 0 || b.Agents != 100001 || b.Messages != 100001 || b.RecordedTokens != 400004 || b.CostEligibleAgents != 100001 || b.CostEligibleMessages != 100001 || b.ComparableCost == nil || math.Abs(*b.ComparableCost-4000.04) > 1e-5 {
			t.Fatalf("scale cohort differs: %+v %+v", result, b)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("100001-child lifetime query p50=%s p95/max=%s; ten samples; diagnostic, not a fleet/HTTP/soak certificate", samples[4], samples[9])
}
