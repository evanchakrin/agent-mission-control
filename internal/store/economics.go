package store

import (
	"context"
	"database/sql"
)

func setupEconomicsColumns(ctx context.Context, tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('query_sessions') WHERE name='component_known'").Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE query_sessions ADD COLUMN component_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE query_sessions ADD COLUMN component_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE query_sessions ADD COLUMN cost_input REAL NOT NULL DEFAULT 0;
ALTER TABLE query_sessions ADD COLUMN cost_cache_read REAL NOT NULL DEFAULT 0;
ALTER TABLE query_sessions ADD COLUMN cost_cache_write REAL NOT NULL DEFAULT 0;
ALTER TABLE query_sessions ADD COLUMN cost_output REAL NOT NULL DEFAULT 0;
UPDATE query_sessions SET (component_known,component_tokens,cost_input,cost_cache_read,cost_cache_write,cost_output)=(SELECT json_extract(projection,'$.pricing.components') IS NOT NULL,COALESCE(json_extract(projection,'$.pricing.components.pricedTokens'),0),COALESCE(json_extract(projection,'$.pricing.components.input'),0),COALESCE(json_extract(projection,'$.pricing.components.cacheRead'),0),COALESCE(json_extract(projection,'$.pricing.components.cacheWrite'),0),COALESCE(json_extract(projection,'$.pricing.components.output'),0) FROM sessions WHERE sessions.id=query_sessions.id);`)
	return err
}

// EconomicsCosts aggregates published estimates only. Missing historical
// breakdowns remain separately counted instead of being allocated by token share.
type EconomicsCosts struct {
	Sessions                     int64          `json:"sessions"`
	RecordedTokens               int64          `json:"recordedTokens"`
	PricedTokens                 int64          `json:"pricedTokens"`
	UnpricedOrUnmeasuredTokens   int64          `json:"unpricedOrUnmeasuredTokens"`
	PricedTokensWithoutBreakdown int64          `json:"pricedTokensWithoutBreakdown"`
	SessionsWithBreakdown        int64          `json:"sessionsWithBreakdown"`
	SessionsWithoutBreakdown     int64          `json:"sessionsWithoutBreakdown"`
	KnownCost                    *float64       `json:"knownCost"`
	Components                   CostComponents `json:"components"`
}

func (s *Store) EconomicsCostTotals(ctx context.Context, q SessionQuery) (EconomicsCosts, error) {
	var result EconomicsCosts
	if err := s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	return economicsCostTotals(ctx, s.db, q)
}

func economicsCostTotals(ctx context.Context, db economicsReader, q SessionQuery) (EconomicsCosts, error) {
	var result EconomicsCosts
	where, args, err := catalogWhere(q)
	if err != nil {
		return result, err
	}
	var cost sql.NullFloat64
	err = db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out),0),COALESCE(sum(q.priced_tokens),0),sum(q.cost_estimate),
COALESCE(sum(q.component_known),0),COALESCE(sum(q.component_tokens),0),COALESCE(sum(q.cost_input),0),COALESCE(sum(q.cost_cache_read),0),COALESCE(sum(q.cost_cache_write),0),COALESCE(sum(q.cost_output),0)
FROM query_sessions q WHERE `+where, args...).Scan(&result.Sessions, &result.RecordedTokens, &result.PricedTokens, &cost, &result.SessionsWithBreakdown, &result.Components.PricedTokens, &result.Components.Input, &result.Components.CacheRead, &result.Components.CacheWrite, &result.Components.Output)
	if err != nil {
		return result, err
	}
	if cost.Valid {
		result.KnownCost = &cost.Float64
	}
	result.SessionsWithoutBreakdown = result.Sessions - result.SessionsWithBreakdown
	result.PricedTokensWithoutBreakdown = result.PricedTokens - result.Components.PricedTokens
	result.UnpricedOrUnmeasuredTokens = result.RecordedTokens - result.PricedTokens
	if result.PricedTokensWithoutBreakdown < 0 || result.UnpricedOrUnmeasuredTokens < 0 {
		return EconomicsCosts{}, ErrInvalid
	}
	return result, nil
}
