package accounting

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type StagedForkBaseline struct {
	Inspection ForkBaselineInspection `json:"inspection"`
	ProofID    string                 `json:"proofId,omitempty"`
}

func SelectForkBaseline(ctx context.Context, s *store.Store, id string) error {
	return s.SelectForkBaselineContribution(ctx, id, func(child, parent store.Session, first []store.UsageObservation, parentUsage store.UsageObservation) (json.RawMessage, bool) {
		match, ok := MatchForkBaselineGroup(child, parent, first, parentUsage)
		if !ok {
			return nil, false
		}
		raw, err := json.Marshal(match)
		return raw, err == nil
	})
}

// StageForkBaseline keeps both pieces when the first inherited counter was
// split into prior history and reported last-call attribution. Staging alone
// never removes either piece or any subsequent independent child delta.
func StageForkBaseline(ctx context.Context, s *store.Store, childID, parentID string, maxPages int) (StagedForkBaseline, error) {
	var result StagedForkBaseline
	inspection, err := InspectForkBaseline(ctx, s, childID, parentID, maxPages)
	result.Inspection = inspection
	if err != nil {
		return result, err
	}
	if inspection.Match == nil {
		return result, nil
	}
	child, err := s.GetSession(ctx, childID)
	if err != nil {
		return result, err
	}
	parent, err := s.GetSession(ctx, parentID)
	if err != nil {
		return result, err
	}
	for _, session := range []store.Session{child} {
		owner, err := s.CanonicalDuplicateSession(ctx, session)
		if err != nil {
			return result, err
		}
		if owner != session.ID {
			return result, store.ErrConflict
		}
	}
	ids := append([]string{}, inspection.Match.ChildObservationIDs...)
	if len(ids) == 0 && inspection.Match.ChildObservationID != "" {
		ids = append(ids, inspection.Match.ChildObservationID)
	}
	if len(ids) < 1 || len(ids) > 2 {
		return result, store.ErrInvalid
	}
	sort.Strings(ids)
	var first []store.UsageObservation
	for _, id := range ids {
		u, err := s.UsageObservation(ctx, child.ID, id)
		if err != nil {
			return result, err
		}
		first = append(first, u)
	}
	parentUsage, err := s.UsageObservation(ctx, parent.ID, inspection.Match.ParentObservationID)
	if err != nil {
		return result, err
	}
	match, ok := MatchForkBaselineGroup(child, parent, first, parentUsage)
	if !ok || !reflect.DeepEqual(match, *inspection.Match) {
		return result, store.ErrConflict
	}
	proof := store.UsageReconciliationProof{Kind: "fork-baseline-v1"}
	for i, session := range []store.Session{parent, child} {
		receipt, err := s.SourceOffset(ctx, session.SourceID, session.Generation)
		if err != nil {
			return result, err
		}
		if receipt.SourceID != session.SourceID || receipt.Generation != session.Generation || receipt.RecoveryEpoch == "" {
			return result, store.ErrConflict
		}
		if i == 0 {
			proof.RecoveryEpoch = receipt.RecoveryEpoch
		} else if proof.RecoveryEpoch != receipt.RecoveryEpoch {
			return result, store.ErrConflict
		}
		observations := first
		if i == 0 {
			observations = []store.UsageObservation{parentUsage}
		}
		for j, u := range observations {
			e := store.UsageProofEndpoint{SessionID: session.ID, SourceID: session.SourceID, Generation: session.Generation, Revision: session.ProjectionRevision, IndexedOffset: receipt.IndexedOffset, ObservationID: u.ID, ObservationHash: store.UsageObservationHash(u)}
			if i == 0 {
				proof.Left = e
			} else if j == 0 {
				proof.Right = e
			} else {
				proof.ExtraRight = append(proof.ExtraRight, e)
			}
		}
	}
	proof.Evidence, err = json.Marshal(match)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return result, err
	}
	proof.ID = parser.ID("fork-baseline-proof-v1", string(raw))
	if err = s.StageUsageReconciliationProof(ctx, proof); err != nil {
		return result, err
	}
	result.ProofID = proof.ID
	return result, nil
}
