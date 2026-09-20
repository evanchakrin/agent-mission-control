package accounting

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestCostComponentsUseSelectedRatesNotTokenProportions(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rates := []Rate{{ID: "rate", Model: "known", Context: "fixture", Source: "fixture", EffectiveFrom: at, Input: 2, CacheRead: 1, CacheWrite: 5, Output: 10}}
	rows := []store.UsageObservation{{Model: "known", Timestamp: at, TokensIn: 100, TokensCache: 200, TokensCacheWrite: 300, TokensOut: 400}, {Model: "unknown", Timestamp: at, TokensIn: 9000}}
	e := Price(rows, rates, "fixture", nil)
	c := e.Components
	if c == nil || c.PricedTokens != 1000 || e.UnpricedTokens != 9000 {
		t.Fatalf("coverage: %+v", e)
	}
	for _, pair := range [][2]float64{{c.Input, .0002}, {c.CacheRead, .0002}, {c.CacheWrite, .0015}, {c.Output, .004}, {e.Cost, .0059}} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Fatalf("rate split: %+v", e)
		}
	}
	if !validPricingProgress(e) {
		t.Fatal("valid split rejected")
	}
	copy := e
	copy.Components = &store.CostComponents{Input: e.Cost, PricedTokens: 999}
	if validPricingProgress(copy) {
		t.Fatal("component coverage mismatch accepted")
	}
	if rows[0].TokensIn != 100 || rows[1].TokensIn != 9000 {
		t.Fatal("pricing changed usage")
	}
	var old Estimate
	if err := json.Unmarshal([]byte(`{"cost":1,"pricedTokens":100,"recordedTokens":100}`), &old); err != nil || old.Components != nil {
		t.Fatal("old total received invented components")
	}
}

func TestCostComponentsAccumulateAcrossPagesAndPreserveFreeRates(t *testing.T) {
	var e Estimate
	for i := 0; i < 3; i++ {
		addComponents(&e, &store.CostComponents{Input: 1, CacheRead: 2, CacheWrite: 3, Output: 4, PricedTokens: 10})
	}
	if *e.Components != (store.CostComponents{Input: 3, CacheRead: 6, CacheWrite: 9, Output: 12, PricedTokens: 30}) {
		t.Fatal("page sum lost cost evidence")
	}
	at := time.Now()
	free := Price([]store.UsageObservation{{Model: "free", Timestamp: at, TokensIn: 7}}, []Rate{{ID: "free", Model: "free", Context: "fixture", EffectiveFrom: at.Add(-time.Hour)}}, "fixture", nil)
	if free.Components == nil || free.Components.PricedTokens != 7 || free.Cost != 0 {
		t.Fatal("explicit free rate became missing split")
	}
}
