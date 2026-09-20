package store

import (
	"context"
	"strings"
)

// CurrentAttributionPrices reads bounded groups from the selected published
// estimate only. Unpublished and superseded evidence is intentionally invisible.
func (s *Store) CurrentAttributionPrices(ctx context.Context, session, dimension string, keys []string) (map[string]SessionPricing, error) {
	out := map[string]SessionPricing{}
	if dimension != "agent" && dimension != "model" || len(keys) > 500 {
		return nil, ErrInvalid
	}
	// One SQL statement observes one selected snapshot, including 500-row API
	// pages. Splitting the query could mix rate catalogs during publication.
	if len(keys) == 0 {
		return out, nil
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return nil, err
	}
	column := "p.agent_id"
	if dimension == "model" {
		column = "p.model"
	}
	args := []any{session}
	marks := make([]string, len(keys))
	for i, key := range keys {
		if len(key) > 512 {
			return nil, ErrInvalid
		}
		marks[i] = "?"
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+column+`,a.id,json_extract(a.estimate,'$.catalogId'),json_extract(a.estimate,'$.context'),
 sum(json_extract(p.evidence,'$.cost')),sum(json_extract(p.evidence,'$.recordedTokens')),sum(json_extract(p.evidence,'$.pricedTokens')),sum(json_extract(p.evidence,'$.unpricedTokens')),sum(json_extract(p.evidence,'$.unattributedTokens'))
 FROM observation_prices p JOIN accounting_estimates a ON a.id=p.snapshot_id JOIN sessions s ON s.id=a.session_id AND json_extract(s.projection,'$.pricing.snapshotId')=a.id
 WHERE s.id=? AND json_extract(a.estimate,'$.attributionVersion')=1 AND `+column+` IN (`+strings.Join(marks, ",")+`) GROUP BY `+column, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var p SessionPricing
		if err = rows.Scan(&key, &p.SnapshotID, &p.CatalogID, &p.Context, &p.Cost, &p.RecordedTokens, &p.PricedTokens, &p.UnpricedTokens, &p.UnattributedTokens); err != nil {
			return nil, err
		}
		if p.RecordedTokens > 0 {
			p.Coverage = float64(p.PricedTokens) / float64(p.RecordedTokens)
		}
		out[key] = p
	}
	return out, rows.Err()
}
