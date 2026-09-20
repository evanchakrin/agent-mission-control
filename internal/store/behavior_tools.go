package store

import "context"

// Versioned, rebuildable counts only: no event body or organization state is
// duplicated. A normalization change requires a new read-model version.
func (s *Store) ensureBehaviorTools(ctx context.Context) error {
	objects := func() (int, error) {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE (type='table' AND name='query_flow_tools_v1') OR (type='trigger' AND name IN ('flow_tools_v1_insert','flow_tools_v1_update','flow_tools_v1_delete'))`).Scan(&n)
		return n, err
	}
	if n, err := objects(); err != nil {
		return err
	} else if n == 4 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if n, err := objects(); err != nil {
		return err
	} else if n == 4 {
		return nil
	}
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
	if _, err = tx.ExecContext(ctx, `
DROP TRIGGER IF EXISTS flow_tools_v1_insert;
DROP TRIGGER IF EXISTS flow_tools_v1_update;
DROP TRIGGER IF EXISTS flow_tools_v1_delete;
CREATE TABLE IF NOT EXISTS query_flow_tools_v1 (
 session_id TEXT NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 projection_revision TEXT NOT NULL, tool TEXT NOT NULL, calls INTEGER NOT NULL CHECK(calls>=0),
 PRIMARY KEY(session_id,source_id,generation,projection_revision,tool));
DELETE FROM query_flow_tools_v1;
INSERT INTO query_flow_tools_v1
 SELECT session_id,source_id,generation,projection_revision,amc_flow_tool(json_extract(data,'$.tool')),COUNT(*)
 FROM events WHERE kind='tool-call' GROUP BY session_id,source_id,generation,projection_revision,amc_flow_tool(json_extract(data,'$.tool'));
CREATE TRIGGER flow_tools_v1_insert AFTER INSERT ON events BEGIN `+behaviorToolAdd+` END;
CREATE TRIGGER flow_tools_v1_delete AFTER DELETE ON events BEGIN `+behaviorToolRemove+` END;
CREATE TRIGGER flow_tools_v1_update AFTER UPDATE ON events BEGIN `+behaviorToolRemove+behaviorToolAdd+` END;`); err != nil {
		return err
	}
	if err = checkSpace(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

const behaviorToolAdd = `INSERT INTO query_flow_tools_v1
 SELECT NEW.session_id,NEW.source_id,NEW.generation,NEW.projection_revision,amc_flow_tool(json_extract(NEW.data,'$.tool')),1 WHERE NEW.kind='tool-call'
 ON CONFLICT(session_id,source_id,generation,projection_revision,tool) DO UPDATE SET calls=calls+1;`

const behaviorToolMatch = `OLD.kind='tool-call' AND session_id=OLD.session_id AND source_id=OLD.source_id AND generation=OLD.generation AND projection_revision=OLD.projection_revision AND tool=amc_flow_tool(json_extract(OLD.data,'$.tool'))`
const behaviorToolRemove = `UPDATE query_flow_tools_v1 SET calls=calls-1 WHERE ` + behaviorToolMatch + `;
 DELETE FROM query_flow_tools_v1 WHERE calls=0 AND ` + behaviorToolMatch + `;`
