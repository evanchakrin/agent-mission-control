package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

type ToolCount struct {
	Name  string `json:"name"`
	Calls int64  `json:"calls"`
}
type AgentToolPage struct {
	Tools           []ToolCount `json:"tools"`
	NextCursor      string      `json:"nextCursor,omitempty"`
	ThroughSequence int64       `json:"throughSequence"`
}
type agentToolCursor struct {
	Scope  string `json:"scope"`
	Head   int64  `json:"head"`
	Offset int64  `json:"offset"`
}

// Counts cover the selected agent's full indexed history up to a fixed record
// boundary. Appends cannot reorder later pages; publication invalidates them.
// Requested on demand: opening the Board never launches one scan per card.
func (s *Store) AgentTools(ctx context.Context, id, agent, cursor string, limit int) (AgentToolPage, error) {
	page := AgentToolPage{Tools: []ToolCount{}}
	if len(agent) > 4096 || len(cursor) > 1024 {
		return page, ErrInvalid
	}
	base, err := s.historySnapshot(ctx, id, "")
	if err != nil {
		return page, err
	}
	if err = s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	encoded, _ := json.Marshal([]string{base, "agent-tools", agent})
	hash := sha256.Sum256(encoded)
	cp := agentToolCursor{Scope: hex.EncodeToString(hash[:])}
	if cursor != "" {
		var prior agentToolCursor
		data, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(data, &prior) != nil || prior.Head < 0 || prior.Offset < 0 || prior.Offset > prior.Head {
			return page, ErrInvalid
		}
		if prior.Scope != cp.Scope {
			return page, ErrHistoryChanged
		}
		cp = prior
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(e.seq),0) FROM query_sessions q JOIN events e INDEXED BY query_events_scope ON e.session_id=q.id AND e.generation=q.generation AND e.projection_revision=q.projection_revision AND e.source_id=q.source_id WHERE q.id=? AND e.agent_id=?`, id, agent).Scan(&cp.Head)
		if err != nil {
			return page, err
		}
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, agentToolsSQL, id, agent, cp.Head, limit+1, cp.Offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var tool ToolCount
		if err = rows.Scan(&tool.Name, &tool.Calls); err != nil {
			return page, err
		}
		page.Tools = append(page.Tools, tool)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if err = rows.Close(); err != nil {
		return page, err
	}
	if _, err = s.historySnapshot(ctx, id, base); err != nil {
		return page, err
	}
	page.ThroughSequence = cp.Head
	if len(page.Tools) > limit {
		page.Tools = page.Tools[:limit]
		cp.Offset += int64(limit)
		data, _ := json.Marshal(cp)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

const agentToolsSQL = `SELECT CASE WHEN json_type(e.data,'$.tool')='text' THEN json_extract(e.data,'$.tool') ELSE '' END AS tool,COUNT(*) AS calls
FROM query_sessions q JOIN events e INDEXED BY query_events_scope ON e.session_id=q.id AND e.generation=q.generation AND e.projection_revision=q.projection_revision AND e.source_id=q.source_id
WHERE q.id=? AND e.agent_id=? AND e.seq<=? AND e.kind='tool-call'
GROUP BY tool ORDER BY calls DESC,tool LIMIT ? OFFSET ?`
