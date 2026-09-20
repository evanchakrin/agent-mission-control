package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"
)

// UsageProofEndpoint pins an observation to one published source prefix.
type UsageProofEndpoint struct {
	SessionID       string `json:"sessionId"`
	SourceID        string `json:"sourceId"`
	Generation      string `json:"generation"`
	Revision        string `json:"revision"`
	IndexedOffset   int64  `json:"indexedOffset"`
	ObservationID   string `json:"observationId"`
	ObservationHash string `json:"observationHash"`
}

// UsageReconciliationProof is staged evidence, not an active subtraction.
// Keeping this separate prevents a partial reconciliation from silently
// changing totals before contribution ownership and query integration finish.
type UsageReconciliationProof struct {
	ID            string               `json:"id"`
	RecoveryEpoch string               `json:"recoveryEpoch"`
	Kind          string               `json:"kind"`
	Left          UsageProofEndpoint   `json:"left"`
	Right         UsageProofEndpoint   `json:"right"`
	ExtraRight    []UsageProofEndpoint `json:"extraRight,omitempty"`
	Evidence      json.RawMessage      `json:"evidence"`
}

func UsageObservationHash(u UsageObservation) string {
	b, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// StageUsageReconciliationProof revalidates both inputs under the writer
// transaction and records evidence idempotently. No raw observation, session,
// organization row, pricing snapshot, or accounting total is changed.
func (s *Store) StageUsageReconciliationProof(ctx context.Context, p UsageReconciliationProof) error {
	if !validID(p.ID) || (p.Kind != "duplicate-codex-usage-v1" && p.Kind != "fork-baseline-v1") || p.RecoveryEpoch != s.epoch || p.Left.SessionID == p.Right.SessionID || p.Left.SourceID == p.Right.SourceID || !json.Valid(p.Evidence) || len(p.Evidence) > 8192 || len(p.ExtraRight) > 1 || (p.Kind == "duplicate-codex-usage-v1" && len(p.ExtraRight) != 0) {
		return ErrInvalid
	}
	for _, e := range p.ExtraRight {
		if e.SessionID != p.Right.SessionID || e.SourceID != p.Right.SourceID || e.Generation != p.Right.Generation || e.Revision != p.Right.Revision || e.IndexedOffset != p.Right.IndexedOffset || e.ObservationID == p.Right.ObservationID {
			return ErrInvalid
		}
	}
	for _, e := range proofEndpoints(p) {
		hash, err := hex.DecodeString(e.ObservationHash)
		if !validID(e.SessionID) || !validID(e.SourceID) || !validID(e.Generation) || !validID(e.ObservationID) || len(e.Revision) > 512 || e.IndexedOffset <= 0 || err != nil || len(hash) != 32 {
			return ErrInvalid
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	s.capacityMu.Lock()
	err = s.capacity(int64(len(raw)) + 65536)
	s.capacityMu.Unlock()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_reconciliation_proofs(id TEXT PRIMARY KEY,created_at TEXT NOT NULL,proof BLOB NOT NULL)`); err != nil {
		return err
	}
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT proof FROM usage_reconciliation_proofs WHERE id=?`, p.ID).Scan(&prior)
	if err == nil {
		if !bytes.Equal(prior, raw) {
			return ErrConflict
		}
		// A prior receipt means staged, never that its sources remain current.
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	for _, e := range proofEndpoints(p) {
		if err = verifyUsageProofEndpoint(ctx, tx, e, false); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_reconciliation_proofs VALUES(?,?,?)`, p.ID, stamp(time.Now()), raw); err != nil {
		return err
	}
	if s.options.BeforeCommit != nil {
		if err = s.options.BeforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func verifyUsageProofEndpoint(ctx context.Context, tx *sql.Tx, e UsageProofEndpoint, allowAppend bool) error {
	var raw []byte
	// A committed proof pins a verified prefix, not the eventual file length.
	// New bytes may extend it; the generation, revision, observation hash, and
	// proof's minimum indexed boundary must still match. Staging remains strict.
	offsetPredicate := "r.indexed_offset=?"
	if allowAppend {
		offsetPredicate = "r.indexed_offset>=?"
	}
	err := tx.QueryRowContext(ctx, `SELECT u.observation FROM usage_observations u
 JOIN sessions s ON s.id=u.session_id AND s.source_id=u.source_id AND s.generation=u.generation
 JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation
 WHERE u.id=? AND s.id=? AND s.source_id=? AND s.generation=?
 AND COALESCE(json_extract(s.projection,'$.projectionRevision'),'')=? AND u.projection_revision=? AND `+offsetPredicate, e.ObservationID, e.SessionID, e.SourceID, e.Generation, e.Revision, e.Revision, e.IndexedOffset).Scan(&raw)
	if err == sql.ErrNoRows {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var u UsageObservation
	if json.Unmarshal(raw, &u) != nil || u.ID != e.ObservationID || u.SessionID != e.SessionID || UsageObservationHash(u) != e.ObservationHash {
		return ErrConflict
	}
	var evidence struct {
		Offset *int64 `json:"offset"`
	}
	if json.Unmarshal(u.Evidence, &evidence) != nil || evidence.Offset == nil || *evidence.Offset < 0 || *evidence.Offset >= e.IndexedOffset {
		return ErrConflict
	}
	return nil
}

// UsageReconciliationProofCurrent distinguishes a durable receipt from usable
// evidence. Reindexing, changing an observation, shrinking the indexed prefix,
// or restoring to another epoch invalidates it without deleting audit evidence.
// Ordinary append-only progress preserves an unchanged verified prefix.
func (s *Store) UsageReconciliationProofCurrent(ctx context.Context, id string) (UsageReconciliationProof, bool, error) {
	var p UsageReconciliationProof
	if !validID(id) {
		return p, false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, false, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT proof FROM usage_reconciliation_proofs WHERE id=?`, id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			err = ErrNotFound
		}
		return p, false, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, false, err
	}
	if p.RecoveryEpoch != s.epoch {
		return p, false, nil
	}
	for _, e := range proofEndpoints(p) {
		if err = verifyUsageProofEndpoint(ctx, tx, e, true); err != nil {
			if err == ErrConflict {
				return p, false, nil
			}
			return p, false, err
		}
	}
	return p, true, nil
}

func proofEndpoints(p UsageReconciliationProof) []UsageProofEndpoint {
	return append([]UsageProofEndpoint{p.Left, p.Right}, p.ExtraRight...)
}

// UsageObservation retrieves one observation only from the current projection.
func (s *Store) UsageObservation(ctx context.Context, sessionID, observationID string) (UsageObservation, error) {
	var u UsageObservation
	if !validID(sessionID) || !validID(observationID) {
		return u, ErrInvalid
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT u.observation FROM usage_observations u JOIN sessions s ON s.id=u.session_id AND s.source_id=u.source_id AND s.generation=u.generation AND COALESCE(json_extract(s.projection,'$.projectionRevision'),'')=u.projection_revision WHERE u.id=? AND s.id=?`, observationID, sessionID).Scan(&raw)
	if err == sql.ErrNoRows {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	if err = json.Unmarshal(raw, &u); err != nil {
		return u, err
	}
	if u.ID != observationID || u.SessionID != sessionID {
		return u, ErrConflict
	}
	return u, nil
}
