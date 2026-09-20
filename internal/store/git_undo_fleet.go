package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"
)

type FleetGitUndoEvent struct {
	GitUndoEvent
	SessionID       string     `json:"sessionId"`
	SessionSnapshot string     `json:"sessionSnapshot"`
	Title           string     `json:"title"`
	Project         string     `json:"project"`
	MachineID       string     `json:"machineId"`
	MachineName     string     `json:"machineName"`
	Provider        string     `json:"provider"`
	Archived        bool       `json:"archived"`
	AgentID         string     `json:"agentId"`
	Timestamp       *time.Time `json:"timestamp"`
}
type FleetGitUndoPage struct {
	Project    *string             `json:"project"`
	Scope      string              `json:"scope"`
	Snapshot   string              `json:"snapshot"`
	Events     []FleetGitUndoEvent `json:"events"`
	NextCursor string              `json:"nextCursor,omitempty"`
}
type undoFleetCursor struct {
	At       string `json:"at"`
	Sequence int64  `json:"sequence"`
	Snapshot string `json:"snapshot"`
}

func (s *Store) undoFleetSnapshot(ctx context.Context, expected string, project *string) (string, error) {
	base, err := s.searchSnapshot(ctx, SearchQuery{}, "")
	if err != nil {
		return "", err
	}
	scope, _ := json.Marshal([]any{"git-undo-fleet-v2", base, project})
	hash := sha256.Sum256(scope)
	actual := hex.EncodeToString(hash[:])
	if expected != "" && actual != expected {
		return "", ErrHistoryChanged
	}
	return actual, nil
}

const undoFleetFrom = `FROM git_undo_events u INDEXED BY git_undo_time
CROSS JOIN events e ON e.seq=u.event_seq
CROSS JOIN query_sessions q ON q.id=e.session_id AND q.source_id=u.source_id AND q.generation=u.generation AND q.projection_revision=u.revision
JOIN git_undo_checkpoints c ON c.source_id=u.source_id AND c.generation=u.generation AND c.revision=u.revision `

// FleetGitUndoHistory streams the current interpretation in recorded-time order,
// including archived sessions. Appends are allowed; publication/restore invalidates
// continuation. Unknown timestamps sort last. No history-sized result is retained.
func (s *Store) FleetGitUndoHistory(ctx context.Context, cursor string, limit int) (FleetGitUndoPage, error) {
	return s.FleetGitUndoHistoryForProject(ctx, cursor, limit, nil)
}

