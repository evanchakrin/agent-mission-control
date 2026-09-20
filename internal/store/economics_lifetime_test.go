package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestEconomicsLifetimeBucketEdgesAndUsageUpdates(t *testing.T) {
	counts := []int{0, 1, 2, 3, 5, 6, 10, 11, 20, 21, 40, 41, 80, 81, 160, 161, 320}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, len(counts))
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, count := range counts {
		id := fmt.Sprintf("s-%06d", i)
		if _, err = tx.Exec(`UPDATE sessions SET provider='claude',projection=json_set(projection,'$.nativeAgentId','main','$.tokensIn',?,'$.tokensCache',?,'$.tokensOut',?,'$.costEstimate',?,'$.pricing',json(?)) WHERE id=?`, count, count, 2*count, float64(count)*.04, fmt.Sprintf(`{"snapshotId":"fixture","pricedTokens":%d}`, count*4), id); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			if _, err = tx.Exec("DELETE FROM usage_observations WHERE session_id=?", id); err != nil {
				t.Fatal(err)
			}
		}
		for j := 1; j < count; j++ {
			observation := UsageObservation{ID: fmt.Sprintf("extra-%d-%d", i, j), SessionID: id, AgentID: "main", Model: "fixture-model", Kind: "message-final", TokensIn: 1, TokensCache: 1, TokensOut: 2}
			raw, _ := json.Marshal(observation)
			if _, err = tx.Exec("INSERT INTO usage_observations(id,session_id,source_id,generation,observation) SELECT ?,?,source_id,generation,? FROM sessions WHERE id=?", observation.ID, id, raw, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result, err := s.EconomicsLifetimes(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ChildSources != 17 || result.WithoutMessageObservations != 1 {
		t.Fatalf("lost sources: %+v", result)
	}
	for i, messages := range []int64{3, 8, 16, 31, 61, 121, 241, 481} {
		b := result.Buckets[i]
		if b.Agents != 2 || b.Messages != messages || b.RecordedTokens != messages*4 || b.CostEligibleAgents != 2 || b.CostEligibleMessages != messages || b.ComparableCost == nil || math.Abs(*b.ComparableCost-float64(messages)*.04) > 1e-9 {
			t.Fatalf("bucket edge %d: %+v", i, b)
		}
	}
	// An upsert/model correction is still the same message, not a longer lifetime.
	if _, err = s.db.Exec(`UPDATE usage_observations SET observation=json_set(observation,'$.model','corrected-model') WHERE session_id='s-000001'`); err != nil {
		t.Fatal(err)
	}
	after, err := s.EconomicsLifetimes(ctx, SessionQuery{})
	if err != nil || after.Buckets[0].Messages != 3 {
		t.Fatalf("usage update inflated lifetime: %+v %v", after, err)
	}
}

func TestEconomicsLifetimesUsePricedCohortDenominatorAndPreserveUnknowns(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 5)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE sessions SET provider='claude',projection=json_set(projection,'$.nativeAgentId','main','$.parentThreadId','parent');
UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.04,'$.pricing',json('{"snapshotId":"fixture","pricedTokens":4}')) WHERE id='s-000000';
UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0.12) WHERE id='s-000002';
UPDATE usage_observations SET observation=json_set(observation,'$.kind','message-without-id') WHERE session_id='s-000003';
DELETE FROM usage_observations WHERE session_id='s-000004';`); err != nil {
		t.Fatal(err)
	}
	result, err := s.EconomicsLifetimes(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	first := result.Buckets[0]
	if result.ChildSources != 5 || result.WithoutMessageObservations != 2 || result.WithUnstableMessageIdentity != 1 || first.Agents != 3 || first.Messages != 3 || first.CostEligibleAgents != 1 || first.CostEligibleMessages != 1 || first.ComparableCost == nil || *first.ComparableCost != .04 {
		t.Fatalf("misleading cohort: %+v %+v", result, first)
	}
	for _, b := range result.Buckets[1:] {
		if b.Agents != 0 || b.ComparableCost != nil {
			t.Fatal("empty bucket invented free cost", b)
		}
	}
	filtered, err := s.EconomicsLifetimes(ctx, SessionQuery{MachineID: "m1"})
	if err != nil || filtered.ChildSources != 1 || filtered.Buckets[0].ComparableCost != nil {
		t.Fatalf("unpriced source appeared free: %+v %v", filtered, err)
	}
	if _, err = s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.costEstimate',0) WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	free, err := s.EconomicsLifetimes(ctx, SessionQuery{MachineID: "m0"})
	if err != nil || free.Buckets[0].ComparableCost == nil || *free.Buckets[0].ComparableCost != 0 {
		t.Fatalf("explicit zero price lost: %+v %v", free, err)
	}
}
