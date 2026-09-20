package store

import (
	"context"
	"database/sql"
	"math"
)

type ContributionTokenTotals struct {
	TokensIn         int64 `json:"tokensIn"`
	TokensCache      int64 `json:"tokensCache"`
	TokensCacheWrite int64 `json:"tokensCacheWrite"`
	TokensOut        int64 `json:"tokensOut"`
	Total            int64 `json:"total"`
}

type UsageContributionTotals struct {
	Scope            string                  `json:"scope"`
	Sessions         int64                   `json:"sessions"`
	Recorded         ContributionTokenTotals `json:"recorded"`
	Excluded         ContributionTokenTotals `json:"excluded"`
	Counted          ContributionTokenTotals `json:"counted"`
	ActiveExclusions int64                   `json:"activeExclusions"`
	StaleSelections  int64                   `json:"staleSelections"`
}

func (t *ContributionTokenTotals) sum() error {
	t.Total = 0
	for _, n := range []int64{t.TokensIn, t.TokensCache, t.TokensCacheWrite, t.TokensOut} {
		if n < 0 || n > math.MaxInt64-t.Total {
			return ErrInvalid
		}
		t.Total += n
	}
	return nil
}

// ContributionTotals applies only current explicit duplicate decisions. This is
// not a certification that all repeated/inherited usage has been discovered.
// It does not reprice historical estimates or mutate the recorded catalog.
func (s *Store) ContributionTotals(ctx context.Context, q SessionQuery) (UsageContributionTotals, error) {
	result := UsageContributionTotals{Scope: "verified-usage-exclusions-only"}
	if err := s.ensureAnalytics(ctx); err != nil {
		return result, err
	}
	where, args, err := catalogWhere(q)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(q.tokens_in),0),COALESCE(SUM(q.tokens_cache),0),COALESCE(SUM(q.tokens_write),0),COALESCE(SUM(q.tokens_out),0) FROM query_sessions q WHERE `+where, args...).Scan(&result.Sessions, &result.Recorded.TokensIn, &result.Recorded.TokensCache, &result.Recorded.TokensCacheWrite, &result.Recorded.TokensOut)
	if err != nil {
		return result, err
	}
	if err = result.Recorded.sum(); err != nil {
		return result, err
	}
	result.Counted = result.Recorded
	var ready int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='usage_contribution_owners'`).Scan(&ready); err != nil {
		return result, err
	}
	if ready == 0 {
		return result, nil
	}
	after := ""
	for {
		pageArgs := append(append([]any{}, args...), after)
		rows, err := tx.QueryContext(ctx, `SELECT a.excluded_id,a.proof_id FROM usage_contribution_owners a JOIN query_usage u ON u.id=a.excluded_id JOIN query_sessions q ON q.id=u.session_id AND q.source_id=u.source_id AND q.generation=u.generation AND q.projection_revision=u.projection_revision WHERE (`+where+`) AND a.excluded_id>? ORDER BY a.excluded_id LIMIT 100`, pageArgs...)
		if err != nil {
			return UsageContributionTotals{}, err
		}
		type entry struct{ id, proof string }
		batch := []entry{}
		for rows.Next() {
			var e entry
			if err = rows.Scan(&e.id, &e.proof); err != nil {
				rows.Close()
				return UsageContributionTotals{}, err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return UsageContributionTotals{}, err
		}
		for _, e := range batch {
			after = e.id
			_, _, usage, err := loadCurrentContributionProof(ctx, tx, e.proof, s.epoch)
			if err == ErrConflict {
				result.StaleSelections++
				continue
			}
			if err != nil {
				return UsageContributionTotals{}, err
			}
			u, ok := excludedProofObservation(usage, e.id)
			if !ok {
				return UsageContributionTotals{}, ErrConflict
			}
			for _, bucket := range []struct {
				total *int64
				value int64
			}{{&result.Excluded.TokensIn, u.TokensIn}, {&result.Excluded.TokensCache, u.TokensCache}, {&result.Excluded.TokensCacheWrite, u.TokensCacheWrite}, {&result.Excluded.TokensOut, u.TokensOut}} {
				if bucket.value < 0 || bucket.value > math.MaxInt64-*bucket.total {
					return UsageContributionTotals{}, ErrInvalid
				}
				*bucket.total += bucket.value
			}
			result.ActiveExclusions++
		}
		if len(batch) < 100 {
			break
		}
	}
	if err = result.Excluded.sum(); err != nil {
		return UsageContributionTotals{}, err
	}
	result.Counted = ContributionTokenTotals{TokensIn: result.Recorded.TokensIn - result.Excluded.TokensIn, TokensCache: result.Recorded.TokensCache - result.Excluded.TokensCache, TokensCacheWrite: result.Recorded.TokensCacheWrite - result.Excluded.TokensCacheWrite, TokensOut: result.Recorded.TokensOut - result.Excluded.TokensOut}
	if err = result.Counted.sum(); err != nil {
		return UsageContributionTotals{}, err
	}
	return result, nil
}