// A nil project selects the fleet; an empty project selects an unrecorded folder.
// This is the recorded session project, not an inferred command working directory.
func (s *Store) FleetGitUndoHistoryForProject(ctx context.Context, cursor string, limit int, project *string) (FleetGitUndoPage, error) {
	out := FleetGitUndoPage{Scope: "all-current-sessions-including-archived", Project: project, Events: []FleetGitUndoEvent{}}
	if project != nil && len(*project) > 4096 {
		return out, ErrInvalid
	}
	if limit < 0 || limit > 100 || len(cursor) > 2048 {
		return out, ErrInvalid
	}
	if limit == 0 {
		limit = 50
	}
	var cp undoFleetCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || cp.Sequence <= 0 || len(cp.Snapshot) != 64 {
			return out, ErrInvalid
		}
		at, err := time.Parse(time.RFC3339Nano, cp.At)
		if err != nil || stamp(at) != cp.At {
			return out, ErrInvalid
		}
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	token, err := s.undoFleetSnapshot(ctx, cp.Snapshot, project)
	if err != nil {
		return out, err
	}
	out.Snapshot = token
	where, args := "WHERE 1=1 ", []any{}
	if project != nil {
		where += "AND q.project=? "
		args = append(args, *project)
	}
	if cursor != "" {
		where += "AND (u.timestamp,u.event_seq)<(?,?) "
		args = append(args, cp.At, cp.Sequence)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT u.event_seq,u.timestamp,u.evidence,q.id,e.agent_id,q.title,q.machine_id,
COALESCE(NULLIF((SELECT display_name FROM machine_labels ml WHERE ml.machine_id=q.machine_id),''),(SELECT json_extract(m.heartbeat,'$.name') FROM machines m WHERE m.machine_id=q.machine_id),''),
q.provider,q.archived,q.source_id,q.generation,q.projection_revision,q.project `+undoFleetFrom+where+`ORDER BY u.timestamp DESC,u.event_seq DESC LIMIT ?`, args...)
	if err != nil {
		return FleetGitUndoPage{}, err
	}
	var last undoFleetCursor
	for rows.Next() {
		var item FleetGitUndoEvent
		var at, source, generation, revision string
		var raw []byte
		if err := rows.Scan(&item.Sequence, &at, &raw, &item.SessionID, &item.AgentID, &item.Title, &item.MachineID, &item.MachineName, &item.Provider, &item.Archived, &source, &generation, &revision, &item.Project); err != nil {
			rows.Close()
			return FleetGitUndoPage{}, err
		}
		if len(out.Events) == limit {
			b, _ := json.Marshal(last)
			out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		if json.Unmarshal(raw, &item.GitUndo) != nil {
			rows.Close()
			return FleetGitUndoPage{}, ErrInvalid
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return FleetGitUndoPage{}, ErrInvalid
		}
		if !parsed.IsZero() {
			item.Timestamp = &parsed
		}
		item.SessionSnapshot = s.eventSnapshot(item.SessionID, source, generation, revision)
		out.Events = append(out.Events, item)
		last = undoFleetCursor{at, item.Sequence, token}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return FleetGitUndoPage{}, err
	}
	if _, err = s.undoFleetSnapshot(ctx, token, project); err != nil {
		return FleetGitUndoPage{}, err
	}
	return out, nil
}

type GitUndoSummary struct {
	Scope         string `json:"scope"`
	Sessions      int64  `json:"sessions"`
	NeedsRebuild  int64  `json:"needsRebuild"`
	Incomplete    int64  `json:"incomplete"`
	Ready         int64  `json:"ready"`
	Attempts      int64  `json:"attempts"`
	ReadyAttempts int64  `json:"readyAttempts"`
	Unsupported   int64  `json:"unsupported"`
	Diagnostics   int64  `json:"diagnostics"`
}

func (s *Store) GitUndoSummary(ctx context.Context) (GitUndoSummary, error) {
	out := GitUndoSummary{Scope: "all-current-sessions-including-archived"}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `WITH evidence AS (
SELECT c.attempts,c.unsupported,c.diagnostics,
CASE WHEN c.source_id IS NULL OR r.source_id IS NULL OR c.indexed_offset<>r.indexed_offset THEN 'missing'
WHEN c.unsupported>0 OR c.diagnostics>0 OR r.durable_offset<>r.indexed_offset
OR COALESCE(json_extract(s.projection,'$.completeness'),'') NOT IN ('indexed-source','complete') THEN 'incomplete'
ELSE 'ready' END AS state
FROM sessions s LEFT JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation
LEFT JOIN git_undo_checkpoints c ON c.source_id=s.source_id AND c.generation=s.generation AND c.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'')
) SELECT COUNT(*),COALESCE(SUM(state='missing'),0),COALESCE(SUM(state='incomplete'),0),COALESCE(SUM(state='ready'),0),
COALESCE(SUM(CASE WHEN state<>'missing' THEN attempts ELSE 0 END),0),COALESCE(SUM(CASE WHEN state='ready' THEN attempts ELSE 0 END),0),
COALESCE(SUM(CASE WHEN state<>'missing' THEN unsupported ELSE 0 END),0),COALESCE(SUM(CASE WHEN state<>'missing' THEN diagnostics ELSE 0 END),0) FROM evidence`).Scan(&out.Sessions, &out.NeedsRebuild, &out.Incomplete, &out.Ready, &out.Attempts, &out.ReadyAttempts, &out.Unsupported, &out.Diagnostics)
	if err != nil {
		return GitUndoSummary{}, err
	}
	return out, tx.Commit()
}
