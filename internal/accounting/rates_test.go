package accounting

import (
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"testing"
	"time"
)

func TestImportedRateRequiresExplicitPrices(t *testing.T) {
	var duplicate Rate
	if json.Unmarshal([]byte(`{"Input":1,"Input":2,"CacheRead":0,"CacheWrite":0,"Output":2}`), &duplicate) == nil {
		t.Fatal("duplicate exact price key accepted")
	}
	for _, raw := range []string{`{}`, `{"Input":1,"Output":2}`, `{"Input":1,"CacheRead":null,"CacheWrite":0,"Output":2}`, `{"Input":1,"input":2,"CacheRead":0,"CacheWrite":0,"Output":2}`, `{"Input":1,"CacheRead":0,"CacheWrite":0,"Output":2,"typo":1}`} {
		var r Rate
		if json.Unmarshal([]byte(raw), &r) == nil {
			t.Fatal("ambiguous rate accepted", raw)
		}
	}
	var r Rate
	if err := json.Unmarshal([]byte(`{"Input":0,"CacheRead":0,"CacheWrite":0,"Output":0}`), &r); err != nil {
		t.Fatal("explicit free rates rejected", err)
	}
}

func TestCoverageAndRepricingNeverMutateTokens(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rates := []Rate{{ID: "v1", Model: "known", Context: "standard", EffectiveFrom: at.Add(-time.Hour), Source: "fixture", Input: 1, Output: 2}}
	usage := []store.UsageObservation{{Model: "known", Timestamp: at, TokensIn: 100, TokensOut: 10}, {Model: "unknown", Timestamp: at, TokensIn: 300}}
	before := usage[0]
	e := Price(usage, rates, "standard", nil)
	if e.RecordedTokens != 410 || e.PricedTokens != 110 || e.UnpricedTokens != 300 || e.Coverage >= 1 {
		t.Fatalf("partial pricing called complete: %+v", e)
	}
	rates[0].Input = 100
	_ = Price(usage, rates, "standard", nil)
	if usage[0].TokensIn != before.TokensIn || usage[0].TokensOut != before.TokensOut {
		t.Fatal("repricing changed evidence")
	}
	usage[0].Model = "known-extra"
	if Price(usage, rates, "standard", nil).PricedTokens != 0 {
		t.Fatal("substring pricing used")
	}
}
func TestOverlappingRatesFailClosed(t *testing.T) {
	at := time.Now()
	a := Rate{ID: "a", Model: "x", Context: "standard", Source: "fixture", EffectiveFrom: at}
	b := a
	b.ID = "b"
	if Validate([]Rate{a, b}) == nil {
		t.Fatal("ambiguous rate accepted")
	}
}
