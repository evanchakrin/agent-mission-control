package store

import (
	"context"
	"sort"
)

// Unfiltered inventory includes evidence that has not produced a session. Keep
// session-filtered catalogs unchanged: raw sources cannot satisfy those filters.
func (s *Store) includeSourceOnlyMachines(ctx context.Context, q SessionQuery, page CatalogPage) (CatalogPage, error) {
	after, _, err := decodeKey(q.Cursor, "machine")
	if err != nil {
		return page, err
	}
	limit := pageLimit(q.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT i.machine_id,COALESCE(NULLIF(ml.display_name,''),i.machine_id)
 FROM source_identity i LEFT JOIN machine_labels ml ON ml.machine_id=i.machine_id
 WHERE i.machine_id>? AND NOT EXISTS(SELECT 1 FROM query_sessions q WHERE q.machine_id=i.machine_id)
 GROUP BY i.machine_id ORDER BY i.machine_id LIMIT ?`, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	more := page.NextCursor != ""
	for rows.Next() {
		var item CatalogItem
		item.SourceOnly = true
		if err = rows.Scan(&item.ID, &item.Name); err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].ID < page.Items[j].ID })
	more = more || len(page.Items) > limit
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
	}
	page.NextCursor = ""
	if more && len(page.Items) > 0 {
		page.NextCursor = encodeKey(page.Items[len(page.Items)-1].ID, "machine")
	}
	return page, nil
}
