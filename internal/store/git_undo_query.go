package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
)

type GitUndoEvent struct {
	Sequence int64 `json:"sequence"`
	hookideas.GitUndo
}

type GitUndoPage struct {
	State         string         `json:"state"`
	Snapshot      string         `json:"snapshot"`
	IndexedOffset int64          `json:"indexedOffset"`
	Attempts      int64          `json:"attempts"`
	Unsupported   int64          `json:"unsupported"`
	Diagnostics   int64          `json:"diagnostics"`
	Events        []GitUndoEvent `json:"events"`
	NextSequence  int64          `json:"nextSequence,omitempty"`
}

// GitUndoHistory reads only the published interpretation. Every displayed
// attempt links back to an indexed event and its retained raw source range.
func (s *Store) GitUndoHistory(ctx context.Context, id, snapshot string, after int64, limit int) (GitUndoPage, error) {
	out := GitUndoPage{State: "needs-rebuild", Events: []GitUndoEvent{}}
	if after < 0 || after > 0 && snapshot == "" || limit < 0 || limit > 100 {
		return out, ErrInvalid
	}
	if limit == 0 {
		limit = 50
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return out, err
	}
	out.Snapshot = token
	err = s.db.QueryRowContext(ctx, `SELECT c.indexed_offset,c.attempts,c.unsupported,c.diagnostics FROM sessions s JOIN git_undo_checkpoints c ON c.source_id=s.source_id AND c.generation=s.generation AND c.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'') WHERE s.id=?`, id).Scan(&out.IndexedOffset, &out.Attempts, &out.Unsupported, &out.Diagnostics)
	if err != nil && err != sql.ErrNoRows {
		return GitUndoPage{}, err
	}
	if err == nil {
		out.State = "indexed-history"
		if out.Unsupported > 0 || out.Diagnostics > 0 {
			out.State = "incomplete-indexing"
		}
		rows, err := s.db.QueryContext(ctx, `SELECT u.event_seq,u.evidence FROM sessions s JOIN git_undo_events u ON u.source_id=s.source_id AND u.generation=s.generation AND u.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'') WHERE s.id=? AND u.event_seq>? ORDER BY u.event_seq LIMIT ?`, id, after, limit+1)
		if err != nil {
			return GitUndoPage{}, err
		}
		for rows.Next() {
			var item GitUndoEvent
			var raw []byte
			if err := rows.Scan(&item.Sequence, &raw); err != nil {
				rows.Close()
				return GitUndoPage{}, err
			}
			if len(out.Events) == limit {
				out.NextSequence = out.Events[len(out.Events)-1].Sequence
				break
			}
			if json.Unmarshal(raw, &item.GitUndo) != nil {
				rows.Close()
				return GitUndoPage{}, ErrInvalid
			}
			out.Events = append(out.Events, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return GitUndoPage{}, err
		}
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return GitUndoPage{}, err
	}
	return out, nil
}
