package store

import "context"

// Keep the existing lookup order and cover source guards and activity bounds.
// This avoids fetching every usage row to count scopes or find first activity.
// Run at startup, not on each calendar request; old query results stay valid.
func (s *Store) ensureUsageScopeIndex(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `PRAGMA index_info(query_usage_session_agent)`)
	if err != nil {
		return err
	}
	covered, timestamp := false, false
	for rows.Next() {
		var sequence, column int
		var name string
		if err = rows.Scan(&sequence, &column, &name); err != nil {
			rows.Close()
			return err
		}
		if name == "source_id" {
			covered = true
		}
		if name == "timestamp" {
			timestamp = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if covered && timestamp {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DROP INDEX IF EXISTS query_usage_session_agent;
 CREATE INDEX query_usage_session_agent ON query_usage(session_id,generation,projection_revision,agent_id,model,id,source_id,timestamp);`); err != nil {
		return err
	}
	return tx.Commit()
}
