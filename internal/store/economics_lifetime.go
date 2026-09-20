package store

import (
	"context"
	"database/sql"
)

type LifetimeBucket struct {
	Label                string   `json:"label"`
	Agents               int64    `json:"agents"`
	Messages             int64    `json:"messages"`
	RecordedTokens       int64    `json:"recordedTokens"`
	CostEligibleAgents   int64    `json:"costEligibleAgents"`
	CostEligibleMessages int64    `json:"costEligibleMessages"`
	ComparableCost       *float64 `json:"comparableCost"`
}
type EconomicsLifetime struct {
	Scope                       string           `json:"scope"`
	ChildSources                int64            `json:"childSources"`
	WithoutMessageObservations  int64            `json:"withoutMessageObservations"`
	WithUnstableMessageIdentity int64            `json:"withUnstableMessageIdentity"`
	Buckets                     []LifetimeBucket `json:"buckets"`
}

// Lifetimes count deduplicated usage-bearing provider messages, not elapsed
// duration or every chat turn. Costs are comparable only when the entire source
// is attributed to this evidenced child and all recorded tokens are priced.
func (s *Store) EconomicsLifetimes(ctx context.Context, q SessionQuery) (EconomicsLifetime, error) {
	if err := s.ensureAnalytics(ctx); err != nil {
		return EconomicsLifetime{}, err
	}
	return economicsLifetimes(ctx, s.db, q)
}

func economicsLifetimes(ctx context.Context, db economicsReader, q SessionQuery) (EconomicsLifetime, error) {
	result := EconomicsLifetime{Scope: "claude-child-usage-messages-v1", Buckets: make([]LifetimeBucket, 8)}
	for i, label := range []string{"1–2", "3–5", "6–10", "11–20", "21–40", "41–80", "81–160", "161+"} {
		result.Buckets[i].Label = label
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return result, err
	}
	rows, err := db.QueryContext(ctx, `WITH agents AS (
SELECT q.id,q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out AS recorded,q.priced_tokens,q.pricing_known,q.cost_estimate,
SUM(CASE WHEN u.agent_id=q.native_agent_id AND u.kind IN ('message-final','message-partial') THEN 1 ELSE 0 END) AS messages,
SUM(CASE WHEN u.agent_id=q.native_agent_id AND u.kind='message-without-id' THEN 1 ELSE 0 END) AS unstable,
SUM(CASE WHEN u.id IS NOT NULL AND (u.agent_id<>q.native_agent_id OR u.kind NOT IN ('message-final','message-partial')) THEN 1 ELSE 0 END) AS unsupported,
COALESCE(SUM(u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out),0) AS observed
FROM query_sessions q LEFT JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
WHERE q.provider='claude' AND q.native_agent_id<>'' AND (`+where+`) GROUP BY q.id
), classified AS (
SELECT *,CASE WHEN messages=0 THEN -1 WHEN messages<=2 THEN 0 WHEN messages<=5 THEN 1 WHEN messages<=10 THEN 2 WHEN messages<=20 THEN 3 WHEN messages<=40 THEN 4 WHEN messages<=80 THEN 5 WHEN messages<=160 THEN 6 ELSE 7 END AS bucket,
messages>0 AND unsupported=0 AND observed=recorded AND recorded>0 AND pricing_known=1 AND priced_tokens=recorded AND cost_estimate IS NOT NULL AS eligible FROM agents
)
SELECT bucket,count(*),sum(messages),sum(recorded),sum(unstable>0),sum(eligible),sum(CASE WHEN eligible THEN messages ELSE 0 END),sum(CASE WHEN eligible THEN cost_estimate END)
FROM classified GROUP BY bucket ORDER BY bucket`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var bucket LifetimeBucket
		var unstable int64
		var cost sql.NullFloat64
		if err = rows.Scan(&index, &bucket.Agents, &bucket.Messages, &bucket.RecordedTokens, &unstable, &bucket.CostEligibleAgents, &bucket.CostEligibleMessages, &cost); err != nil {
			return result, err
		}
		result.ChildSources += bucket.Agents
		result.WithUnstableMessageIdentity += unstable
		if index == -1 {
			result.WithoutMessageObservations += bucket.Agents
			continue
		}
		if index < 0 || index >= len(result.Buckets) {
			return result, ErrInvalid
		}
		bucket.Label = result.Buckets[index].Label
		if cost.Valid {
			bucket.ComparableCost = &cost.Float64
		}
		result.Buckets[index] = bucket
	}
	return result, rows.Err()
}
