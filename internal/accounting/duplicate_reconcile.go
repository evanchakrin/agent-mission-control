package accounting

import (
	"context"
	"encoding/json"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type DuplicateReconciliationPage struct {
	OwnerSessionID string `json:"ownerSessionId"`
	CopySessionID  string `json:"copySessionId"`
	Scanned        int    `json:"scanned"`
	Matched        int    `json:"matched"`
	Unverified     int    `json:"unverified"`
	Ambiguous      int    `json:"ambiguous"`
	NextCursor     string `json:"nextCursor,omitempty"`
	Snapshot       string `json:"snapshot"`
}

// ReconcileDuplicateUsagePage advances at most 100 existing observations.
// Each exclusion has an independently durable idempotent receipt. Interrupted
// callers may repeat a page; source changes require starting a new snapshot.
// No transcript copy, unbounded in-memory catalog, or historical retention cap.
func ReconcileDuplicateUsagePage(ctx context.Context, s *store.Store, leftID, rightID, after, snapshot string, limit int) (DuplicateReconciliationPage, error) {
	var result DuplicateReconciliationPage
	if leftID == "" || rightID == "" || leftID == rightID || len(leftID) > 512 || len(rightID) > 512 || len(after) > 512 || len(snapshot) > 64 || limit < 1 || limit > 100 || (after != "" && snapshot == "") {
		return result, store.ErrInvalid
	}
	left, right, token, err := duplicatePairSnapshot(ctx, s, leftID, rightID)
	if err != nil {
		return result, err
	}
	if snapshot != "" && snapshot != token {
		return result, store.ErrHistoryChanged
	}
	result.OwnerSessionID, result.CopySessionID, result.Snapshot = left.ID, right.ID, token
	rows, err := s.GetUsage(ctx, right.ID, after, limit+1)
	if err != nil {
		return result, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		result.NextCursor = rows[len(rows)-1].ID
	}
	for _, u := range rows {
		result.Scanned++
		// Existing verified prefixes survive ordinary append-only collection.
		// Do not manufacture a fresh proof for every old observation on each run.
		if exclusion, active, err := s.CurrentUsageContributionExclusion(ctx, u.ID); err != nil {
			return result, err
		} else if active && exclusion.OwnerSessionID == left.ID {
			result.Matched++
			continue
		}
		if _, _, ok := duplicateUsageEvidence(right, u); !ok {
			result.Unverified++
			continue
		}
		candidates, err := s.DuplicateUsageCandidates(ctx, left, u)
		if err != nil {
			return result, err
		}
		if len(candidates) >= 3 {
			result.Ambiguous++
			continue
		}
		var matched []store.UsageObservation
		for _, v := range candidates {
			if _, ok := MatchDuplicateCodexUsage(left, right, v, u); ok {
				matched = append(matched, v)
			}
		}
		if len(matched) == 0 {
			result.Unverified++
			continue
		}
		if len(matched) != 1 {
			result.Ambiguous++
			continue
		}
		// Do not quietly use a fresh larger prefix to publish a page whose caller
		// is still traversing the old snapshot.
		_, _, current, err := duplicatePairSnapshot(ctx, s, left.ID, right.ID)
		if err != nil {
			return result, err
		}
		if current != token {
			return result, store.ErrHistoryChanged
		}
		proof, err := StageDuplicateUsage(ctx, s, left.ID, right.ID, matched[0], u)
		if err != nil {
			return result, err
		}
		if err = SelectDuplicateUsage(ctx, s, proof); err != nil {
			return result, err
		}
		result.Matched++
	}
	_, _, current, err := duplicatePairSnapshot(ctx, s, left.ID, right.ID)
	if err != nil {
		return result, err
	}
	if current != token {
		return result, store.ErrHistoryChanged
	}
	return result, nil
}

func duplicatePairSnapshot(ctx context.Context, s *store.Store, leftID, rightID string) (store.Session, store.Session, string, error) {
	left, err := s.GetSession(ctx, leftID)
	if err != nil {
		return left, store.Session{}, "", err
	}
	right, err := s.GetSession(ctx, rightID)
	if err != nil {
		return left, right, "", err
	}
	if left.ID > right.ID {
		left, right = right, left
	}
	if left.MachineID == "" || left.MachineID != right.MachineID || left.Provider != "codex" || right.Provider != "codex" || left.NativeID == "" || left.NativeID != right.NativeID || left.SourceID == right.SourceID {
		return left, right, "", store.ErrInvalid
	}
	owner, err := s.CanonicalDuplicateSession(ctx, left)
	if err != nil {
		return left, right, "", err
	}
	if owner != left.ID {
		return left, right, "", store.ErrConflict
	}
	a, err := s.SourceOffset(ctx, left.SourceID, left.Generation)
	if err != nil {
		return left, right, "", err
	}
	b, err := s.SourceOffset(ctx, right.SourceID, right.Generation)
	if err != nil {
		return left, right, "", err
	}
	if a.RecoveryEpoch == "" || a.RecoveryEpoch != b.RecoveryEpoch || a.SourceID != left.SourceID || b.SourceID != right.SourceID || a.Generation != left.Generation || b.Generation != right.Generation {
		return left, right, "", store.ErrConflict
	}
	raw, _ := json.Marshal([]any{left.ID, left.SourceID, left.Generation, left.ProjectionRevision, a.IndexedOffset, right.ID, right.SourceID, right.Generation, right.ProjectionRevision, b.IndexedOffset, a.RecoveryEpoch})
	return left, right, parser.ID("duplicate-pair-v1", string(raw)), nil
}
