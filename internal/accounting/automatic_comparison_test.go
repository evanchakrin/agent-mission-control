package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// A tiny 501-record fixture crosses one worker page; it is not a capacity test.
func TestAutomaticComparisonRestartAndPolicyRace(t *testing.T) {
	testAutomaticComparisonRecovery(t, false)
}

func TestAutomaticComparisonBackupRestore(t *testing.T) {
	testAutomaticComparisonRecovery(t, true)
}

func testAutomaticComparisonRecovery(t *testing.T, restore bool) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	var body bytes.Buffer
	for i := 0; i < 501; i++ {
		fmt.Fprintf(&body, "{\"type\":\"assistant\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"id\":\"r%d\",\"model\":\"known\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n", i)
	}
	src := protocol.Source{MachineID: "fixture", SourceID: "comparison", Generation: "g", Provider: "claude", Size: int64(body.Len())}
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
	c := Catalog{ID: "historical", Rates: []Rate{{ID: "rate", Model: "known", Context: "test", Source: "fixture", EffectiveFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Input: 1, Output: 1}}}
	if err = SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	historical, err := Reprice(ctx, s, id, c.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = "comparison"
	c.ComparisonOnly = true
	c.Rates[0].Input = 2
	c.Rates[0].Output = 2
	if err = SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err = EnableAutomaticAt(ctx, s, id, c.ID, "test", &at); err != nil {
		t.Fatal(err)
	}
	j, err := s.NextPricingJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if done, e := priceBatch(ctx, s, j); done || e != nil {
		t.Fatal("unbounded first page", done, e)
	}
	backup := filepath.Join(t.TempDir(), "comparison-backup")
	if restore {
		if _, err = s.Backup(ctx, backup); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if restore {
		s, err = store.RestoreBackup(ctx, backup, filepath.Join(t.TempDir(), "restored-comparison"), store.Options{})
	} else {
		s, err = store.Open(dir, store.Options{})
	}
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.NextPricingJob(ctx)
	if err != nil || !sameComparison(resumed.ComparisonAt, &at) || len(resumed.Checkpoint) == 0 {
		t.Fatal(resumed, err)
	}
	wrong := resumed
	wrongDate := at.Add(time.Minute)
	wrong.ComparisonAt = &wrongDate
	if _, e := priceBatch(ctx, s, wrong); e == nil {
		t.Fatal("checkpoint accepted a different comparison date")
	}
	if worked, e := PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	after, err := s.GetSession(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("comparison altered historical session", err)
	}
	rows, err := s.EstimateHistory(ctx, id, "", 100)
	if err != nil || len(rows) != 2 {
		t.Fatal(len(rows), err)
	}
	found := false
	for _, raw := range rows {
		var p Snapshot
		if err = json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.ComparisonAt != nil {
			found = true
			if p.Estimate.Cost != historical.Estimate.Cost*2 || p.Observations != 501 {
				t.Fatal(p)
			}
		}
	}
	if !found {
		t.Fatal("comparison not saved")
	}
	if restore {
		publishedBackup := filepath.Join(t.TempDir(), "published-comparison-backup")
		if _, err = s.Backup(ctx, publishedBackup); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = store.RestoreBackup(ctx, publishedBackup, filepath.Join(t.TempDir(), "restored-published-comparison"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		p, e := s.PricingPolicy(ctx, id)
		if e != nil || !sameComparison(p.ComparisonAt, &at) {
			t.Fatal("restored comparison policy", p, e)
		}
		row, e := s.GetSession(ctx, id)
		if e != nil || !reflect.DeepEqual(before, row) {
			t.Fatal("restored historical projection changed", e)
		}
	}
	totals, err := s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.CurrentComparisons == nil || totals.CurrentComparisons.SessionsWithComparison != 1 || totals.CurrentComparisons.PricedTokens != 501*12 || totals.CurrentComparisons.CostEstimate == nil || *totals.CurrentComparisons.CostEstimate != historical.Estimate.Cost*2 {
		t.Fatal("current comparison aggregate", totals.CurrentComparisons, err)
	}
	filtered, err := s.CatalogTotals(ctx, store.SessionQuery{Text: "does-not-match-this-session"})
	if err != nil || filtered.CurrentComparisons.Sessions != 0 || filtered.CurrentComparisons.CostEstimate != nil {
		t.Fatal("comparison ignored filters", filtered.CurrentComparisons, err)
	}
	if _, err = s.NextPricingJob(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("queue not drained", err)
	}
	if err = EnableAutomaticAt(ctx, s, id, c.ID, "test", &at); err != nil {
		t.Fatal(err)
	}
	old, err := s.NextPricingJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	if err = EnableAutomaticAt(ctx, s, id, c.ID, "test", &later); err != nil {
		t.Fatal(err)
	}
	if _, err = priceBatch(ctx, s, old); !errors.Is(err, store.ErrConflict) {
		t.Fatal("obsolete policy accepted", err)
	}
	// One extra newline advances durable indexing without changing token totals.
	previousSize := src.Size
	src.Size++
	extra := []byte("\n")
	digest := sha256.Sum256(extra)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: previousSize, Length: 1, SHA256: hex.EncodeToString(digest[:])}, bytes.NewReader(extra)); err != nil {
		t.Fatal(err)
	}
	x.Store = s
	if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	totals, err = s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.CurrentComparisons.SessionsWithComparison != 0 || totals.CurrentComparisons.CostEstimate != nil || totals.CurrentComparisons.UnpricedTokens != 501*12 {
		t.Fatal("stale comparison counted", totals.CurrentComparisons, err)
	}
	current, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	views, err := s.SessionResponses(ctx, []store.Session{current})
	if err != nil || len(views) != 1 || len(views[0].APIComparison) != 0 {
		t.Fatal("stale comparison exposed on session", views, err)
	}
}
