package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"testing"
	"time"
)

func TestAutomaticPricingRestartReindexPolicyRace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	src := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "g", Provider: "claude"}
	var offset int64
	appendRecord := func() {
		b := []byte("{\"type\":\"assistant\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"id\":\"r\",\"model\":\"known\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n")
		h := sha256.Sum256(b)
		src.Size = offset + int64(len(b))
		if _, e := s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: offset, Length: int64(len(b)), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(b)); e != nil {
			t.Fatal(e)
		}
		offset = src.Size
		x := indexer.Indexer{Store: s}
		if _, e := x.Once(ctx, src.SourceID, src.Generation); e != nil {
			t.Fatal(e)
		}
	}
	appendRecord()
	id := parser.SessionID(src)
	for _, catalog := range []string{"a", "b"} {
		if e := SaveCatalog(ctx, s, Catalog{ID: catalog, Rates: []Rate{{ID: catalog, Model: "known", Context: "test", EffectiveFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Source: "fixture", Input: 1, Output: 1}}}); e != nil {
			t.Fatal(e)
		}
	}
	if e := EnableAutomatic(ctx, s, id, "a", "test"); e != nil {
		t.Fatal(e)
	}
	old, e := s.NextPricingJob(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e := s.FinishPricingJob(ctx, old, errors.New("fixture temporary failure")); e != nil {
		t.Fatal(e)
	}
	status, e := s.PricingPolicy(ctx, id)
	if e != nil || status.Problem != "fixture temporary failure" {
		t.Fatal("failure not visible", status, e)
	}
	if _, e := s.NextPricingJob(ctx); !errors.Is(e, store.ErrNotFound) {
		t.Fatal("failed job busy loop", e)
	}
	if e := EnableAutomatic(ctx, s, id, "b", "test"); e != nil {
		t.Fatal(e)
	}
	if _, e := reprice(ctx, s, id, "a", "test", nil, &old); !errors.Is(e, store.ErrConflict) {
		t.Fatal("stale policy published", e)
	}
	if e := s.FinishPricingJob(ctx, old, nil); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = store.Open(dir, store.Options{})
	if e != nil {
		t.Fatal(e)
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	got, e := s.GetSession(ctx, id)
	if e != nil || got.Pricing == nil || got.Pricing.CatalogID != "b" {
		t.Fatal(got, e)
	}
	if worked, e := PriceNext(ctx, s); worked || e != nil {
		t.Fatal("queue did not drain", worked, e)
	}
	appendRecord()
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal("index did not enqueue", worked, e)
	}
	if e := EnableAutomatic(ctx, s, id, "a", "test"); e != nil {
		t.Fatal(e)
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	if e := EnableAutomatic(ctx, s, id, "b", "test"); e != nil {
		t.Fatal(e)
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	got, e = s.GetSession(ctx, id)
	if e != nil || got.Pricing == nil || got.Pricing.CatalogID != "b" || got.TokensIn != 10 {
		t.Fatal("existing snapshot not reselected or tokens changed", got, e)
	}
	if e := s.DisablePricingPolicy(ctx, id); e != nil {
		t.Fatal(e)
	}
	appendRecord()
	if worked, e := PriceNext(ctx, s); worked || e != nil {
		t.Fatal("disabled policy queued", worked, e)
	}
}
