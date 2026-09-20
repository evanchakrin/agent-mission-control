package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestDuplicateReconcilerPagesRealHistoryWithoutDoubleSubtraction(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sourceID := range []string{"original", "copy"} {
		var body []byte
		if sourceID == "copy" {
			body = []byte("\n")
		}
		for i := 0; i < 3; i++ {
			body = append(body, []byte(fmt.Sprintf(`{"timestamp":"2026-09-01T01:02:0%dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`+"\n", i, 100+i*20, 50+i*10, 10+i*2))...)
		}
		src := protocol.Source{MachineID: "m", SourceID: sourceID, Generation: "g", GenerationSequence: 1, Provider: "codex", NativeID: "thread", Size: int64(len(body))}
		hash := sha256.Sum256(body)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = (&indexer.Indexer{Store: s}).Once(ctx, sourceID, "g"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, parser.SessionID(src))
	}
	page, err := ReconcileDuplicateUsagePage(ctx, s, ids[0], ids[1], "", "", 1)
	if err != nil || page.Scanned != 1 || page.Matched != 1 || page.NextCursor == "" || page.Snapshot == "" {
		t.Fatal(page, err)
	}
	head, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := ReconcileDuplicateUsagePage(ctx, s, ids[1], ids[0], "", page.Snapshot, 1)
	if err != nil || retry.NextCursor != page.NextCursor {
		t.Fatal("page retry", retry, err)
	}
	if now, err := s.ChangeHead(ctx); err != nil || now != head {
		t.Fatal("page retry subtracted twice", now, err)
	}
	if _, err := ReconcileDuplicateUsagePage(ctx, s, ids[0], ids[1], page.NextCursor, "", 1); !errors.Is(err, store.ErrInvalid) {
		t.Fatal("unbound cursor", err)
	}
	matched := page.Matched
	for step := 0; page.NextCursor != ""; step++ {
		if step >= 3 {
			t.Fatal("cursor did not terminate")
		}
		page, err = ReconcileDuplicateUsagePage(ctx, s, ids[0], ids[1], page.NextCursor, page.Snapshot, 1)
		if err != nil || page.Scanned != 1 || page.Matched != 1 || page.Unverified != 0 || page.Ambiguous != 0 {
			t.Fatal(page, err)
		}
		matched += page.Matched
	}
	if matched != 3 {
		t.Fatal(matched)
	}
	totals, err := s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil || totals.Recorded.Total != 308 || totals.Excluded.Total != 154 || totals.Counted.Total != 154 || totals.ActiveExclusions != 3 {
		t.Fatal(totals, err)
	}
	if _, err := ReconcileDuplicateUsagePage(ctx, s, ids[0], ids[1], "", "wrong", 1); !errors.Is(err, store.ErrHistoryChanged) {
		t.Fatal("stale cursor accepted", err)
	}
	// Matching candidates uses the existing index and exact bucket predicates;
	// unsupported evidence stays recorded without an exclusion.
	copySession, err := s.GetSession(ctx, page.CopySessionID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUsage(ctx, copySession.ID, "", 1)
	if err != nil || len(u) != 1 {
		t.Fatal(u, err)
	}
	ownerSession, err := s.GetSession(ctx, page.OwnerSessionID)
	if err != nil {
		t.Fatal(err)
	}
	u[0].TokensOut++
	if candidates, err := s.DuplicateUsageCandidates(ctx, ownerSession, u[0]); err != nil || len(candidates) != 0 {
		t.Fatal("unmatched buckets accepted", candidates, err)
	}
	t.Run("backup-restore-revalidation", func(t *testing.T) {
		archived := true
		name := "Archived accounting fixture"
		metadata, err := s.PatchMetadata(ctx, copySession.ID, store.MetadataPatch{OperationID: "archive-before-backup", Archived: &archived, Name: &name})
		if err != nil {
			t.Fatal(err)
		}
		observations, err := s.GetUsage(ctx, copySession.ID, "", 10)
		if err != nil || len(observations) != 3 {
			t.Fatal(observations, err)
		}
		exclusion, active, err := s.CurrentUsageContributionExclusion(ctx, observations[0].ID)
		if err != nil || !active {
			t.Fatal(exclusion, active, err)
		}
		proof, active, err := s.UsageReconciliationProofCurrent(ctx, exclusion.ProofID)
		if err != nil || !active {
			t.Fatal(proof, active, err)
		}
		root := t.TempDir()
		backupDir := filepath.Join(root, "backup")
		if _, err = s.Backup(ctx, backupDir); err != nil {
			t.Fatal(err)
		}
		t.Run("process-exit-commit-boundaries", func(t *testing.T) {
			checkDuplicateReconciliationCrashes(t, backupDir, filepath.Join(root, "crash-restored"), ids[0], ids[1])
		})
		restored, err := store.RestoreBackup(ctx, backupDir, filepath.Join(root, "restored"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		if err = restored.SetupAnalytics(ctx); err != nil {
			t.Fatal(err)
		}
		if restored.RecoveryEpoch() == s.RecoveryEpoch() {
			t.Fatal("restore reused recovery epoch")
		}
		retainedProof, current, err := restored.UsageReconciliationProofCurrent(ctx, proof.ID)
		if err != nil || current || !reflect.DeepEqual(retainedProof, proof) {
			t.Fatal("proof lost or applied before revalidation", retainedProof, current, err)
		}
		beforeRevalidation, err := restored.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || beforeRevalidation.Recorded.Total != 308 || beforeRevalidation.Counted.Total != 308 || beforeRevalidation.ActiveExclusions != 0 || beforeRevalidation.StaleSelections != 3 {
			t.Fatal("old exclusions active after restore", beforeRevalidation, err)
		}
		if _, err = ReconcileDuplicateUsagePage(ctx, restored, ids[0], ids[1], "", page.Snapshot, 100); !errors.Is(err, store.ErrHistoryChanged) {
			t.Fatal("pre-restore cursor accepted", err)
		}
		revalidated, err := ReconcileDuplicateUsagePage(ctx, restored, ids[0], ids[1], "", "", 100)
		if err != nil || revalidated.Matched != 3 || revalidated.NextCursor != "" {
			t.Fatal("restored evidence did not reconcile", revalidated, err)
		}
		afterRevalidation, err := restored.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || !reflect.DeepEqual(afterRevalidation, totals) {
			t.Fatal("reconciliation changed restored usage", afterRevalidation, totals, err)
		}
		after, err := restored.GetSession(ctx, copySession.ID)
		if err != nil || !reflect.DeepEqual(after.Metadata, metadata) {
			t.Fatal("archive or name lost", after.Metadata, metadata, err)
		}
		afterUsage, err := restored.GetUsage(ctx, copySession.ID, "", 10)
		if err != nil || !reflect.DeepEqual(observations, afterUsage) {
			t.Fatal("raw observations changed", err)
		}
		if retained, current, err := restored.UsageReconciliationProofCurrent(ctx, proof.ID); err != nil || current || !reflect.DeepEqual(retained, proof) {
			t.Fatal("new decision overwrote old proof", retained, current, err)
		}
	})
}
