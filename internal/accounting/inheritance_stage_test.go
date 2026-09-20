package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestForkBaselineStagesBothAttributionPiecesAndPreservesChildDeltas(t *testing.T) {
	ctx := context.Background()
	failCommit := false
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{BeforeCommit: func() error {
		if failCommit {
			return store.ErrCapacity
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"parent", "child"} {
		meta := map[string]string{"id": name}
		if name == "child" {
			meta["forked_from_id"] = "parent"
		}
		header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
		body := append(header, '\n')
		if name == "child" {
			body = append(body, []byte(`{"type":"turn_context","payload":{"model":"model-a"}}`+"\n")...)
		}
		body = append(body, []byte(`{"timestamp":"2026-09-01T01:02:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10},"last_token_usage":{"input_tokens":20,"cached_input_tokens":10,"output_tokens":2}}}}`+"\n")...)
		if name == "child" {
			body = append(body, []byte(`{"timestamp":"2026-09-01T01:02:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"cached_input_tokens":60,"output_tokens":12}}}}`+"\n")...)
		}
		src := protocol.Source{MachineID: "m", SourceID: name, Generation: "g", GenerationSequence: 1, Provider: "codex", NativeID: name, Size: int64(len(body))}
		hash := sha256.Sum256(body)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if _, err = (&indexer.Indexer{Store: s}).Once(ctx, name, "g"); err != nil {
			t.Fatal(err)
		}
		ids[name] = parser.SessionID(src)
	}
	archived := true
	if _, err = s.PatchMetadata(ctx, ids["child"], store.MetadataPatch{OperationID: "archive-child", Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, ids["child"])
	if err != nil {
		t.Fatal(err)
	}
	observations, err := s.GetUsage(ctx, ids["child"], "", 10)
	if err != nil || len(observations) != 3 {
		t.Fatal(observations, err)
	}
	staged, err := StageForkBaseline(ctx, s, ids["child"], ids["parent"], 10)
	if err != nil || staged.ProofID == "" || staged.Inspection.Match == nil || len(staged.Inspection.Match.ChildObservationIDs) != 2 {
		t.Fatal(staged, err)
	}
	proof, current, err := s.UsageReconciliationProofCurrent(ctx, staged.ProofID)
	if err != nil || !current || proof.Kind != "fork-baseline-v1" || len(proof.ExtraRight) != 1 {
		t.Fatal(proof, current, err)
	}
	excludedIDs := map[string]bool{proof.Right.ObservationID: true, proof.ExtraRight[0].ObservationID: true}
	var inherited, independent int64
	for _, u := range observations {
		n := u.TokensIn + u.TokensCache + u.TokensCacheWrite + u.TokensOut
		if excludedIDs[u.ID] {
			inherited += n
			if u.Kind == "cumulative-delta" {
				t.Fatal("later delta included in baseline")
			}
		} else {
			independent += n
		}
	}
	if inherited != 110 || independent != 22 {
		t.Fatal(inherited, independent)
	}
	retry, err := StageForkBaseline(ctx, s, ids["child"], ids["parent"], 10)
	if err != nil || retry.ProofID != staged.ProofID {
		t.Fatal("non-idempotent baseline stage", retry, err)
	}
	if err = SelectDuplicateUsage(ctx, s, staged.ProofID); !errors.Is(err, store.ErrConflict) {
		t.Fatal("duplicate selector accepted a fork group", err)
	}
	bad := proof
	bad.ID = "invalid-group"
	bad.ExtraRight = append([]store.UsageProofEndpoint{}, proof.ExtraRight...)
	bad.ExtraRight[0].SourceID = "wrong-source"
	if err = s.StageUsageReconciliationProof(ctx, bad); !errors.Is(err, store.ErrInvalid) {
		t.Fatal("mixed-source group accepted", err)
	}
	after, err := s.GetSession(ctx, ids["child"])
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("staging changed child history/organization", err)
	}
	afterUsage, err := s.GetUsage(ctx, ids["child"], "", 10)
	if err != nil || !reflect.DeepEqual(observations, afterUsage) {
		t.Fatal("staging mutated raw attribution", err)
	}
	totals, err := s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil || totals.Recorded.Total != 242 || totals.Counted.Total != 242 || totals.ActiveExclusions != 0 {
		t.Fatal("staged baseline applied early", totals, err)
	}
	pageBefore, err := s.UsageContributions(ctx, ids["child"], "", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	failCommit = true
	if err = SelectForkBaseline(ctx, s, staged.ProofID); !errors.Is(err, store.ErrCapacity) {
		t.Fatal("expected failed atomic selection", err)
	}
	failCommit = false
	pageFailed, err := s.UsageContributions(ctx, ids["child"], "", 10, "")
	if err != nil || !reflect.DeepEqual(pageBefore, pageFailed) {
		t.Fatal("failed selection changed contributions or change head", pageFailed, err)
	}
	if err = SelectForkBaseline(ctx, s, staged.ProofID); err != nil {
		t.Fatal(err)
	}
	totals, err = s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil || totals.Recorded.Total != 242 || totals.Excluded.Total != 110 || totals.Counted.Total != 132 || totals.ActiveExclusions != 2 {
		t.Fatal("fork baseline not selected as a complete group", totals, err)
	}
	page, err := s.UsageContributions(ctx, ids["child"], "", 10, "")
	if err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	for _, row := range page.Items {
		if excludedIDs[row.Recorded.ID] {
			if row.Counted || row.ExcludedByProof != staged.ProofID || row.ExclusionKind != "fork-baseline-v1" || row.OwnerSessionID != ids["parent"] {
				t.Fatal("missing grouped exclusion", row)
			}
		} else if !row.Counted {
			t.Fatal("independent child usage excluded", row)
		}
	}
	if err = SelectForkBaseline(ctx, s, staged.ProofID); err != nil {
		t.Fatal(err)
	}
	pageRetry, err := s.UsageContributions(ctx, ids["child"], "", 10, "")
	if err != nil || !reflect.DeepEqual(page, pageRetry) {
		t.Fatal("retry changed selection or change head", err)
	}
	after, err = s.GetSession(ctx, ids["child"])
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("selection changed history or organization", err)
	}
	afterUsage, err = s.GetUsage(ctx, ids["child"], "", 10)
	if err != nil || !reflect.DeepEqual(observations, afterUsage) {
		t.Fatal("selection changed recorded usage", err)
	}
	// Fault injection is confined to this test's disposable ledger. A change to
	// either piece invalidates the entire proof, not just that observation.
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, endpoint := range []store.UsageProofEndpoint{proof.Right, proof.ExtraRight[0]} {
		var original []byte
		if err = db.QueryRow(`SELECT observation FROM usage_observations WHERE id=?`, endpoint.ObservationID).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`UPDATE usage_observations SET observation=json_set(observation,'$.evidence.changed',true) WHERE id=?`, endpoint.ObservationID); err != nil {
			t.Fatal(err)
		}
		for id := range excludedIDs {
			if _, active, err := s.CurrentUsageContributionExclusion(ctx, id); err != nil || active {
				t.Fatal("part of stale group remained active", id, active, err)
			}
		}
		totals, err = s.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || totals.Excluded.Total != 0 || totals.Counted.Total != 242 || totals.ActiveExclusions != 0 {
			t.Fatal("stale group still subtracted from totals", totals, err)
		}
		if _, err = db.Exec(`UPDATE usage_observations SET observation=? WHERE id=?`, original, endpoint.ObservationID); err != nil {
			t.Fatal(err)
		}
		totals, err = s.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || totals.Excluded.Total != 110 || totals.ActiveExclusions != 2 {
			t.Fatal("restored exact evidence did not revalidate group", totals, err)
		}
	}
	t.Run("shorter canonical parent does not hide captured evidence", func(t *testing.T) {
		raw := []byte(`{"type":"session_meta","payload":{"id":"parent"}}` + "\n" + `{"timestamp":"2026-09-01T01:02:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":1}}}}` + "\n")
		src := protocol.Source{MachineID: "m", SourceID: "short", Generation: "g", GenerationSequence: 1, Provider: "codex", NativeID: "parent", Size: int64(len(raw))}
		for n := 0; parser.SessionID(src) >= ids["parent"] && n < 100; n++ {
			src.SourceID += "x"
		}
		if parser.SessionID(src) >= ids["parent"] {
			t.Fatal("fixture failed to select earlier copy ID")
		}
		hash := sha256.Sum256(raw)
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := (&indexer.Indexer{Store: s}).Once(ctx, src.SourceID, src.Generation); err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectCapturedForkBaseline(ctx, s, ids["child"], 20)
		if err != nil || inspection.Match == nil || inspection.Match.ParentSessionID != ids["parent"] {
			t.Fatal("longer evidence copy was missed", inspection, err)
		}
		if _, err := InspectCapturedForkBaseline(ctx, s, ids["child"], 1); !errors.Is(err, ErrInheritanceScanIncomplete) {
			t.Fatal("shared scan budget not enforced", err)
		}
		fresh := proof
		fresh.ID += "-longer-parent"
		if err := s.StageUsageReconciliationProof(ctx, fresh); err != nil {
			t.Fatal("fresh longer-copy proof", err)
		}
		if err := SelectForkBaseline(ctx, s, fresh.ID); err != nil {
			t.Fatal("fresh selection incorrectly requires shortest parent copy", err)
		}
		for id := range excludedIDs {
			if exclusion, active, err := s.CurrentUsageContributionExclusion(ctx, id); err != nil || !active || exclusion.ProofID != fresh.ID {
				t.Fatal("valid evidence lost to shorter copy", active, err)
			}
		}
		totals, err := s.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || totals.Recorded.Total != 253 || totals.Excluded.Total != 110 || totals.Counted.Total != 143 {
			t.Fatal(totals, err)
		}
		// Conflicting fork identity cannot be hidden by selecting another copy.
		if _, err := db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.forkedFromId','child') WHERE id=?`, parser.SessionID(src)); err != nil {
			t.Fatal(err)
		}
		for id := range excludedIDs {
			if _, active, err := s.CurrentUsageContributionExclusion(ctx, id); err != nil || active {
				t.Fatal("conflicting parent ancestry accepted", active, err)
			}
		}
		conflicting := proof
		conflicting.ID += "-conflicting-parent"
		if err := s.StageUsageReconciliationProof(ctx, conflicting); err != nil {
			t.Fatal(err)
		}
		if err := SelectForkBaseline(ctx, s, conflicting.ID); !errors.Is(err, store.ErrConflict) {
			t.Fatal("new correction accepted conflicting copy ancestry", err)
		}
	})
}
