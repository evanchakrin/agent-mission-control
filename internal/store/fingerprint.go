package store

import (
	"context"
	"database/sql"
)

type Fingerprint struct {
	Tiers               []FingerprintTier `json:"tiers"`
	RecordedTokens      int64             `json:"recordedTokens"`
	SessionID           string            `json:"sessionId"`
	Buckets             [24]int64         `json:"buckets"`
	Errors              [24]int64         `json:"errors"`
	Events              int64             `json:"events"`
	UndatedEvents       int64             `json:"undatedEvents"`
	UndatedActivity     int64             `json:"undatedActivity"`
	UndatedErrors       int64             `json:"undatedErrors"`
	UnknownResultStatus int64             `json:"unknownResultStatus"`
	DurationMS          *float64          `json:"durationMs"`
	ThroughSequence     int64             `json:"throughSequence"`
}

type FingerprintTier struct {
	Tier         string  `json:"tier"`
	PricedTokens int64   `json:"pricedTokens"`
	Cost         float64 `json:"cost"`
}

const fingerprintTiersSQL = `WITH selected AS (SELECT * FROM query_sessions WHERE id=?),` + selectedTierCTEs + `
 classified AS (SELECT r.tier,json_extract(p.evidence,'$.pricedTokens') AS tokens,json_extract(p.evidence,'$.cost') AS cost
 FROM selected q JOIN snapshots a ON a.session_id=q.id
 JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
 JOIN observation_prices p ON p.snapshot_id=a.snapshot_id AND p.observation_id=u.id AND p.agent_id=u.agent_id AND p.model=u.model
 AND json_extract(p.evidence,'$.recordedTokens')=u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out
 JOIN rates r ON r.catalog_id=a.catalog_id AND r.rate_id=json_extract(p.evidence,'$.rateIds[0]')
 WHERE json_array_length(p.evidence,'$.rateIds')=1 AND r.tier IN('flagship','premium','mid','cheap') AND json_extract(p.evidence,'$.pricedTokens')>0)
 SELECT tier,SUM(tokens),SUM(cost) FROM classified GROUP BY tier ORDER BY tier`

const fingerprintScope = ` FROM query_sessions q JOIN events e INDEXED BY query_events_scope ON e.session_id=q.id AND e.source_id=q.source_id AND e.generation=q.generation AND e.projection_revision=q.projection_revision WHERE q.id=?`

// Zero timestamps are missing evidence, not a run starting in year one.
const fingerprintTime = `CASE WHEN e.timestamp='' OR substr(e.timestamp,1,4)='0001' THEN NULL ELSE julianday(e.timestamp) END`

// A fixed-size response over the whole current projection, never a transcript
// tail. Both passes share a read snapshot; no event bodies enter Go memory.
func (s *Store) SessionFingerprint(ctx context.Context, id string) (Fingerprint, error) {
	out := Fingerprint{SessionID: id, Tiers: []FingerprintTier{}}
	if _, err := s.historySnapshot(ctx, id, ""); err != nil {
		return out, err
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		return out, err
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var first, last sql.NullFloat64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(e.seq),0),MIN(`+fingerprintTime+`),MAX(`+fingerprintTime+`)`+fingerprintScope, id).Scan(&out.Events, &out.ThroughSequence, &first, &last)
	if err != nil {
		return out, err
	}
	span := 0.0
	if first.Valid && last.Valid {
		span = last.Float64 - first.Float64
		duration := span * 86400000
		out.DurationMS = &duration
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN (`+fingerprintTime+`) IS NULL THEN -1 WHEN ?<=0 THEN 0 ELSE MIN(23,MAX(0,CAST(((`+fingerprintTime+`)-?)/?*24 AS INTEGER))) END AS bucket,
 COUNT(*),COALESCE(SUM(e.kind IN('tool-call','tool-result','spawn')),0),COALESCE(SUM(e.kind='tool-result' AND json_extract(e.data,'$.error')=1),0),
 COALESCE(SUM(e.kind='tool-result' AND (json_extract(e.data,'$.error') IS NULL OR json_extract(e.data,'$.error') NOT IN(0,1))),0)`+fingerprintScope+` GROUP BY bucket`, span, first.Float64, span, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int
		var count, activity, failures, unknown int64
		if err = rows.Scan(&bucket, &count, &activity, &failures, &unknown); err != nil {
			return out, err
		}
		out.UnknownResultStatus += unknown
		if bucket < 0 {
			out.UndatedEvents = count
			out.UndatedActivity = activity
			out.UndatedErrors = failures
		} else {
			out.Buckets[bucket] = activity
			out.Errors[bucket] = failures
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT tokens_in+tokens_cache+tokens_write+tokens_out FROM query_sessions WHERE id=?`, id).Scan(&out.RecordedTokens); err != nil {
		return out, err
	}
	tiers, err := tx.QueryContext(ctx, fingerprintTiersSQL, id)
	if err != nil {
		return out, err
	}
	defer tiers.Close()
	for tiers.Next() {
		var tier FingerprintTier
		if err = tiers.Scan(&tier.Tier, &tier.PricedTokens, &tier.Cost); err != nil {
			return out, err
		}
		out.Tiers = append(out.Tiers, tier)
	}
	return out, tiers.Err()
}
