package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

type DelegationMatch struct {
	Sequence        int64  `json:"sequence"`
	SessionID       string `json:"sessionId"`
	SessionSnapshot string `json:"sessionSnapshot"`
	CallerAgentID   string `json:"callerAgentId"`
	MachineID       string `json:"machineId"`
	Provider        string `json:"provider"`
	Title           string `json:"title"`
	Timestamp       string `json:"timestamp"`
	Archived        bool   `json:"archived"`
}

type DelegationMatchPage struct {
	State        string            `json:"state"`
	Coverage     string            `json:"coverage"`
	Scope        string            `json:"scope"`
	Matches      []DelegationMatch `json:"matches"`
	Snapshot     string            `json:"snapshot"`
	NextSequence int64             `json:"nextSequence,omitempty"`
}

// Matches are delegation calls, not identified child runs or proven successes.
// Every page is bounded; missing fingerprints are not an exhaustive no-match.
func (s *Store) DelegationMatches(ctx context.Context, id string, anchor, after int64, limit int, expected string) (DelegationMatchPage, error) {
	out := DelegationMatchPage{Matches: []DelegationMatch{}, Coverage: "indexed-fingerprints-only", Scope: "other-current-sessions-including-archived"}
	if id == "" || len(id) > 4096 || anchor < 1 || after < 0 || limit < 1 || limit > 100 || len(expected) > 128 || after > 0 && expected == "" {
		return out, ErrInvalid
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var eligible bool
	var fingerprint string
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN e.kind='tool-call' AND json_extract(e.data,'$.tool') IN ('Task','Agent','spawn_agent','functions.spawn_agent','collaboration.spawn_agent') THEN 1 ELSE 0 END,COALESCE(d.state,'needs-reindex'),COALESCE(d.prompt_hash,'')
 FROM events e JOIN query_sessions q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision
 LEFT JOIN delegation_tasks_v1 d ON d.event_sequence=e.seq WHERE e.session_id=? AND e.seq=?`, id, anchor).Scan(&eligible, &out.State, &fingerprint)
	if err == sql.ErrNoRows {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if !eligible {
		out.State = "not-delegation"
	}
	var revision, interpretation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM properties WHERE key='delegation_revision'`).Scan(&revision); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT value FROM properties WHERE key='search_revision'`).Scan(&interpretation); err != nil {
		return out, err
	}
	key, _ := json.Marshal([]any{"delegation-matches-v1", s.epoch, interpretation, revision, id, anchor, out.State, fingerprint})
	hash := sha256.Sum256(key)
	out.Snapshot = hex.EncodeToString(hash[:])
	if expected != "" && expected != out.Snapshot {
		return out, ErrHistoryChanged
	}
	if out.State != "known" {
		return out, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.seq,e.session_id,e.agent_id,q.machine_id,q.provider,q.title,e.timestamp,q.archived,q.source_id,q.generation,q.projection_revision
 FROM delegation_tasks_v1 d INDEXED BY delegation_tasks_v1_match CROSS JOIN events e ON e.seq=d.event_sequence
 CROSS JOIN query_sessions q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision
 WHERE d.state='known' AND d.prompt_hash=? AND d.event_sequence>? AND e.session_id<>? ORDER BY d.event_sequence LIMIT ?`, fingerprint, after, id, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m DelegationMatch
		var source, generation, projection string
		if err = rows.Scan(&m.Sequence, &m.SessionID, &m.CallerAgentID, &m.MachineID, &m.Provider, &m.Title, &m.Timestamp, &m.Archived, &source, &generation, &projection); err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Matches) == limit {
			out.NextSequence = out.Matches[len(out.Matches)-1].Sequence
			break
		}
		m.SessionSnapshot = s.eventSnapshot(m.SessionID, source, generation, projection)
		out.Matches = append(out.Matches, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}
