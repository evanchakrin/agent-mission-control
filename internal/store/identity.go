package store

import (
	"context"
	"database/sql"
)

// HubID survives restore. RecoveryEpoch does not: the two identities serve
// different purposes for pending browser intent and collector reconciliation.
func (s *Store) HubID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key='hub_id'`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO properties VALUES('hub_id',?)`, randomID()); err != nil {
		return "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT value FROM properties WHERE key='hub_id'`).Scan(&id)
	return id, err
}
