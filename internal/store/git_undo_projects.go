package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

type GitUndoProject struct {
	Project  string `json:"project"`
	Attempts int64  `json:"attempts"`
	Sessions int64  `json:"sessions"`
}

type GitUndoProjectPage struct {
	Scope      string           `json:"scope"`
	Snapshot   string           `json:"snapshot"`
	Projects   []GitUndoProject `json:"projects"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type undoProjectCursor struct {
	Project  string `json:"project"`
	Attempts int64  `json:"attempts"`
	Snapshot string `json:"snapshot"`
}

// GitUndoProjects ranks exact recorded session folders, including archived
// sessions. Counts describe recognized attempts, not complete capture or success.
// Stream the aggregate to fingerprint its entire ordering with bounded Go memory.
// Unlike event keysets, ranked counts can move on append: reject changed rankings
// rather than silently skip or repeat groups across continuation requests.
func (s *Store) GitUndoProjects(ctx context.Context, cursor string, limit int) (GitUndoProjectPage, error) {
	out := GitUndoProjectPage{Scope: "all-current-sessions-including-archived", Projects: []GitUndoProject{}}
	if limit < 0 || limit > 100 || len(cursor) > 8192 {
		return out, ErrInvalid
	}
	if limit == 0 {
		limit = 25
	}
	var cp undoProjectCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || cp.Attempts < 1 || len(cp.Project) > 4096 || len(cp.Snapshot) != 64 {
			return out, ErrInvalid
		}
		if _, err := hex.DecodeString(cp.Snapshot); err != nil {
			return out, ErrInvalid
		}
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	base, err := s.undoFleetSnapshot(ctx, "", nil)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT q.project,COUNT(*),COUNT(DISTINCT q.id) `+undoFleetFrom+`GROUP BY q.project ORDER BY COUNT(*) DESC,q.project COLLATE BINARY`)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	encoder := json.NewEncoder(h)
	_ = encoder.Encode([]string{"git-undo-projects-v1", base})
	more := false
	for rows.Next() {
		var item GitUndoProject
		if err := rows.Scan(&item.Project, &item.Attempts, &item.Sessions); err != nil {
			rows.Close()
			return out, err
		}
		_ = encoder.Encode(item)
		if cursor != "" && (item.Attempts > cp.Attempts || item.Attempts == cp.Attempts && item.Project <= cp.Project) {
			continue
		}
		if len(out.Projects) < limit {
			out.Projects = append(out.Projects, item)
		} else {
			more = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GitUndoProjectPage{}, err
	}
	if err = tx.Commit(); err != nil {
		return GitUndoProjectPage{}, err
	}
	out.Snapshot = hex.EncodeToString(h.Sum(nil))
	if cursor != "" && cp.Snapshot != out.Snapshot {
		return GitUndoProjectPage{}, ErrHistoryChanged
	}
	if _, err = s.undoFleetSnapshot(ctx, base, nil); err != nil {
		return GitUndoProjectPage{}, err
	}
	if more {
		last := out.Projects[len(out.Projects)-1]
		raw, _ := json.Marshal(undoProjectCursor{last.Project, last.Attempts, out.Snapshot})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
