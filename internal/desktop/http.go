package desktop

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type input struct {
	OperationID      string   `json:"operationId"`
	Op               string   `json:"op"`
	ID               string   `json:"id"`
	Path             string   `json:"path"`
	Name             string   `json:"name"`
	Title            string   `json:"title"`
	Body             string   `json:"body"`
	Content          *string  `json:"content"`
	ExpectedHash     string   `json:"expectedHash"`
	ExpectedRevision *int64   `json:"expectedRevision"`
	BaseMtime        float64  `json:"baseMtime"`
	Kind             string   `json:"kind"`
	Source           string   `json:"source"`
	Key              string   `json:"key"`
	Status           string   `json:"status"`
	Note             string   `json:"note"`
	Targets          []string `json:"targets"`
	Topic            string   `json:"topic"`
	InsightKey       string   `json:"insightKey"`
	ReviewEveryDays  int      `json:"reviewEveryDays"`
}

func (m *Manager) gate(r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fail(403, "local desktop access only")
	}
	host = r.Host
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fail(421, "invalid desktop host")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return fail(403, "cross-site request refused")
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		return fail(403, "invalid desktop origin")
	}
	if r.Method == http.MethodPost {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			return fail(415, "JSON content is required")
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-MC-CSRF")), []byte(m.opts.CSRFToken)) != 1 {
			return fail(403, "refresh the desktop page before saving")
		}
	} else if r.Method != http.MethodGet {
		return fail(405, "method is not supported")
	}
	return nil
}
func respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		code := http.StatusInternalServerError
		message := "Desktop operation failed; its outcome may be uncertain. Preserve pending work before retrying."
		var api *apiError
		if errors.As(err, &api) {
			code = api.code
			message = api.Error()
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}
func (m *Manager) handle(w http.ResponseWriter, r *http.Request) {
	if err := m.gate(r); err != nil {
		respond(w, nil, err)
		return
	}
	var b input
	if r.Method == http.MethodPost {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err := d.Decode(&b); err != nil {
			respond(w, nil, fail(400, "invalid request body"))
			return
		}
		if d.Decode(new(any)) != io.EOF {
			respond(w, nil, fail(400, "request must contain one JSON object"))
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	var value any
	var err error
	switch r.URL.Path {
	case "/api/brain":
		if r.Method != http.MethodGet {
			err = fail(405, "read-only route")
			break
		}
		var items []Item
		items, err = m.inventory()
		value = map[string]any{"items": items, "csrf": m.opts.CSRFToken}
	case "/api/brain/file":
		value, err = m.brainFile(r, b)
	case "/api/brain/history", "/api/brain/snapshot":
		if r.Method != http.MethodGet {
			err = fail(405, "read-only route")
			break
		}
		value, err = m.brainHistory(r)
	case "/api/directives":
		value, err = m.directives(r, b)
	case "/api/playbooks":
		value, err = m.playbooks(r, b)
	case "/api/triage":
		value, err = m.triage(r, b)
	case "/api/audit":
		if r.Method != http.MethodGet {
			err = fail(405, "read-only route")
			break
		}
		value, err = m.auditList(r)
	default:
		err = fail(404, "unknown local route")
	}
	respond(w, value, err)
}
func (m *Manager) brainFile(r *http.Request, b input) (any, error) {
	id := b.ID
	if r.Method == http.MethodGet {
		id = r.URL.Query().Get("id")
	}
	item, err := m.resolve(id, true)
	if err != nil {
		return nil, err
	}
	content, err := readBounded(item.Path)
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodGet {
		item.ExpectedHash = hash(content)
		return struct {
			Item
			Content string `json:"content"`
		}{item, string(content)}, nil
	}
	if b.Content == nil {
		return nil, fail(400, "content is required")
	}
	expected := b.ExpectedHash
	if expected == "" {
		if b.BaseMtime == 0 || math.Abs(item.Mtime-b.BaseMtime) > 1 {
			return nil, fail(409, "file changed on disk — reload before saving")
		}
		expected = hash(content)
	}
	if err = m.writeFile(item, expected, []byte(*b.Content), "brain-write"); err != nil {
		return nil, err
	}
	st, err := os.Stat(item.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "mtime": float64(st.ModTime().UnixNano()) / 1e6, "expectedHash": hash([]byte(*b.Content))}, nil
}
func (m *Manager) brainHistory(r *http.Request) (any, error) {
	item, err := m.resolve(r.URL.Query().Get("id"), true)
	if err != nil {
		return nil, err
	}
	if r.URL.Path == "/api/brain/snapshot" {
		var content []byte
		if err = m.db.QueryRow("SELECT content FROM snapshots WHERE path=? AND stamp=?", item.Path, r.URL.Query().Get("stamp")).Scan(&content); err != nil {
			return nil, fail(404, "snapshot was not found")
		}
		return map[string]string{"content": string(content)}, nil
	}
	query := "SELECT stamp,length(content),created_at FROM snapshots WHERE path=?"
	args := []any{item.Path}
	if before := r.URL.Query().Get("before"); before != "" {
		if len(before) > 256 {
			return nil, fail(400, "invalid snapshot cursor")
		}
		var at int64
		if err := m.db.QueryRowContext(r.Context(), "SELECT created_at FROM snapshots WHERE path=? AND stamp=?", item.Path, before).Scan(&at); err != nil {
			return nil, fail(400, "snapshot cursor does not belong to this file")
		}
		query += " AND (created_at < ? OR (created_at = ? AND stamp < ?))"
		args = append(args, at, at, before)
	}
	rows, err := m.db.QueryContext(r.Context(), query+" ORDER BY created_at DESC,stamp DESC LIMIT 101", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := []map[string]any{}
	for rows.Next() {
		var stamp string
		var size, at int64
		if err = rows.Scan(&stamp, &size, &at); err != nil {
			return nil, err
		}
		history = append(history, map[string]any{"stamp": stamp, "name": item.Name, "size": size, "at": at})
	}
	next := ""
	if len(history) > 100 {
		history = history[:100]
		next = history[99]["stamp"].(string)
	}
	return map[string]any{"history": history, "nextCursor": next}, rows.Err()
}

type Playbook struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Revision  int64  `json:"revision"`
}

func (m *Manager) playbooks(r *http.Request, b input) (any, error) {
	if r.Method == http.MethodGet && (r.URL.Query().Has("limit") || r.URL.Query().Has("id")) {
		return m.playbookPage(r)
	}
	var receiptKey, requestHash string
	if r.Method == http.MethodPost && b.OperationID != "" {
		if len(b.OperationID) > 128 || strings.ContainsAny(b.OperationID, " \r\n\t/\\") || b.ExpectedRevision == nil {
			return nil, fail(400, "operation identity and expected revision are required")
		}
		receiptKey = "playbook-operation:" + b.OperationID
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		requestHash = hash(raw)
		var prior localOperationReceipt
		if err = m.load(receiptKey, &prior); err != nil {
			return nil, err
		}
		if prior.RequestHash != "" {
			if prior.RequestHash != requestHash {
				return nil, fail(409, "operation identity was already used for a different request")
			}
			return prior.Result, nil
		}
	}
	items := []Playbook{}
	if err := m.load("playbooks", &items); err != nil {
		return nil, err
	}
	if r.Method == http.MethodGet {
		return map[string]any{"v": 1, "items": items}, nil
	}
	// Legacy callers may omit revisions during the parallel migration. The v2
	// editor supplies an explicit observed revision, including zero for imports.
	if b.ExpectedRevision != nil {
		if *b.ExpectedRevision < 0 {
			return nil, fail(400, "invalid playbook revision")
		}
		if b.ID == "" {
			if *b.ExpectedRevision != 0 {
				return nil, fail(409, "new playbook requires revision zero")
			}
		} else {
			found := false
			for _, item := range items {
				if item.ID == b.ID {
					found = true
					if item.Revision != *b.ExpectedRevision {
						return nil, fail(409, "playbook changed; reload and compare before saving")
					}
				}
			}
			if !found {
				return nil, fail(409, "playbook was removed; reload before changing it")
			}
		}
	}
	switch b.Op {
	case "save":
		if len(b.Body) > 60000 {
			return nil, fail(400, "playbook is too long")
		}
		name := clean(b.Name, 100)
		if name == "" {
			name = "Untitled play"
		}
		found := -1
		for i := range items {
			if items[i].ID == b.ID {
				found = i
				break
			}
		}
		if b.ID != "" {
			if found < 0 {
				return nil, fail(404, "playbook was not found")
			}
			items[found].Name = name
			items[found].Body = b.Body
			if b.Kind != "" {
				items[found].Kind = clean(b.Kind, 40)
			}
			items[found].UpdatedAt = now()
			if items[found].Revision == math.MaxInt64 {
				return nil, fail(409, "playbook revision exhausted")
			}
			items[found].Revision++
		} else {
			if len(items) >= 300 {
				return nil, fail(400, "playbook library is full")
			}
			kind := clean(b.Kind, 40)
			if kind == "" {
				kind = "custom"
			}
			source := clean(b.Source, 40)
			if source == "" {
				source = "manual"
			}
			items = append(items, Playbook{randomID("pb_"), name, kind, b.Body, source, now(), now(), 1})
		}
	case "delete":
		for i := range items {
			if items[i].ID == b.ID {
				items = append(items[:i], items[i+1:]...)
				break
			}
		}
	default:
		return nil, fail(400, "unknown playbook action")
	}
	if receiptKey != "" {
		result := map[string]any{"ok": true, "operationId": b.OperationID, "deleted": b.Op == "delete", "id": b.ID}
		if b.Op == "save" {
			if b.ID == "" {
				result["item"] = items[len(items)-1]
				result["id"] = items[len(items)-1].ID
			} else {
				for _, item := range items {
					if item.ID == b.ID {
						result["item"] = item
						break
					}
				}
			}
		}
		if err := m.saveOperation("playbooks", items, "playbook-"+b.Op, receiptKey, &localOperationReceipt{RequestHash: requestHash, Result: result}); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err := m.save("playbooks", items, "playbook-"+b.Op); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "items": items}, nil
}
func (m *Manager) triage(r *http.Request, b input) (any, error) {
	items := map[string]any{}
	if err := m.load("triage", &items); err != nil {
		return nil, err
	}
	if r.Method == http.MethodPost {
		if b.Key == "" || len(b.Key) > 500 {
			return nil, fail(400, "triage key is required")
		}
		if b.Status == "open" {
			delete(items, b.Key)
		} else {
			status := clean(b.Status, 20)
			if status == "" {
				status = "resolved"
			}
			items[b.Key] = map[string]any{"status": status, "at": now(), "note": clean(b.Note, 500)}
		}
		if err := m.save("triage", items, "triage"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"ok": true, "triage": items}, nil
}
func (m *Manager) auditList(r *http.Request) (any, error) {
	limit := 400
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 400 {
			return nil, fail(400, "audit limit must be between 1 and 400")
		}
		limit = value
	}
	query := "SELECT id,at,kind,path,status,detail FROM audit"
	args := []any{}
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			return nil, fail(400, "invalid audit cursor")
		}
		query += " WHERE id < ?"
		args = append(args, before)
	}
	args = append(args, limit+1)
	rows, err := m.db.QueryContext(r.Context(), query+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []map[string]any{}
	for rows.Next() {
		var id, at int64
		var kind, path, status, detail string
		if err = rows.Scan(&id, &at, &kind, &path, &status, &detail); err != nil {
			return nil, err
		}
		entry := map[string]any{}
		_ = json.Unmarshal([]byte(detail), &entry)
		entry["id"] = id
		entry["at"] = at
		entry["kind"] = kind
		entry["path"] = path
		entry["status"] = status
		entries = append(entries, entry)
	}
	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		next = strconv.FormatInt(entries[limit-1]["id"].(int64), 10)
	}
	return map[string]any{"entries": entries, "nextCursor": next}, rows.Err()
}
