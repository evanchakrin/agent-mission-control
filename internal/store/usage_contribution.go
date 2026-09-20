package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
)

// DuplicateUsageVerifier runs against transactionally loaded observations. It
// must be bounded, pure, and return the same evidence as the staged matcher.
type DuplicateUsageVerifier func(Session, Session, UsageObservation, UsageObservation) (json.RawMessage, bool)

// SelectDuplicateUsageContribution selects the left observation as owner and
// the right as excluded. The raw ledger is unchanged. All query consumers must
// use current selections, never subtract staged proofs or historical receipts.
func (s *Store) SelectDuplicateUsageContribution(ctx context.Context, proofID string, verify DuplicateUsageVerifier) error {
	if !validID(proofID) || verify == nil {
		return ErrInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureUsageContributionSchema(ctx, tx); err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM usage_contribution_selections WHERE proof_id=?`, proofID).Scan(&exists)
	if err == nil {
		return nil
	} // Historical receipt; never re-select over newer work.
	if err != sql.ErrNoRows {
		return err
	}
	p, sessions, usage, err := loadCurrentDuplicateProof(ctx, tx, proofID, s.epoch)
	if err != nil {
		return err
	}
	evidence, ok := verify(sessions[0], sessions[1], usage[0], usage[1])
	if !ok || !bytes.Equal(evidence, p.Evidence) {
		return ErrConflict
	}
	s.capacityMu.Lock()
	err = s.capacity(65536)
	s.capacityMu.Unlock()
	if err != nil {
		return err
	}
	if err = writeUsageContributionSelection(ctx, tx, p); err != nil {
		return err
	}
	if s.options.BeforeCommit != nil {
		if err = s.options.BeforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadCurrentDuplicateProof(ctx context.Context, tx *sql.Tx, id, epoch string) (UsageReconciliationProof, []Session, []UsageObservation, error) {
	var p UsageReconciliationProof
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT proof FROM usage_reconciliation_proofs WHERE id=?`, id).Scan(&raw); err != nil {
		return p, nil, nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, nil, nil, err
	}
	if p.Kind != "duplicate-codex-usage-v1" || p.RecoveryEpoch != epoch || p.Left.SessionID >= p.Right.SessionID {
		return p, nil, nil, ErrConflict
	}
	var sessions []Session
	var usage []UsageObservation
	for _, e := range []UsageProofEndpoint{p.Left, p.Right} {
		if err := verifyUsageProofEndpoint(ctx, tx, e, true); err != nil {
			return p, nil, nil, err
		}
		var session Session
		if err := tx.QueryRowContext(ctx, `SELECT projection FROM sessions WHERE id=?`, e.SessionID).Scan(&raw); err != nil {
			return p, nil, nil, err
		}
		if err := json.Unmarshal(raw, &session); err != nil {
			return p, nil, nil, err
		}
		var u UsageObservation
		if err := tx.QueryRowContext(ctx, `SELECT observation FROM usage_observations WHERE id=?`, e.ObservationID).Scan(&raw); err != nil {
			return p, nil, nil, err
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			return p, nil, nil, err
		}
		sessions = append(sessions, session)
		usage = append(usage, u)
	}
	left, right := sessions[0], sessions[1]
	if left.MachineID == "" || left.MachineID != right.MachineID || left.Provider != "codex" || right.Provider != "codex" || left.NativeID == "" || left.NativeID != right.NativeID {
		return p, nil, nil, ErrConflict
	}
	// A single catalog owner forbids exclusion chains and cycles. If an earlier
	// source is subsequently discovered, old selections become stale, not links
	// through an observation that may itself be excluded. Missing owner evidence
	// stays unreconciled rather than inventing an exclusion.
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT MIN(id) FROM query_sessions WHERE machine_id=? AND provider=? AND native_id=?`, left.MachineID, left.Provider, left.NativeID).Scan(&owner); err != nil {
		return p, nil, nil, err
	}
	if owner != left.ID {
		return p, nil, nil, ErrConflict
	}
	return p, sessions, usage, nil
}

type UsageContributionExclusion struct {
	Kind           string           `json:"kind"`
	ProofID        string           `json:"proofId"`
	OwnerSessionID string           `json:"ownerSessionId"`
	Observation    UsageObservation `json:"observation"`
}

// CurrentUsageContributionExclusion returns no correction for stale evidence.
// Missing selection is normal. The original usage remains the recorded truth.
func (s *Store) CurrentUsageContributionExclusion(ctx context.Context, observationID string) (UsageContributionExclusion, bool, error) {
	var result UsageContributionExclusion
	if !validID(observationID) {
		return result, false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, false, err
	}
	defer tx.Rollback()
	return currentUsageContributionExclusion(ctx, tx, observationID, s.epoch)
}

func currentUsageContributionExclusion(ctx context.Context, tx *sql.Tx, observationID, epoch string) (UsageContributionExclusion, bool, error) {
	var result UsageContributionExclusion
	var ready int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='usage_contribution_owners'`).Scan(&ready); err != nil {
		return result, false, err
	}
	if ready == 0 {
		return result, false, nil
	}
	var proofID string
	if err := tx.QueryRowContext(ctx, `SELECT proof_id FROM usage_contribution_owners WHERE excluded_id=?`, observationID).Scan(&proofID); err != nil {
		if err == sql.ErrNoRows {
			return result, false, nil
		}
		return result, false, err
	}
	p, _, usage, err := loadCurrentContributionProof(ctx, tx, proofID, epoch)
	if err != nil {
		if err == ErrConflict {
			return result, false, nil
		}
		return result, false, err
	}
	u, ok := excludedProofObservation(usage, observationID)
	if !ok {
		return result, false, ErrConflict
	}
	return UsageContributionExclusion{Kind: p.Kind, ProofID: proofID, OwnerSessionID: p.Left.SessionID, Observation: u}, true, nil
}
