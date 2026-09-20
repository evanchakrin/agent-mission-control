package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestComparisonOnlyPreservesSelectedHistoryAndBlocksWorker(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body := []byte("{\"type\":\"assistant\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"id\":\"fixture\",\"model\":\"known\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n")
	src := protocol.Source{MachineID: "fixture", SourceID: "source", Generation: "g", Provider: "claude", Size: int64(len(body))}
	hash := sha256.Sum256(body)
	if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	x := indexer.Indexer{Store: s}
	if _, err := x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	id := parser.SessionID(src)
	before, err := s.GetUsage(ctx, id, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	rate := Rate{ID: "fixture-rate", Model: "known", Context: "test", Source: "fixture-only", EffectiveFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Input: 1, Output: 1}
	c := Catalog{ID: "historical-fixture", Rates: []Rate{rate}}
	if err := SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	historical, err := Reprice(ctx, s, id, c.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = "comparison-only-fixture"
	c.ComparisonOnly = true
	c.Rates[0].Input = 2
	c.Rates[0].Output = 2
	if err := SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	comparison, err := Reprice(ctx, s, id, c.ID, "test", &at)
	if err != nil || comparison.Estimate.Cost != historical.Estimate.Cost*2 {
		t.Fatal(comparison, err)
	}
	session, err := s.GetSession(ctx, id)
	if err != nil || session.CostEstimate == nil || *session.CostEstimate != historical.Estimate.Cost {
		t.Fatal("comparison replaced selected historical estimate", session.CostEstimate, err)
	}
	if err := s.InitializePricingQueue(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate an invalid persisted policy, bypassing the public enable guard.
	if err := s.SetPricingPolicy(ctx, id, c.ID, "test"); err != nil {
		t.Fatal(err)
	}
	job, err := s.NextPricingJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := priceBatch(ctx, s, job); !errors.Is(err, store.ErrInvalid) {
		t.Fatal("worker accepted comparison-only policy", err)
	}
	after, err := s.GetUsage(ctx, id, "", 100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("pricing changed recorded usage", err)
	}
	// Selecting an already-saved manual comparison must publish its pointer too.
	if err := EnableAutomaticAt(ctx, s, id, c.ID, "test", &at); err != nil {
		t.Fatal(err)
	}
	if worked, err := PriceNext(ctx, s); !worked || err != nil {
		t.Fatal(worked, err)
	}
	totals, err := s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.CurrentComparisons == nil || totals.CurrentComparisons.CostEstimate == nil || *totals.CurrentComparisons.CostEstimate != comparison.Estimate.Cost {
		t.Fatal(totals.CurrentComparisons, err)
	}
}

func TestComparisonOnlyCatalogRejectsHistoricalEntryPoints(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	c := Catalog{ID: "comparison-fixture", ComparisonOnly: true, Rates: []Rate{{ID: "rate", Model: "fixture", Context: "test", Source: "fixture-only", EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Input: 1, Output: 1}}}
	if err := SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	if _, err := Reprice(ctx, s, "not-a-session", c.ID, "test", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("historical pricing must reject before session lookup: %v", err)
	}
	if err := EnableAutomatic(ctx, s, "not-a-session", c.ID, "test"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("automatic pricing must reject before policy creation: %v", err)
	}
	at := c.Rates[0].EffectiveFrom
	if _, err := Reprice(ctx, s, "not-a-session", c.ID, "test", &at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("explicit comparison should reach session lookup: %v", err)
	}
	if err := validateCatalogUse(c, nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatal(err)
	}
	c.ComparisonOnly = false
	if err := validateCatalogUse(c, nil); err != nil {
		t.Fatal("legacy catalog compatibility", err)
	}
}

func TestPublishedBaseline20260909(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "backend-v2", "rates", "published-api-baseline-observed-20260909.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Catalog
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if !c.ComparisonOnly || len(c.Rates) != 11 {
		t.Fatal("catalog scope changed")
	}
	if err = validateCatalogUse(c, &at); err != nil {
		t.Fatal(err)
	}
	if validateCatalogUse(c, nil) == nil {
		t.Fatal("historical pricing allowed")
	}
	for _, tc := range []struct {
		model string
		cost  float64
	}{
		{"gpt-daybreak-blue-latest", 29.4}, {"claude-fable-5-1", 72.75},
		{"claude-sonnet-5", 14.7}, {"gpt-5.6-luna", 1.67},
	} {
		u := []store.UsageObservation{{Model: tc.model, TokensIn: 1000000, TokensCache: 1000000, TokensCacheWrite: 1000000, TokensOut: 1000000}}
		p := Price(u, c.Rates, c.Rates[0].Context, &at)
		if p.Cost != tc.cost || p.PricedTokens != 4000000 || u[0].TokensIn != 1000000 {
			t.Fatal(tc.model, p)
		}
	}
	u := []store.UsageObservation{{Model: "codex-auto-review", TokensIn: 10}}
	if p := Price(u, c.Rates, c.Rates[0].Context, &at); p.PricedTokens != 0 || p.UnpricedTokens != 10 {
		t.Fatal(p)
	}
}

func TestObservedOpenAICatalogIsExplicitComparisonOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "backend-v2", "rates", "openai-observed-20260906.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !c.ComparisonOnly || c.ID != "openai-observed-20260906" || len(c.Rates) != 32 {
		t.Fatal("catalog scope changed")
	}
	at := time.Date(2026, 9, 6, 19, 16, 28, 0, time.UTC)
	if err := validateCatalogUse(c, &at); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Rates {
		if r.Source != "https://developers.openai.com/api/docs/pricing" || !r.EffectiveFrom.Equal(at) || r.EffectiveTo == nil || !r.EffectiveTo.Equal(at.Add(24*time.Hour)) {
			t.Fatal("missing observed rate provenance", r.ID)
		}
	}
	usage := []store.UsageObservation{{Model: "gpt-daybreak-blue-latest", TokensIn: 1000000, TokensOut: 1000000, TokensCache: 1000000, TokensCacheWrite: 1000000}}
	short := Price(usage, c.Rates, "openai-api-standard-short-nonregional", &at)
	long := Price(usage, c.Rates, "openai-api-standard-long-nonregional", &at)
	if short.Cost != 29.4 || long.Cost != 48.8 || short.PricedTokens != 4000000 {
		t.Fatal(short, long)
	}
	unknown := Price(usage, c.Rates, "unspecified", &at)
	if unknown.PricedTokens != 0 || unknown.UnpricedTokens != 4000000 {
		t.Fatal("unknown context priced", unknown)
	}
	outside := at.Add(48 * time.Hour)
	if p := Price(usage, c.Rates, "openai-api-standard-short-nonregional", &outside); p.PricedTokens != 0 {
		t.Fatal("observation window ignored", p)
	}
}
