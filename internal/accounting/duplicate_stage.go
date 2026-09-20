package accounting

import (
	"context"
	"encoding/json"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// StageDuplicateUsage persists a verified pair, not an active correction. The
// caller can page observations without holding a database transaction; the
// store checks their hashes and published source prefixes again at commit.
func StageDuplicateUsage(ctx context.Context, s *store.Store, leftID, rightID string, a, b store.UsageObservation) (string, error) {
	left, err := s.GetSession(ctx, leftID)
	if err != nil {
		return "", err
	}
	right, err := s.GetSession(ctx, rightID)
	if err != nil {
		return "", err
	}
	// Deterministic orientation makes reverse traversal the same staged pair.
	if left.ID > right.ID {
		left, right = right, left
		a, b = b, a
	}
	match, ok := MatchDuplicateCodexUsage(left, right, a, b)
	if !ok {
		return "", store.ErrInvalid
	}
	proof := store.UsageReconciliationProof{Kind: "duplicate-codex-usage-v1"}
	for i, session := range []store.Session{left, right} {
		receipt, err := s.SourceOffset(ctx, session.SourceID, session.Generation)
		if err != nil {
			return "", err
		}
		if receipt.SourceID != session.SourceID || receipt.Generation != session.Generation || receipt.RecoveryEpoch == "" {
			return "", store.ErrConflict
		}
		if i == 0 {
			proof.RecoveryEpoch = receipt.RecoveryEpoch
		} else if receipt.RecoveryEpoch != proof.RecoveryEpoch {
			return "", store.ErrConflict
		}
		u := []store.UsageObservation{a, b}[i]
		endpoint := store.UsageProofEndpoint{SessionID: session.ID, SourceID: session.SourceID, Generation: session.Generation, Revision: session.ProjectionRevision, IndexedOffset: receipt.IndexedOffset, ObservationID: u.ID, ObservationHash: store.UsageObservationHash(u)}
		if i == 0 {
			proof.Left = endpoint
		} else {
			proof.Right = endpoint
		}
	}
	proof.Evidence, err = json.Marshal(match)
	if err != nil {
		return "", err
	}
	request, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	proof.ID = parser.ID("duplicate-usage-proof-v1", string(request))
	if err = s.StageUsageReconciliationProof(ctx, proof); err != nil {
		return "", err
	}
	return proof.ID, nil
}

// SelectDuplicateUsage activates only the contribution decision. Recorded usage
// and historical prices remain immutable; reconciled query totals are separate.
func SelectDuplicateUsage(ctx context.Context, s *store.Store, proofID string) error {
	return s.SelectDuplicateUsageContribution(ctx, proofID, func(left, right store.Session, a, b store.UsageObservation) (json.RawMessage, bool) {
		match, ok := MatchDuplicateCodexUsage(left, right, a, b)
		if !ok {
			return nil, false
		}
		raw, err := json.Marshal(match)
		return raw, err == nil
	})
}
