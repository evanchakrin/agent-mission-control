package store

import (
	"context"
	"database/sql"
	"time"
)

// This default applies only to newly inserted sessions. Setting or clearing it
// never changes existing policies, including sessions explicitly disabled.
type ComparisonDefault struct {
	CatalogID    string    `json:"catalogId"`
	Context      string    `json:"context"`
	ComparisonAt time.Time `json:"comparisonAt"`
}

func (s *Store) ComparisonDefault(ctx context.Context) (ComparisonDefault, error) {
	var p ComparisonDefault
	if err := s.InitializePricingQueue(ctx); err != nil {
		return p, err
	}
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT catalog_id,billing_context,comparison_at FROM pricing_comparison_default WHERE id=1`).Scan(&p.CatalogID, &p.Context, &at)
	if err == sql.ErrNoRows {
		return p, ErrNotFound
	}
	if err == nil {
		p.ComparisonAt, err = time.Parse(time.RFC3339Nano, at)
	}
	return p, err
}

func (s *Store) SetComparisonDefault(ctx context.Context, p *ComparisonDefault) error {
	if p != nil && (!validID(p.CatalogID) || p.Context == "" || len(p.Context) > 1024 || p.ComparisonAt.IsZero()) {
		return ErrInvalid
	}
	if err := s.InitializePricingQueue(ctx); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM pricing_comparison_default WHERE id=1`)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO pricing_comparison_default VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET catalog_id=excluded.catalog_id,billing_context=excluded.billing_context,comparison_at=excluded.comparison_at`, p.CatalogID, p.Context, p.ComparisonAt.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('pricing-default','',?)`, stamp(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}
