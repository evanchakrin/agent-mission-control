package desktop

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// The compatibility store is still a JSON library. SQLite selects bounded rows
// without materializing every body in Go or sending the library to the browser.
func (m *Manager) playbookPage(r *http.Request) (any, error) {
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 50 {
			return nil, fail(400, "playbook limit must be between 1 and 50")
		}
		limit = n
	}
	id, before := r.URL.Query().Get("id"), r.URL.Query().Get("after")
	if len(id) > 512 || len(before) > 512 {
		return nil, fail(400, "invalid playbook identity")
	}
	query := `SELECT j.value FROM local_state s,json_each(s.value) j WHERE s.key='playbooks'`
	args := []any{}
	if r.URL.Query().Has("id") {
		if id == "" {
			return nil, fail(400, "playbook identity is required")
		}
		query += ` AND json_extract(j.value,'$.id')=?`
		args = append(args, id)
	} else if before != "" {
		query += ` AND json_extract(j.value,'$.id')>?`
		args = append(args, before)
	}
	query += ` ORDER BY json_extract(j.value,'$.id') LIMIT ?`
	args = append(args, limit+1)
	rows, err := m.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Playbook{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item Playbook
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if id != "" {
		if len(items) != 1 {
			return nil, fail(404, "playbook was not found uniquely")
		}
		return map[string]any{"item": items[0]}, nil
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[limit-1].ID
	}
	return map[string]any{"items": items, "nextCursor": next}, nil
}
