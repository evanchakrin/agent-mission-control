package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestCalendarTierCostsFollowImmutableSelectedRateEvidence(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	body := []byte("tier accounting fixture\n")
	hash := sha256.Sum256(body)
	source := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "g", GenerationSequence: 1, Provider: "claude", Size: int64(len(body))}
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: source, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	usage := []store.UsageObservation{}
	for i, model := range []string{"flag-alias", "middle", "unknown-tier", "unpriced"} {
		usage = append(usage, store.UsageObservation{ID: model, AgentID: model, Model: model, Timestamp: at, TokensIn: int64((i + 1) * 10), Kind: "message-final", CounterScope: "message"})
	}
	if err = s.CommitIndex(ctx, store.IndexBatch{SourceID: "source", Generation: "g", ToOffset: int64(len(body)), Session: store.Session{ID: "session", LastActivity: at, TokensIn: 100}, Usage: usage}); err != nil {
		t.Fatal(err)
	}
	rates := []Rate{}
	for i, model := range []string{"flagship-model", "middle", "unknown-tier"} {
		r := Rate{ID: model, Model: model, Context: "fixture", EffectiveFrom: at.Add(-time.Hour), Source: "synthetic fixture", Input: 1}
		if i == 0 {
			r.Tier = "flagship"
			r.Aliases = []string{"flag-alias"}
		}
		if i == 1 {
			r.Tier = "mid"
		}
		rates = append(rates, r)
	}
	if err = SaveCatalog(ctx, s, Catalog{ID: "tier-v1", Rates: rates}); err != nil {
		t.Fatal(err)
	}
	original, err := Reprice(ctx, s, "session", "tier-v1", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := store.CalendarQuery{Start: "2026-01-01", End: "2026-01-02", Timezone: "UTC"}
	page, err := s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	d := page.Days[0]
	rhythm, err := s.Rhythm(ctx, store.RhythmQuery{Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	bucket := rhythm.Hours[12]
	if bucket.Sessions != 1 || bucket.RecordedTokens != 100 || bucket.PricedTokens != 60 || bucket.TierPricedTokens != 30 || bucket.ClassifiedCost == nil || math.Abs(*bucket.ClassifiedCost-0.00003) > 1e-12 || bucket.TopTierCost == nil || math.Abs(*bucket.TopTierCost-0.00001) > 1e-12 {
		t.Fatal("rhythm mixed classified and unknown tier costs", bucket)
	}
	if d.RecordedTokens != 100 || d.PricedTokens != 60 || d.TierPricedTokens != 30 || d.TopTierCost == nil || math.Abs(*d.TopTierCost-0.00001) > 1e-12 || d.Sessions != 1 {
		t.Fatalf("wrong partial tier attribution: %+v", d)
	}
	rates[0].Tier = "cheap"
	if err = SaveCatalog(ctx, s, Catalog{ID: "tier-v1", Rates: rates}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("historical classification overwritten", err)
	}
	if err = SaveCatalog(ctx, s, Catalog{ID: "tier-v2", Rates: rates}); err != nil {
		t.Fatal(err)
	}
	// Importing a new card alone cannot alter the selected historical estimate.
	page, err = s.Calendar(ctx, q)
	if err != nil || page.Days[0].TopTierCost == nil || *page.Days[0].TopTierCost != *d.TopTierCost {
		t.Fatal("catalog import repriced history", page, err)
	}
	updated, err := Reprice(ctx, s, "session", "tier-v2", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err = s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	d = page.Days[0]
	if d.TopTierCost == nil || *d.TopTierCost != 0 || d.TierPricedTokens != 30 || d.RecordedTokens != 100 || updated.Estimate.Cost != original.Estimate.Cost {
		t.Fatal("classification changed tokens or cost, or known zero became unknown", d)
	}
	stored, err := s.EstimateHistory(ctx, "session", "", 100)
	if err != nil || len(stored) != 2 {
		t.Fatal("prior estimate not preserved", len(stored), err)
	}
}

func TestRateTierValidationDoesNotInferUnknownClassifications(t *testing.T) {
	r := Rate{ID: "r", Model: "m", Context: "fixture", EffectiveFrom: time.Now(), Source: "fixture"}
	for _, tier := range []string{"", "unknown", "flagship", "premium", "mid", "cheap"} {
		r.Tier = tier
		if err := Validate([]Rate{r}); err != nil {
			t.Fatal(tier, err)
		}
	}
	r.Tier = "expensive-ish"
	if Validate([]Rate{r}) == nil {
		t.Fatal("invalid classification accepted")
	}
}
