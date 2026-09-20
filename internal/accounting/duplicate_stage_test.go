package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestDuplicateUsageStagingUsesRealParserAndCurrentLedger(t *testing.T) {
	ctx := context.Background()
	failCommit := false
	s, err := store.Open(t.TempDir(), store.Options{BeforeCommit: func() error {
		if failCommit {
			return store.ErrCapacity
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var sessions []store.Session
	var rows []store.UsageObservation
	for _, id := range []string{"original", "replacement"} {
		src := protocol.Source{MachineID: "m", SourceID: id, Generation: "g", GenerationSequence: 1, Provider: "codex", NativeID: "thread"}
		body := []byte(`{"timestamp":"2026-09-01T01:02:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10}}}}` + "\n")
		if id == "replacement" {
			body = append([]byte("\n"), body...)
		}
		h := sha256.Sum256(body)
		src.Size = int64(len(body))
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(body)), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		x := indexer.Indexer{Store: s}
		if _, err := x.Once(ctx, src.SourceID, src.Generation); err != nil {
			t.Fatal(err)
		}
		session, err := s.GetSession(ctx, parser.SessionID(src))
		if err != nil {
			t.Fatal(err)
		}
		usage, err := s.GetUsage(ctx, session.ID, "", 10)
		if err != nil || len(usage) != 1 {
			t.Fatal(usage, err)
		}
		sessions = append(sessions, session)
		rows = append(rows, usage[0])
	}
	id, err := StageDuplicateUsage(ctx, s, sessions[0].ID, sessions[1].ID, rows[0], rows[1])
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := StageDuplicateUsage(ctx, s, sessions[1].ID, sessions[0].ID, rows[1], rows[0])
	if err != nil || reverse != id {
		t.Fatal("reverse retry duplicated proof", reverse, id, err)
	}
	if _, current, err := s.UsageReconciliationProofCurrent(ctx, id); err != nil || !current {
		t.Fatal(current, err)
	}
	owner, excluded := 0, 1
	if sessions[owner].ID > sessions[excluded].ID {
		owner, excluded = excluded, owner
	}
	beforeSelection, err := s.UsageContributions(ctx, sessions[excluded].ID, "", 1, "")
	if err != nil || len(beforeSelection.Items) != 1 || !beforeSelection.Items[0].Counted {
		t.Fatal("unselected usage excluded", beforeSelection, err)
	}
	headBefore, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failCommit = true
	if err := SelectDuplicateUsage(ctx, s, id); !errors.Is(err, store.ErrCapacity) {
		t.Fatal("selection fault not returned", err)
	}
	failCommit = false
	if _, active, err := s.CurrentUsageContributionExclusion(ctx, rows[excluded].ID); err != nil || active {
		t.Fatal("failed selection visible", active, err)
	}
	if head, err := s.ChangeHead(ctx); err != nil || head != headBefore {
		t.Fatal("failed selection emitted a change", head, err)
	}
	if err := SelectDuplicateUsage(ctx, s, id); err != nil {
		t.Fatal(err)
	}
	headSelected, err := s.ChangeHead(ctx)
	if err != nil || headSelected <= headBefore {
		t.Fatal("committed selection not observable", headSelected, err)
	}
	if err := SelectDuplicateUsage(ctx, s, id); err != nil {
		t.Fatal("selection retry", err)
	}
	if head, err := s.ChangeHead(ctx); err != nil || head != headSelected {
		t.Fatal("selection retry duplicated change", head, err)
	}
	if _, active, err := s.CurrentUsageContributionExclusion(ctx, rows[owner].ID); err != nil || active {
		t.Fatal("owner excluded", active, err)
	}
	if exclusion, active, err := s.CurrentUsageContributionExclusion(ctx, rows[excluded].ID); err != nil || !active || exclusion.Observation.ID != rows[excluded].ID || exclusion.OwnerSessionID != sessions[owner].ID {
		t.Fatal("wrong exclusion", exclusion, active, err)
	}
	if _, err := s.UsageContributions(ctx, sessions[excluded].ID, "", 1, beforeSelection.Snapshot); !errors.Is(err, store.ErrHistoryChanged) {
		t.Fatal("mixed accounting snapshot accepted", err)
	}
	selectedPage, err := s.UsageContributions(ctx, sessions[excluded].ID, "", 1, "")
	if err != nil || len(selectedPage.Items) != 1 || selectedPage.Items[0].Counted || selectedPage.Items[0].ExcludedByProof != id || selectedPage.Items[0].Recorded.TokensIn != rows[excluded].TokensIn || selectedPage.NextCursor != "" {
		t.Fatal("correction query", selectedPage, err)
	}
	totals, err := s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil || totals.Sessions != 2 || totals.Recorded.Total != 220 || totals.Excluded.Total != 110 || totals.Counted.Total != 110 || totals.ActiveExclusions != 1 || totals.StaleSelections != 0 {
		t.Fatal("contribution aggregate", totals, err)
	}
	if totals.Recorded.TokensIn != 100 || totals.Excluded.TokensCache != 50 || totals.Counted.TokensOut != 10 {
		t.Fatal("bucket attribution changed", totals)
	}
	emptyTotals, err := s.ContributionTotals(ctx, store.SessionQuery{Provider: "claude"})
	if err != nil || emptyTotals.Sessions != 0 || emptyTotals.Recorded.Total != 0 || emptyTotals.ActiveExclusions != 0 {
		t.Fatal("aggregate filter ignored", emptyTotals, err)
	}
	selectedTotals, err := s.ContributionTotals(ctx, store.SessionQuery{MachineID: "m", Provider: "codex"})
	if err != nil || !reflect.DeepEqual(totals, selectedTotals) {
		t.Fatal("same filtered aggregate differs", selectedTotals, err)
	}
	for _, before := range sessions {
		after, err := s.GetSession(ctx, before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("staging changed accounting", err)
		}
	}
	bad := rows[1]
	bad.TokensIn++
	if _, err := StageDuplicateUsage(ctx, s, sessions[0].ID, sessions[1].ID, rows[0], bad); !errors.Is(err, store.ErrInvalid) {
		t.Fatal("unsupported adjustment accepted", err)
	}
	// A normal append must not resurrect already-verified duplicate usage.
	st, err := s.SourceState(ctx, sessions[owner].SourceID, sessions[owner].Generation)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("\n")
	h := sha256.Sum256(body)
	st.Source.Size = st.DurableOffset + 1
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: st.Source, Offset: st.DurableOffset, Length: 1, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	x := indexer.Indexer{Store: s}
	if _, err = x.Once(ctx, st.Source.SourceID, st.Source.Generation); err != nil {
		t.Fatal(err)
	}
	if _, active, err := s.CurrentUsageContributionExclusion(ctx, rows[excluded].ID); err != nil || !active {
		t.Fatal("normal append invalidated unchanged evidence", active, err)
	}
	if _, err := s.UsageContributions(ctx, sessions[excluded].ID, "", 1, selectedPage.Snapshot); !errors.Is(err, store.ErrHistoryChanged) {
		t.Fatal("owner append did not invalidate pagination", err)
	}
	freshPage, err := s.UsageContributions(ctx, sessions[excluded].ID, "", 1, "")
	if err != nil || len(freshPage.Items) != 1 || freshPage.Items[0].Counted {
		t.Fatal("normal append recounted a duplicate", freshPage, err)
	}
	staleTotals, err := s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil || !reflect.DeepEqual(staleTotals, totals) {
		t.Fatal("normal append changed historical totals", staleTotals, totals, err)
	}
	appendHead, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rescanned, err := ReconcileDuplicateUsagePage(ctx, s, sessions[owner].ID, sessions[excluded].ID, "", "", 100)
	if err != nil || rescanned.Matched != 1 || rescanned.NextCursor != "" {
		t.Fatal("rescan after append", rescanned, err)
	}
	if head, err := s.ChangeHead(ctx); err != nil || head != appendHead {
		t.Fatal("append caused redundant reconciliation writes", head, appendHead, err)
	}
	if kept, active, err := s.CurrentUsageContributionExclusion(ctx, rows[excluded].ID); err != nil || !active || kept.ProofID != id {
		t.Fatal("append replaced a valid proof", kept, active, err)
	}
}
