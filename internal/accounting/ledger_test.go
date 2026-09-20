package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// All transcripts and prices in this integration test are generated fixtures.
// It exercises the actual raw receipt -> checkpoint -> usage -> estimate path.
func TestDurablePartialUsageZeroRepricingAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pricingFault := false
	s, err := store.Open(dir, store.Options{BeforeCommit: func() error {
		if pricingFault {
			return errors.New("injected pricing commit failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	source := protocol.Source{MachineID: "fixture-machine", SourceID: "fixture-source", Generation: "g", GenerationSequence: 1, Provider: "claude"}
	offset := int64(0)
	appendLines := func(lines ...string) {
		t.Helper()
		body := []byte(strings.Join(lines, "\n") + "\n")
		hash := sha256.Sum256(body)
		source.Size = offset + int64(len(body))
		_, err := s.IngestChunk(ctx, protocol.Chunk{Source: source, Offset: offset, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		offset += int64(len(body))
		x := indexer.Indexer{Store: s}
		if n, err := x.Once(ctx, source.SourceID, source.Generation); err != nil || n != int64(len(lines)) {
			t.Fatalf("index n=%d err=%v", n, err)
		}
		checkpoint, err := s.SourceState(ctx, source.SourceID, source.Generation)
		if err != nil || checkpoint.IndexedOffset != offset {
			t.Fatal("checkpoint not durable", checkpoint, err)
		}
	}
	appendLines(
		`{"timestamp":"2026-01-01T10:00:00Z","type":"assistant","uuid":"a","message":{"id":"reply","model":"known","usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"output_tokens":8},"content":"fixture"}}`,
		`{"timestamp":"2026-01-01T10:00:01Z","type":"assistant","uuid":"b","message":{"id":"unknown","model":"no-rate","usage":{"input_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
		`{"timestamp":"2026-01-01T10:00:02Z","type":"assistant","uuid":"c","message":{"id":"partial","model":"known","usage":{"input_tokens":5,"output_tokens":0}}}`,
	)
	id := parser.SessionID(source)
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := Rate{ID: "rate-v1", Model: "known", Context: "standard", EffectiveFrom: at, Source: "sanitized-fixture", Input: 1, CacheRead: 1, CacheWrite: 1, Output: 1}
	if err := SaveCatalog(ctx, s, Catalog{ID: "catalog-v1", Rates: []Rate{rate}}); err != nil {
		t.Fatal(err)
	}
	headBefore, _ := s.ChangeHead(ctx)
	pricingFault = true
	if _, err = Reprice(ctx, s, id, "catalog-v1", "standard", nil); err == nil {
		t.Fatal("pricing commit failure was ignored")
	}
	failedHistory, err := s.EstimateHistory(ctx, id, "", 100)
	if err != nil || len(failedHistory) != 0 {
		t.Fatalf("failed estimate became durable: %v %v", failedHistory, err)
	}
	failedRow, err := s.GetSession(ctx, id)
	if err != nil || failedRow.Pricing != nil || failedRow.CostEstimate != nil {
		t.Fatalf("failed price became visible: %+v %v", failedRow, err)
	}
	headAfter, _ := s.ChangeHead(ctx)
	if headAfter != headBefore {
		t.Fatal("failed price emitted a change")
	}
	pricingFault = false
	first, err := Reprice(ctx, s, id, "catalog-v1", "standard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Estimate.RecordedTokens != 37 || first.Estimate.PricedTokens != 24 || first.Estimate.UnpricedTokens != 13 || first.Estimate.UnattributedTokens != 5 {
		t.Fatalf("incomplete coverage called exact: %+v", first)
	}
	visible, err := s.GetSession(ctx, id)
	if err != nil || visible.CostEstimate == nil || *visible.CostEstimate != first.Estimate.Cost || visible.Pricing == nil || visible.Pricing.SnapshotID != first.ID || visible.Pricing.PricedTokens != 24 || visible.Pricing.UnpricedTokens != 13 {
		t.Fatalf("price/coverage not published: %+v %v", visible, err)
	}
	totals, err := s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.CostEstimate == nil || *totals.CostEstimate != first.Estimate.Cost {
		t.Fatalf("catalog price not committed: %+v %v", totals, err)
	}
	if totals.RecordedTokens != 37 || totals.PricedTokens != 24 || totals.UnpricedTokens != 13 || totals.KnownUnattributedTokens != 5 || totals.TokensAwaitingPricing != 0 {
		t.Fatalf("published aggregate coverage wrong: %+v", totals)
	}
	headAfter, _ = s.ChangeHead(ctx)
	if headAfter <= headBefore {
		t.Fatal("price publication omitted change notification")
	}
	appendLines(`{"type":"assistant","uuid":"d","message":{"id":"reply","usage":{"output_tokens":0}}}`)
	visible, err = s.GetSession(ctx, id)
	if err != nil || visible.Pricing != nil || visible.CostEstimate != nil {
		t.Fatalf("new evidence retained stale price: %+v %v", visible, err)
	}
	totals, err = s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || totals.RecordedTokens != 29 || totals.PricedTokens != 0 || totals.TokensAwaitingPricing != 29 {
		t.Fatalf("new evidence retained aggregate coverage: %+v %v", totals, err)
	}
	second, err := Reprice(ctx, s, id, "catalog-v1", "standard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Estimate.RecordedTokens != 29 || second.Estimate.PricedTokens != 16 || second.ID == first.ID {
		t.Fatalf("zero/partial revision failed: %+v", second)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendLines(`{"type":"assistant","uuid":"e","message":{"id":"reply","usage":{"input_tokens":0}}}`)
	rows, err := s.GetUsage(ctx, id, "", 100)
	if err != nil || len(rows) != 3 {
		t.Fatal("message updates duplicated after restart", rows, err)
	}
	found := false
	for _, u := range rows {
		if u.Kind == "message-final" && u.Model == "known" {
			found = true
			if u.TokensIn != 0 || u.TokensCache != 4 || u.TokensCacheWrite != 2 || u.TokensOut != 0 || u.Timestamp.IsZero() || len(u.Present) != 5 {
				t.Fatalf("omitted fields wiped known evidence: %+v", u)
			}
		}
	}
	if !found {
		t.Fatal("complete merged observation missing")
	}
	before, _ := json.Marshal(rows)
	third, err := Reprice(ctx, s, id, "catalog-v1", "standard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Estimate.RecordedTokens != 19 || third.Estimate.PricedTokens != 6 || third.Estimate.UnpricedTokens != 13 {
		t.Fatalf("wrong restarted price: %+v", third)
	}
	rate.ID = "rate-v2"
	rate.Input = 2
	rate.CacheRead = 2
	rate.CacheWrite = 2
	rate.Output = 2
	if err := SaveCatalog(ctx, s, Catalog{ID: "catalog-v2", Rates: []Rate{rate}}); err != nil {
		t.Fatal(err)
	}
	repriced, err := Reprice(ctx, s, id, "catalog-v2", "standard", nil)
	if err != nil || repriced.Estimate.Cost != third.Estimate.Cost*2 || repriced.ID == third.ID {
		t.Fatal("new catalog not independent", repriced, err)
	}
	// Retrying an older completed historical request cannot replace the newer
	// selected catalog. A current-rate comparison is history only, not selection.
	if _, err = Reprice(ctx, s, id, "catalog-v1", "standard", nil); err != nil {
		t.Fatal(err)
	}
	comparisonTime := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	comparison, err := Reprice(ctx, s, id, "catalog-v1", "standard", &comparisonTime)
	if err != nil {
		t.Fatal(err)
	}
	visible, err = s.GetSession(ctx, id)
	if err != nil || visible.Pricing == nil || visible.Pricing.SnapshotID != repriced.ID || visible.CostEstimate == nil || *visible.CostEstimate != repriced.Estimate.Cost {
		t.Fatalf("comparison/retry replaced historical selection: %+v %v", visible, err)
	}
	afterRows, err := s.GetUsage(ctx, id, "", 100)
	after, _ := json.Marshal(afterRows)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repricing mutated token/model evidence", err)
	}
	if err := SaveCatalog(ctx, s, Catalog{ID: "catalog-v1", Rates: []Rate{rate}}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("historical catalog overwritten", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.EstimateHistory(ctx, id, "", 100)
	if err != nil || len(history) != 5 {
		t.Fatal("estimate history lost on restart", len(history), err)
	}
	wanted := map[string]Snapshot{first.ID: first, second.ID: second, third.ID: third, repriced.ID: repriced, comparison.ID: comparison}
	for _, raw := range history {
		var value Snapshot
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, wanted[value.ID]) {
			t.Fatal("immutable estimate changed", value)
		}
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err = s.Backup(ctx, backupDir); err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreBackup(ctx, backupDir, filepath.Join(t.TempDir(), "restored"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredRow, err := restored.GetSession(ctx, id)
	if err != nil || restoredRow.Pricing == nil || restoredRow.Pricing.SnapshotID != repriced.ID {
		t.Fatalf("pricing selection lost on restore: %+v %v", restoredRow, err)
	}
}
