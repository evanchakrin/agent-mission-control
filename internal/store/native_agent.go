package store

import (
	"context"
	"database/sql"
)

func setupNativeAgentColumn(ctx context.Context, tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('query_sessions') WHERE name='native_agent_id'").Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE query_sessions ADD COLUMN native_agent_id TEXT NOT NULL DEFAULT '';
UPDATE query_sessions SET native_agent_id=COALESCE((SELECT json_extract(projection,'$.nativeAgentId') FROM sessions WHERE sessions.id=query_sessions.id),'');`)
	return err
}
