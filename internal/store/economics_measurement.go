package store

import (
	"context"
	"database/sql"
	"time"
)

type economicsReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// EconomicsMeasurement is a single database snapshot, not two independently
// fetched panels. A concurrent reindex or repricing cannot mix its cohorts.
// MeasuredAt records sampling time, not an immutable ledger revision or proof
// that every source has finished indexing. Unknown costs remain unknown.
type EconomicsMeasurement struct {
	Version    int               `json:"version"`
	MeasuredAt time.Time         `json:"measuredAt"`
	Costs      EconomicsCosts    `json:"costs"`
	Lifetimes  EconomicsLifetime `json:"lifetimes"`
}

func (s *Store) MeasureEconomics(ctx context.Context, q SessionQuery) (EconomicsMeasurement, error) {
	if err := s.ensureAnalytics(ctx); err != nil {
		return EconomicsMeasurement{}, err
	}
	// Bound the reader's WAL snapshot even if the owner leaves the preview open.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EconomicsMeasurement{}, err
	}
	defer tx.Rollback()
	result, err := measureEconomics(ctx, tx, q)
	if err != nil {
		return EconomicsMeasurement{}, err
	}
	if err = tx.Commit(); err != nil {
		return EconomicsMeasurement{}, err
	}
	return result, nil
}

func measureEconomics(ctx context.Context, db economicsReader, q SessionQuery) (EconomicsMeasurement, error) {
	result := EconomicsMeasurement{Version: 1, MeasuredAt: time.Now().UTC()}
	var err error
	result.Costs, err = economicsCostTotals(ctx, db, q)
	if err != nil {
		return EconomicsMeasurement{}, err
	}
	result.Lifetimes, err = economicsLifetimes(ctx, db, q)
	if err != nil {
		return EconomicsMeasurement{}, err
	}
	return result, nil
}
