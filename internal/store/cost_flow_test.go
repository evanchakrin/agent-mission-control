package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCostFlowRejectsCheckpointAdvanceWithUnchangedTotals(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\ny\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 2)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.SessionCostFlow(ctx, "session-1", "", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a durable checkpoint-only advance with the projection unchanged.
	if _, err = s.db.ExecContext(ctx, "UPDATE sources SET indexed_offset=4 WHERE source_id=? AND generation=?", src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionCostFlow(ctx, "session-1", "", 100, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("checkpoint change accepted", err)
	}
}

func TestCostFlowPinsWholeSessionAccounting(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 1)
	page, err := s.SessionCostFlow(ctx, "s-000000", "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Session.ID != "s-000000" || page.Session.TokensOut != 2 || len(page.Agents) != 1 || len(page.Snapshot) != 64 {
		t.Fatal(page)
	}
	if _, err = s.SessionCostFlow(ctx, "s-000000", "", 1, "stale"); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	if _, err = s.SessionCostFlow(ctx, "s-000000", "cursor", 1, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = s.SessionCostFlow(ctx, "s-000000", "", 101, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	changed := page.Session
	changed.Metadata.Archived = true
	if s.costFlowSnapshot(changed) != s.costFlowSnapshot(page.Session) {
		t.Fatal("archive invalidated accounting")
	}
	changed.TokensOut++
	b, _ := json.Marshal(changed)
	if _, err = s.db.ExecContext(ctx, "UPDATE sessions SET projection=? WHERE id=?", b, changed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionCostFlow(ctx, changed.ID, "", 1, page.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("append accepted stale totals", err)
	}
	changed = page.Session
	changed.Pricing = &SessionPricing{SnapshotID: "new-rate-estimate"}
	if s.costFlowSnapshot(changed) == s.costFlowSnapshot(page.Session) {
		t.Fatal("repricing retained old snapshot")
	}
	prior := s.costFlowSnapshot(page.Session)
	s.epoch = "restored"
	if s.costFlowSnapshot(page.Session) == prior {
		t.Fatal("restore retained old snapshot")
	}
}
