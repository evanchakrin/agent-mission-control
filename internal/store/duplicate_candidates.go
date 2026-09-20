package store

import (
	"context"
	"encoding/json"
)

func (s *Store) CanonicalDuplicateSession(ctx context.Context, session Session) (string, error) {
	if session.MachineID == "" || session.NativeID == "" || session.Provider != "codex" {
		return "", ErrInvalid
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(id),'') FROM query_sessions WHERE machine_id=? AND provider=? AND native_id=?`, session.MachineID, session.Provider, session.NativeID).Scan(&id)
	if err == nil && id == "" {
		err = ErrNotFound
	}
	return id, err
}

// NativeSessionCopies pages evidence sources without choosing contribution ownership.
func (s *Store) NativeSessionCopies(ctx context.Context, machine, native, after string, limit int) ([]string, error) {
	if machine == "" || native == "" || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM query_sessions WHERE machine_id=? AND provider='codex' AND native_id=? AND id>? ORDER BY id LIMIT ?`, machine, native, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Seek candidates using the existing source/timestamp index, not a scan of the
// parent's complete history. Three rows mean ambiguity to the reconciler; no
// history is discarded and no exclusion is inferred from a truncated set.
func (s *Store) DuplicateUsageCandidates(ctx context.Context, session Session, u UsageObservation) ([]UsageObservation, error) {
	if u.Timestamp.IsZero() {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT original.observation FROM query_usage candidate INDEXED BY query_usage_scope_time JOIN usage_observations original ON original.id=candidate.id WHERE candidate.session_id=? AND candidate.source_id=? AND candidate.generation=? AND candidate.projection_revision=? AND candidate.timestamp=? AND candidate.agent_id=? AND candidate.model=? AND candidate.kind=? AND candidate.tokens_in=? AND candidate.tokens_cache=? AND candidate.tokens_write=? AND candidate.tokens_out=? LIMIT 3`, session.ID, session.SourceID, session.Generation, session.ProjectionRevision, stamp(u.Timestamp), u.AgentID, u.Model, u.Kind, u.TokensIn, u.TokensCache, u.TokensCacheWrite, u.TokensOut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []UsageObservation{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v UsageObservation
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
