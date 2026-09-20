package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

// Ranked catalog pages are invalidated after committed changes rather than
// silently duplicating or skipping projects whose position moved between pages.
func (s *Store) RankedProjects(ctx context.Context, q SessionQuery) (CatalogPage, error) {
	page := CatalogPage{Items: []CatalogItem{}}
	if err := s.ensureAnalytics(ctx); err != nil {
		return page, err
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var head int64
	var epoch string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0),(SELECT value FROM properties WHERE key='recovery_epoch') FROM changes`).Scan(&head, &epoch); err != nil {
		return page, err
	}
	identity := q
	identity.Cursor = ""
	identity.Limit = 0
	encoded, _ := json.Marshal(identity)
	dimension := fmt.Sprintf("ranked-projects:%x", sha256.Sum256(append(encoded, []byte(fmt.Sprintf(":%s:%d", epoch, head))...)))
	if q.Cursor != "" {
		if len(q.Cursor) > 8192 {
			return page, ErrInvalid
		}
		var prior keyCursor
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || json.Unmarshal(raw, &prior) != nil {
			return page, ErrInvalid
		}
		if prior.Dimension != dimension {
			return page, ErrHistoryChanged
		}
	}
	key, has, err := decodeKey(q.Cursor, dimension)
	if err != nil {
		return page, err
	}
	offset := int64(0)
	if has {
		offset, err = strconv.ParseInt(key, 10, 64)
		if err != nil || offset < 0 || offset > 1000000000 {
			return page, ErrInvalid
		}
	}
	limit := pageLimit(q.Limit)
	rows, err := tx.QueryContext(ctx, `SELECT project,COUNT(*) AS sessions,MAX(last_activity) AS latest FROM query_sessions q WHERE `+where+` GROUP BY project ORDER BY sessions DESC,latest DESC,project LIMIT ? OFFSET ?`, append(args, limit+1, offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item CatalogItem
		if err = rows.Scan(&item.ID, &item.Sessions, &item.LastActivity); err != nil {
			return page, err
		}
		item.Name = item.ID
		if item.Name == "" {
			item.Name = "Unassigned"
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeKey(strconv.FormatInt(offset+int64(limit), 10), dimension)
	}
	return page, nil
}
