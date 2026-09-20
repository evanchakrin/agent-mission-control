package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// SourceCatalog includes retained generations without requiring a parsed session.
// It does not expose parser checkpoints or imply that captured bytes are complete.
type SourceCatalogItem struct {
	Source           protocol.Source `json:"source"`
	DurableOffset    int64           `json:"durableOffset"`
	IndexedOffset    int64           `json:"indexedOffset"`
	ActiveGeneration bool            `json:"activeGeneration"`
	SessionID        string          `json:"sessionId,omitempty"`
	Title            string          `json:"title,omitempty"`
}
type SourceCatalogPage struct {
	MachineID  string              `json:"machineId"`
	Query      string              `json:"query"`
	Items      []SourceCatalogItem `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}
type sourceCatalogCursor struct {
	Machine, Epoch, Source, Generation, Query string
}

func (s *Store) SourceCatalog(ctx context.Context, machine, cursor string, limit int) (SourceCatalogPage, error) {
	return s.SearchSourceCatalog(ctx, machine, cursor, limit, "")
}

func (s *Store) SearchSourceCatalog(ctx context.Context, machine, cursor string, limit int, text string) (SourceCatalogPage, error) {
	out := SourceCatalogPage{MachineID: machine, Query: text, Items: []SourceCatalogItem{}}
	if !validID(machine) || limit < 1 || limit > 100 || len(cursor) > 8192 || len(text) > 1024 || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return out, ErrInvalid
	}
	cp := sourceCatalogCursor{Machine: machine, Epoch: s.RecoveryEpoch()}
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cp) != nil || !validID(cp.Source) || !validID(cp.Generation) {
			return out, ErrInvalid
		}
		if cp.Machine != machine || cp.Epoch != s.RecoveryEpoch() || cp.Query != text {
			return out, ErrHistoryChanged
		}
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	// The current query projection already tracks indexed titles and owner
	// names transactionally. Do not parse whole session JSON for each search.
	const title = `COALESCE(se.title,'')`
	rows, err := s.db.QueryContext(ctx, `SELECT so.meta_json,so.durable_offset,so.indexed_offset,i.active_generation=so.generation,
 COALESCE(se.id,''),`+title+`
 FROM source_identity i INDEXED BY source_identity_machine JOIN sources so ON so.source_id=i.source_id
 LEFT JOIN query_sessions se ON se.id=(SELECT linked.id FROM sessions linked INDEXED BY sessions_source_generation WHERE linked.source_id=so.source_id AND linked.generation=so.generation ORDER BY linked.id LIMIT 1)
 WHERE i.machine_id=? AND i.source_id>=? AND (so.source_id,so.generation)>(?,?)
 AND (?='' OR instr(lower(json_extract(so.meta_json,'$.path')),lower(?))>0 OR instr(lower(`+title+`),lower(?))>0)
 ORDER BY i.source_id,so.generation LIMIT ?`, machine, cp.Source, cp.Source, cp.Generation, text, text, text, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item SourceCatalogItem
		var raw []byte
		if err = rows.Scan(&raw, &item.DurableOffset, &item.IndexedOffset, &item.ActiveGeneration, &item.SessionID, &item.Title); err != nil {
			return out, err
		}
		if err = json.Unmarshal(raw, &item.Source); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			last := out.Items[len(out.Items)-1].Source
			b, _ := json.Marshal(sourceCatalogCursor{machine, s.RecoveryEpoch(), last.SourceID, last.Generation, text})
			out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}
