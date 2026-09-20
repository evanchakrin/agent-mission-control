package store

import "context"

// The agent/model index cannot seek the earliest timestamp for a whole session.
// Build this once during startup, never in a browser request. This is a derived
// index only: source evidence, observations and organization remain unchanged.
func (s *Store) ensureUsageTimeIndex(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='query_usage_scope_time'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// This is a minimum safety margin, not an estimate of total build space.
	// Keep schema publication transactional if the reserve is crossed.
	checkSpace := func() error {
		s.capacityMu.Lock()
		defer s.capacityMu.Unlock()
		return s.capacity(128 << 20)
	}
	if err := checkSpace(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS query_usage_scope_time
 ON query_usage(session_id,source_id,generation,projection_revision,timestamp)`)
	if err != nil {
		return err
	}
	if err := checkSpace(); err != nil {
		return err
	}
	return tx.Commit()
}
