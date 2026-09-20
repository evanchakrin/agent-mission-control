package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestPricingPagesResumeAfterBackupAndRejectDuplicateProgress(t *testing.T) {
	for _, legacy := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) { testPricingCheckpointResume(t, legacy) })
	}
}

func testPricingCheckpointResume(t *testing.T, legacy int) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	var body bytes.Buffer
	for i := 0; i < 1101; i++ {
		fmt.Fprintf(&body, "{\"type\":\"assistant\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"id\":\"r%d\",\"model\":\"known\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n", i)
	}
	src := protocol.Source{MachineID: "machine", SourceID: "large", Generation: "g", Provider: "claude", Size: int64(body.Len())}
	h := sha256.Sum256(body.Bytes())
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(body.Bytes())); err != nil {
		t.Fatal(err)
	}
	x := indexer.Indexer{Store: s}
	for {
		n, e := x.Once(ctx, src.SourceID, src.Generation)
		if e != nil {
			t.Fatal(e)
		}
		if n == 0 {
			break
		}
	}
	id := parser.SessionID(src)
	if err = SaveCatalog(ctx, s, Catalog{ID: "catalog", Rates: []Rate{{ID: "fixture", Model: "known", Context: "test", Source: "fixture-only", EffectiveFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Input: 1, Output: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err = EnableAutomatic(ctx, s, id, "catalog", "test"); err != nil {
		t.Fatal(err)
	}
	first, err := s.NextPricingJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if done, e := priceBatch(ctx, s, first); done || e != nil {
		t.Fatal("first page not bounded", done, e)
	}
	second, err := s.NextPricingJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cp pricingCheckpoint
	if err = json.Unmarshal(second.Checkpoint, &cp); err != nil || cp.Snapshot.Estimate.RecordedTokens != 6000 {
		t.Fatal("first page checkpoint", cp, err)
	}
	// New small work gets a turn before this large session's next page.
	small := src
	small.SourceID = "small"
	small.Size = int64(bytes.IndexByte(body.Bytes(), '\n') + 1)
	b := body.Bytes()[:small.Size]
	digest := sha256.Sum256(b)
	if _, e := s.IngestChunk(ctx, protocol.Chunk{Source: small, Length: small.Size, SHA256: hex.EncodeToString(digest[:])}, bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	if _, e := x.Once(ctx, small.SourceID, small.Generation); e != nil {
		t.Fatal(e)
	}
	if e := EnableAutomatic(ctx, s, parser.SessionID(small), "catalog", "test"); e != nil {
		t.Fatal(e)
	}
	next, e := s.NextPricingJob(ctx)
	if e != nil || next.SessionID != parser.SessionID(small) {
		t.Fatal("large pricing monopolized queue", next, e)
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	for _, invalid := range []json.RawMessage{json.RawMessage(`{"after":"present"}`), json.RawMessage(`null`)} {
		bad := second
		bad.Checkpoint = invalid
		if _, e := priceBatch(ctx, s, bad); e == nil {
			t.Fatal("damaged checkpoint accepted")
		}
	}
	if err = s.SavePricingCheckpoint(ctx, first, second.Checkpoint); !errors.Is(err, store.ErrConflict) {
		t.Fatal("duplicate checkpoint accepted", err)
	}
	if err = s.FinishPricingJob(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPricingJob(ctx); err != nil {
		t.Fatal("stale completion deleted progress", err)
	}
	row, err := s.GetSession(ctx, id)
	if err != nil || row.Pricing != nil {
		t.Fatal("partial estimate was published", row, err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if legacy != 0 {
		cp.Version = legacy
		cp.Snapshot.Estimate.Components = nil
		if legacy == 1 {
			cp.Snapshot.ID = ""
			cp.Snapshot.AttributionVersion = 0
			cp.Snapshot.Observations = 0
		} else {
			p := cp.Snapshot
			cp.Snapshot.ID = parser.ID(p.SessionID, p.Generation, p.ProjectionRevision, fmt.Sprint(p.IndexedOffset), p.CatalogID, p.Context, "historical", "attribution-v1")
		}
		oldBytes, e := json.Marshal(cp)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.SavePricingCheckpoint(ctx, second, oldBytes); e != nil {
			t.Fatal("seed legacy checkpoint", e)
		}
	}
	if _, err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.RestoreBackup(ctx, backup, filepath.Join(t.TempDir(), "restored"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	steps := 2
	if legacy != 0 {
		steps = 3
	}
	for i := 0; i < steps; i++ {
		if worked, e := PriceNext(ctx, s); !worked || e != nil {
			t.Fatal("resume", worked, e)
		}
	}
	if worked, e := PriceNext(ctx, s); worked || e != nil {
		t.Fatal("queue still pending", worked, e)
	}
	row, err = s.GetSession(ctx, id)
	if err != nil || row.Pricing == nil || row.Pricing.RecordedTokens != 13212 || row.Pricing.PricedTokens != 13212 || row.TokensIn != 11010 {
		t.Fatal("resumed total wrong", row, err)
	}
	// Whole-session pricing must derive byte-identical immutable snapshot evidence.
	manual, err := Reprice(ctx, s, id, "catalog", "test", nil)
	if err == nil && (manual.Estimate.Components == nil || manual.Estimate.Components.PricedTokens != 13212) {
		t.Fatal("checkpoint migration lost component coverage")
	}
	if err != nil || manual.ID != row.Pricing.SnapshotID || manual.Estimate.Cost != row.Pricing.Cost {
		t.Fatal("batch/manual disagreement", manual, err)
	}
	if err = EnableAutomatic(ctx, s, id, "catalog", "test"); err != nil {
		t.Fatal(err)
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	old, e := s.NextPricingJob(ctx)
	if e != nil || len(old.Checkpoint) == 0 {
		t.Fatal(old, e)
	}
	oldSize := src.Size
	src.Size += int64(len(b))
	if _, e = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: oldSize, Length: int64(len(b)), SHA256: hex.EncodeToString(digest[:])}, bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	x.Store = s
	if _, e = x.Once(ctx, src.SourceID, src.Generation); e != nil {
		t.Fatal(e)
	}
	fresh, e := s.NextPricingJob(ctx)
	if e != nil || fresh.ID == old.ID || len(fresh.Checkpoint) != 0 {
		t.Fatal("changed evidence retained pricing progress", fresh, e)
	}
	if e = s.SavePricingCheckpoint(ctx, old, old.Checkpoint); !errors.Is(e, store.ErrConflict) {
		t.Fatal("old evidence checkpoint accepted", e)
	}
}
