package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestUsageReconciliationProofIsDurableIdempotentAndNeverChangesTotals(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	p := UsageReconciliationProof{ID: "proof", Kind: "duplicate-codex-usage-v1", RecoveryEpoch: s.RecoveryEpoch(), Evidence: json.RawMessage(`{"test":"storage-contract"}`)}
	var before []Session
	for i := 0; i < 2; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("source-%06d", i)
		src.Generation = "g1"
		ingest(t, s, src, 0, "tiny\n")
		if _, err := s.db.Exec(`UPDATE sources SET indexed_offset=5 WHERE source_id=?`, src.SourceID); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("s-%06d", i)
		rows, err := s.GetUsage(ctx, id, "", 1)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		u := rows[0]
		u.Evidence = json.RawMessage(`{"offset":0}`)
		raw, _ := json.Marshal(u)
		if _, err = s.db.Exec(`UPDATE usage_observations SET observation=? WHERE id=?`, raw, u.ID); err != nil {
			t.Fatal(err)
		}
		e := UsageProofEndpoint{SessionID: id, SourceID: src.SourceID, Generation: "g1", IndexedOffset: 5, ObservationID: u.ID, ObservationHash: UsageObservationHash(u)}
		if i == 0 {
			p.Left = e
		} else {
			p.Right = e
		}
		session, err := s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, session)
	}
	if err := s.StageUsageReconciliationProof(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.StageUsageReconciliationProof(ctx, p); err != nil {
		t.Fatal("retry", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_reconciliation_proofs`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	conflict := p
	conflict.Evidence = json.RawMessage(`{"different":true}`)
	if err := s.StageUsageReconciliationProof(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, current, err := s.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || !current {
		t.Fatal(current, err)
	}
	for _, session := range before {
		after, err := s.GetSession(ctx, session.ID)
		if err != nil || !reflect.DeepEqual(session, after) {
			t.Fatal("session changed", err)
		}
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, current, err := reopened.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || !current {
		t.Fatal("restart", current, err)
	}
	for _, name := range []string{"hash", "revision", "offset", "epoch", "commit-failure"} {
		t.Run(name, func(t *testing.T) {
			bad := p
			bad.ID = name
			want := ErrConflict
			switch name {
			case "hash":
				bad.Left.ObservationHash = fmt.Sprintf("%064d", 0)
			case "revision":
				bad.Right.Revision = "different"
			case "offset":
				bad.Right.IndexedOffset++
			case "epoch":
				bad.RecoveryEpoch = "other"
				want = ErrInvalid
			case "commit-failure":
				reopened.options.BeforeCommit = func() error { return ErrCapacity }
				want = ErrCapacity
				defer func() { reopened.options.BeforeCommit = nil }()
			}
			if err := reopened.StageUsageReconciliationProof(ctx, bad); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if _, _, err := reopened.UsageReconciliationProofCurrent(ctx, bad.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("failed proof persisted", err)
			}
		})
	}
	// Growth preserves the proof, but mutating its exact observation does not.
	if _, err := reopened.db.Exec(`UPDATE sources SET indexed_offset=6 WHERE source_id=?`, p.Right.SourceID); err != nil {
		t.Fatal(err)
	}
	if _, current, err := reopened.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || !current {
		t.Fatal("prefix growth invalidated proof", current, err)
	}
	var original []byte
	if err := reopened.db.QueryRow(`SELECT observation FROM usage_observations WHERE id=?`, p.Right.ObservationID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.db.Exec(`UPDATE usage_observations SET observation=json_set(observation,'$.tokensIn',999) WHERE id=?`, p.Right.ObservationID); err != nil {
		t.Fatal(err)
	}
	if _, current, err := reopened.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || current {
		t.Fatal("changed observation accepted after growth", current, err)
	}
	if _, err := reopened.db.Exec(`UPDATE usage_observations SET observation=? WHERE id=?`, original, p.Right.ObservationID); err != nil {
		t.Fatal(err)
	}
	var projection []byte
	if err := reopened.db.QueryRow(`SELECT projection FROM sessions WHERE id=?`, p.Right.SessionID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.projectionRevision','new-revision') WHERE id=?`, p.Right.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, current, err := reopened.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || current {
		t.Fatal("changed projection accepted after growth", current, err)
	}
	if _, err := reopened.db.Exec(`UPDATE sessions SET projection=? WHERE id=?`, projection, p.Right.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.db.Exec(`UPDATE sources SET indexed_offset=4 WHERE source_id=?`, p.Right.SourceID); err != nil {
		t.Fatal(err)
	}
	if _, current, err := reopened.UsageReconciliationProofCurrent(ctx, p.ID); err != nil || current {
		t.Fatal("stale proof current", current, err)
	}
	if err := reopened.StageUsageReconciliationProof(ctx, p); err != nil {
		t.Fatal("durable receipt retry", err)
	}
}
