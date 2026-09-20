package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func indexGroupFixture(t *testing.T, s *Store, count int) []IndexBatch {
	t.Helper()
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	batches := make([]IndexBatch, count)
	for i := range batches {
		src := testSource()
		src.SourceID = fmt.Sprintf("group-source-%d", i)
		src.NativeID = fmt.Sprintf("group-native-%d", i)
		ingest(t, s, src, 0, "row\n")
		b := batch(src, 0, 4)
		b.Session.ID = fmt.Sprintf("group-session-%d", i)
		b.Usage = []UsageObservation{{ID: fmt.Sprintf("group-usage-%d", i), AgentID: "main", Model: "fixture", TokensIn: 2, Kind: "message-final"}}
		batches[i] = b
	}
	return batches
}

func TestIndexGroupCommitsEveryCheckpointAndSurvivesReopen(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	batches := indexGroupFixture(t, s, MaxIndexGroupSources)
	if err := s.CommitIndexGroup(ctx, batches); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, batches[0].Session.ID, MetadataPatch{OperationID: "group-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitIndexGroup(ctx, batches); !errors.Is(err, ErrConflict) {
		t.Fatal("stale group was replayed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, b := range batches {
		state, err := reopened.SourceState(ctx, b.SourceID, b.Generation)
		if err != nil || state.DurableOffset != 4 || state.IndexedOffset != 4 {
			t.Fatal("checkpoint not committed", state, err)
		}
		row, err := reopened.GetSession(ctx, b.Session.ID)
		if err != nil || row.TokensIn != 2 {
			t.Fatal("usage lost or duplicated", row.TokensIn, err)
		}
	}
	m, err := reopened.GetMetadata(ctx, batches[0].Session.ID)
	if err != nil || !m.Archived || m.Revision != 1 {
		t.Fatal("stale group changed organization", m, err)
	}
	if err := reopened.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIndexGroupLateFailureRollsBackEarlierBatch(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	batches := indexGroupFixture(t, s, 2)
	batches[1].FromOffset = 1
	if err := s.CommitIndexGroup(ctx, batches); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid late checkpoint accepted", err)
	}
	for _, b := range batches {
		state, err := s.SourceState(ctx, b.SourceID, b.Generation)
		if err != nil || state.IndexedOffset != 0 || state.DurableOffset != 4 {
			t.Fatal("partial group publication", state, err)
		}
		if _, err := s.GetSession(ctx, b.Session.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("uncommitted session visible", err)
		}
	}
	for _, table := range []string{"usage_observations", "query_usage", "changes"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("uncommitted contributions visible", table, count, err)
		}
	}
	if s.pendingBytes != 0 {
		t.Fatal("failed group leaked capacity reservation", s.pendingBytes)
	}
	batches[1].FromOffset = 0
	if err := s.CommitIndexGroup(ctx, batches); err != nil {
		t.Fatal("corrected retry did not recover", err)
	}
}

func TestIndexGroupRejectsUnboundedOrRepeatedSources(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	batches := indexGroupFixture(t, s, 1)
	for _, input := range [][]IndexBatch{nil, make([]IndexBatch, MaxIndexGroupSources+1), {batches[0], batches[0]}} {
		if err := s.CommitIndexGroup(ctx, input); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid group accepted", err)
		}
	}
	oversized := batches[0]
	oversized.Events = make([]Event, 129)
	text := strings.Repeat("x", 64<<10)
	for i := range oversized.Events {
		oversized.Events[i].Text = text
	}
	if err := s.CommitIndexGroup(ctx, []IndexBatch{oversized}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "allowance") {
		t.Fatal("oversized expansion allowance accepted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.CommitIndexGroup(canceled, batches); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled group accepted", err)
	}
	if s.pendingBytes != 0 {
		t.Fatal("canceled group leaked capacity reservation", s.pendingBytes)
	}
}

// Compare actual indexing transactions, not SQL that bypasses index validation.
// Raw capture occurs before measurement; this is not an end-to-end throughput gate.
func TestIndexGroupWALDiagnostic(t *testing.T) {
	if testing.Short() || os.Getenv("AMC_INDEX_GROUP_DIAGNOSTIC") != "1" {
		t.Skip("opt-in single versus grouped index WAL experiment")
	}
	var sizes []int64
	for _, width := range []int{1, MaxIndexGroupSources} {
		s := openTestStore(t, Options{ExternalCheckpointOwner: true})
		batches := indexGroupFixture(t, s, 64)
		ctx := context.Background()
		if ok, err := s.reclaimWAL(ctx); err != nil || !ok {
			t.Fatal("fixture measurement baseline", ok, err)
		}
		started := time.Now()
		for first := 0; first < len(batches); first += width {
			var err error
			if width == 1 {
				err = s.CommitIndex(ctx, batches[first])
			} else {
				err = s.CommitIndexGroup(ctx, batches[first:first+width])
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		elapsed := time.Since(started)
		info, err := os.Stat(filepath.Join(s.dir, "ledger.sqlite-wal"))
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, info.Size())
		totals, err := s.CatalogTotals(ctx, SessionQuery{})
		if err != nil || totals.Sessions != 64 || totals.RecordedTokens != 128 {
			t.Fatal("grouping changed recorded history", totals, err)
		}
		t.Logf("sources=64 group=%d commits=%d WALBytes=%d elapsed=%s", width, 64/width, info.Size(), elapsed)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if sizes[1] >= sizes[0] {
		t.Fatal("grouping did not reduce fixture WAL writes", sizes)
	}
}
