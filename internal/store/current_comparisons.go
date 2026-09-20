package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// SessionResponse is a read-only API view, never a persisted parser projection.
type SessionResponse struct {
	Session
	APIComparison json.RawMessage `json:"apiComparison,omitempty"`
}

// SessionResponses enriches one bounded page without per-session queries. Match
// both the supplied projection and current source checkpoint to reject stale data.
func (s *Store) SessionResponses(ctx context.Context, sessions []Session) ([]SessionResponse, error) {
	result := make([]SessionResponse, len(sessions))
	if len(sessions) == 0 {
		return result, nil
	}
	if len(sessions) > 500 {
		return nil, ErrInvalid
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return nil, err
	}
	values := make([]string, len(sessions))
	args := make([]any, 0, len(sessions)*4)
	for i, row := range sessions {
		result[i].Session = row
		values[i] = "(?,?,?,?)"
		args = append(args, i, row.ID, row.Generation, row.ProjectionRevision)
	}
	rows, err := s.db.QueryContext(ctx, `WITH requested(position,id,generation,revision) AS (VALUES `+strings.Join(values, ",")+`)
 SELECT p.position,e.estimate FROM requested p
 JOIN sessions q ON q.id=p.id AND q.generation=p.generation AND json_extract(q.projection,'$.projectionRevision')=p.revision
 JOIN sources r ON r.source_id=q.source_id AND r.generation=q.generation
 JOIN current_comparisons c ON c.session_id=q.id AND c.generation=q.generation AND c.projection_revision=p.revision AND c.indexed_offset=r.indexed_offset
 JOIN accounting_estimates e ON e.id=c.snapshot_id AND e.session_id=q.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var raw []byte
		if err := rows.Scan(&position, &raw); err != nil {
			return nil, err
		}
		result[position].APIComparison = json.RawMessage(raw)
	}
	return result, rows.Err()
}

// Comparison totals are separate from historical estimates. Selected snapshots
// may use different explicit catalog dates and billing assumptions.
type ComparisonTotals struct {
	Sessions               int64    `json:"sessions"`
	SessionsWithComparison int64    `json:"sessionsWithComparison"`
	RecordedTokens         int64    `json:"recordedTokens"`
	PricedTokens           int64    `json:"pricedTokens"`
	UnpricedTokens         int64    `json:"unpricedTokens"`
	UnattributedTokens     int64    `json:"unattributedTokens"`
	CostEstimate           *float64 `json:"costEstimate"`
	Basis                  string   `json:"basis"`
}

func (s *Store) comparisonTotals(ctx context.Context, from, where string, args []any) (ComparisonTotals, error) {
	result := ComparisonTotals{Basis: "selected-api-rate-comparisons-not-billing; per-session catalog dates and contexts may differ"}
	if err := s.InitializeAccounting(ctx); err != nil {
		return result, err
	}
	var cost sql.NullFloat64
	// Offset and projection checks exclude an estimate immediately when indexing
	// advances or source generations change, even before its worker runs again.
	err := s.db.QueryRowContext(ctx, `SELECT count(*),count(c.session_id),COALESCE(sum(q.tokens_in+q.tokens_cache+q.tokens_write+q.tokens_out),0),COALESCE(sum(c.priced_tokens),0),COALESCE(sum(c.unattributed_tokens),0),sum(CASE WHEN c.priced_tokens>0 THEN c.cost END)
 FROM `+from+` LEFT JOIN sources r ON r.source_id=q.source_id AND r.generation=q.generation
 LEFT JOIN current_comparisons c ON c.session_id=q.id AND c.generation=q.generation AND c.projection_revision=q.projection_revision AND c.indexed_offset=r.indexed_offset
 WHERE `+where, args...).Scan(&result.Sessions, &result.SessionsWithComparison, &result.RecordedTokens, &result.PricedTokens, &result.UnattributedTokens, &cost)
	if err != nil {
		return result, err
	}
	if result.PricedTokens < 0 || result.PricedTokens > result.RecordedTokens {
		return result, ErrInvalid
	}
	result.UnpricedTokens = result.RecordedTokens - result.PricedTokens
	if cost.Valid {
		result.CostEstimate = &cost.Float64
	}
	return result, nil
}
