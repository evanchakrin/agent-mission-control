package store

import "context"

const delegatedCallPredicate = `kind='tool-call' AND json_extract(data,'$.tool') IN ('Task','Agent','spawn_agent','functions.spawn_agent','collaboration.spawn_agent')`

func (s *Store) ensureDelegatedIndex(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='events_delegated_page'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.capacityMu.Lock()
	err := s.capacity(128 << 20)
	s.capacityMu.Unlock()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS events_delegated_page ON events(seq) WHERE `+delegatedCallPredicate); err != nil {
		return err
	}
	s.capacityMu.Lock()
	err = s.capacity(128 << 20)
	s.capacityMu.Unlock()
	if err != nil {
		return err
	}
	return tx.Commit()
}
