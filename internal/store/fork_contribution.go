package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
)

type ForkBaselineVerifier func(child, parent Session, first []UsageObservation, parentUsage UsageObservation) (json.RawMessage, bool)

func (s *Store) SelectForkBaselineContribution(ctx context.Context, id string, verify ForkBaselineVerifier) error {
	if !validID(id) || verify == nil {
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
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM usage_contribution_selections WHERE proof_id=?`, id).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	p, sessions, usage, err := loadCurrentForkProof(ctx, tx, id, s.epoch)
	if err != nil {
		return err
	}
	evidence, ok := verify(sessions[1], sessions[0], usage[1:], usage[0])
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

func proofSession(ctx context.Context, tx *sql.Tx, id string) (Session, error) {
	var session Session
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT `+sessionProjectionJSON+` FROM sessions s WHERE s.id=?`, id).Scan(&raw)
	if err != nil {
		return session, err
	}
	err = json.Unmarshal(raw, &session)
	return session, err
}

func canonicalProofSession(ctx context.Context, tx *sql.Tx, machine, native string) (Session, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(id),'') FROM query_sessions WHERE machine_id=? AND provider='codex' AND native_id=?`, machine, native).Scan(&id)
	if err != nil {
		return Session{}, err
	}
	if id == "" {
		return Session{}, ErrConflict
	}
	return proofSession(ctx, tx, id)
}

// Bounded ancestry verification rejects cycles and missing ancestry rather than
// making a circular chain of exclusions look like zero usage. This is a proof
// budget, not a session-retention or relationship-depth storage limit.
func verifyForkAncestry(ctx context.Context, tx *sql.Tx, child, parent Session) error {
	seen := map[string]bool{child.NativeID: true}
	current := parent
	for step := 0; step < 1000; step++ {
		if seen[current.NativeID] {
			return ErrConflict
		}
		seen[current.NativeID] = true
		if current.ForkedFromID == "" {
			return nil
		}
		var err error
		current, err = canonicalProofSession(ctx, tx, child.MachineID, current.ForkedFromID)
		if err != nil {
			return err
		}
	}
	return ErrConflict
}

func loadCurrentForkProof(ctx context.Context, tx *sql.Tx, id, epoch string) (UsageReconciliationProof, []Session, []UsageObservation, error) {
	var p UsageReconciliationProof
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT proof FROM usage_reconciliation_proofs WHERE id=?`, id).Scan(&raw); err != nil {
		return p, nil, nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, nil, nil, err
	}
	if p.Kind != "fork-baseline-v1" || p.RecoveryEpoch != epoch || len(p.ExtraRight) > 1 {
		return p, nil, nil, ErrConflict
	}
	var sessions []Session
	var usage []UsageObservation
	seen := map[string]bool{}
	for i, e := range proofEndpoints(p) {
		if seen[e.ObservationID] {
			return p, nil, nil, ErrConflict
		}
		seen[e.ObservationID] = true
		if i > 1 && (e.SessionID != p.Right.SessionID || e.SourceID != p.Right.SourceID || e.Generation != p.Right.Generation || e.Revision != p.Right.Revision || e.IndexedOffset != p.Right.IndexedOffset) {
			return p, nil, nil, ErrConflict
		}
		if err := verifyUsageProofEndpoint(ctx, tx, e, true); err != nil {
			return p, nil, nil, err
		}
		session, err := proofSession(ctx, tx, e.SessionID)
		if err != nil {
			return p, nil, nil, err
		}
		sessions = append(sessions, session)
		if err = tx.QueryRowContext(ctx, `SELECT observation FROM usage_observations WHERE id=?`, e.ObservationID).Scan(&raw); err != nil {
			return p, nil, nil, err
		}
		var u UsageObservation
		if err = json.Unmarshal(raw, &u); err != nil {
			return p, nil, nil, err
		}
		usage = append(usage, u)
	}
	parent, child := sessions[0], sessions[1]
	if child.MachineID == "" || child.MachineID != parent.MachineID || child.Provider != "codex" || parent.Provider != "codex" || child.NativeID == "" || child.NativeID == parent.NativeID || child.SourceID == parent.SourceID || parent.NativeID == "" || child.ForkedFromID != parent.NativeID {
		return p, nil, nil, ErrConflict
	}
	for _, session := range []Session{parent, child} {
		owner, err := canonicalProofSession(ctx, tx, session.MachineID, session.NativeID)
		if err != nil {
			return p, nil, nil, err
		}
		if (session.ID == child.ID && owner.ID != session.ID) || owner.ForkedFromID != session.ForkedFromID {
			return p, nil, nil, ErrConflict
		}
	}
	if err := verifyForkAncestry(ctx, tx, child, parent); err != nil {
		return p, nil, nil, err
	}
	return p, sessions, usage, nil
}

func loadCurrentContributionProof(ctx context.Context, tx *sql.Tx, id, epoch string) (UsageReconciliationProof, []Session, []UsageObservation, error) {
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT json_extract(proof,'$.kind') FROM usage_reconciliation_proofs WHERE id=?`, id).Scan(&kind); err != nil {
		return UsageReconciliationProof{}, nil, nil, err
	}
	if kind == "fork-baseline-v1" {
		return loadCurrentForkProof(ctx, tx, id, epoch)
	}
	return loadCurrentDuplicateProof(ctx, tx, id, epoch)
}

func excludedProofObservation(usage []UsageObservation, id string) (UsageObservation, bool) {
	for _, u := range usage[1:] {
		if u.ID == id {
			return u, true
		}
	}
	return UsageObservation{}, false
}
