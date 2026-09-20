package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

type RhythmQuery struct {
	SessionQuery
	Timezone string
}

type RhythmBucket struct {
	Index                         int      `json:"index"`
	Sessions                      int64    `json:"sessions"`
	SessionsWithErrors            int64    `json:"sessionsWithErrors"`
	SessionsWithIncompleteResults int64    `json:"sessionsWithIncompleteResults"`
	RecordedTokens                int64    `json:"recordedTokens"`
	PricedTokens                  int64    `json:"pricedTokens"`
	TierPricedTokens              int64    `json:"tierPricedTokens"`
	CostEstimate                  *float64 `json:"costEstimate"`
	ClassifiedCost                *float64 `json:"classifiedCost"`
	TopTierCost                   *float64 `json:"topTierCost"`
}

type RhythmResult struct {
	Timezone             string         `json:"timezone"`
	Basis                string         `json:"basis"`
	Sessions             int64          `json:"sessions"`
	UndatedSessions      int64          `json:"undatedSessions"`
	OutsideRangeSessions int64          `json:"outsideRangeSessions"`
	Hours                []RhythmBucket `json:"hours"`
	Weekdays             []RhythmBucket `json:"weekdays"`
}

// Stream one narrow row per session into 31 fixed buckets. No history-sized
// slice, transcript body, or browser-side fleet catalog is needed.
func (s *Store) Rhythm(ctx context.Context, q RhythmQuery) (RhythmResult, error) {
	result := RhythmResult{Timezone: q.Timezone, Basis: "earliest recorded event or usage; sessions counted once; recorded failed-tool results only; retry/stall states not inferred; selected historical rate tiers", Hours: make([]RhythmBucket, 24), Weekdays: make([]RhythmBucket, 7)}
	if q.Timezone == "" || q.Timezone == "Local" || len(q.Timezone) > 128 || q.Cursor != "" || q.Limit != 0 {
		return result, ErrInvalid
	}
	zone, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return result, ErrInvalid
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return result, ErrInvalid
	}
	filters := q.SessionQuery
	filters.From = nil
	filters.To = nil
	where, args, err := catalogWhere(filters)
	if err != nil {
		return result, err
	}
	if err = s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	if err = s.InitializeAccounting(ctx); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, rhythmSQL(where), args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for i := range result.Hours {
		result.Hours[i].Index = i
	}
	for i := range result.Weekdays {
		result.Weekdays[i].Index = i
	}
	for rows.Next() {
		var eventFirst, usageFirst sql.NullString
		var tokens, priced, tier, failures, incomplete int64
		var cost, classified, top sql.NullFloat64
		if err = rows.Scan(&eventFirst, &usageFirst, &failures, &incomplete, &tokens, &priced, &cost, &tier, &classified, &top); err != nil {
			return result, err
		}
		first := eventFirst
		if usageFirst.Valid && (!first.Valid || usageFirst.String < first.String) {
			first = usageFirst
		}
		if !first.Valid {
			result.UndatedSessions++
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, first.String)
		if e != nil || at.IsZero() {
			result.UndatedSessions++
			continue
		}
		if q.From != nil && at.Before(*q.From) || q.To != nil && !at.Before(*q.To) {
			result.OutsideRangeSessions++
			continue
		}
		if tokens < 0 || priced < 0 || priced > tokens || tier < 0 || tier > priced || failures < 0 || incomplete < 0 {
			return result, ErrInvalid
		}
		at = at.In(zone)
		result.Sessions++
		for _, bucket := range []*RhythmBucket{&result.Hours[at.Hour()], &result.Weekdays[(int(at.Weekday())+6)%7]} {
			bucket.Sessions++
			if failures > 0 {
				bucket.SessionsWithErrors++
			}
			if incomplete > 0 {
				bucket.SessionsWithIncompleteResults++
			}
			for _, part := range []struct {
				target *int64
				value  int64
			}{{&bucket.RecordedTokens, tokens}, {&bucket.PricedTokens, priced}, {&bucket.TierPricedTokens, tier}} {
				if part.value > math.MaxInt64-*part.target {
					return result, ErrInvalid
				}
				*part.target += part.value
			}
			for _, part := range []struct {
				target **float64
				value  sql.NullFloat64
			}{{&bucket.CostEstimate, cost}, {&bucket.ClassifiedCost, classified}, {&bucket.TopTierCost, top}} {
				if !part.value.Valid {
					continue
				}
				if part.value.Float64 < 0 || math.IsNaN(part.value.Float64) || math.IsInf(part.value.Float64, 0) {
					return result, ErrInvalid
				}
				if *part.target == nil {
					v := 0.0
					*part.target = &v
				}
				**part.target += part.value.Float64
				if math.IsInf(**part.target, 0) {
					return result, ErrInvalid
				}
			}
		}
	}
	if err = rows.Err(); err != nil {
		return RhythmResult{}, fmt.Errorf("rhythm query: %w", err)
	}
	return result, nil
}

func rhythmSQL(where string) string {
	return `WITH selected AS MATERIALIZED (SELECT q.* FROM query_sessions q WHERE ` + where + `),` + selectedTierCTEs + `
 measured AS (SELECT q.*,
 (SELECT MIN(a.first_at) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision AND a.first_at>'0001-01-01T00:00:00.000000000Z') event_first,
 (SELECT MIN(u.timestamp) FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.timestamp>'0001-01-01T00:00:00.000000000Z') usage_first,
 (SELECT COALESCE(SUM(a.errors),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) failures,
 (SELECT COALESCE(SUM(a.unknown_results+a.indexing_errors),0) FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision) incomplete
 FROM selected q)
 SELECT q.event_first,q.usage_first,q.failures,q.incomplete,q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out,q.priced_tokens,q.cost_estimate,COALESCE(t.tier_tokens,0),t.known_cost,t.top_cost
 FROM measured q LEFT JOIN tiers t ON t.session_id=q.id`
}
