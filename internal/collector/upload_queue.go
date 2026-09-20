package collector

import "database/sql"

// This is a derived durable queue, not a retention policy. Chunk changes and
// source scheduling update membership in the same SQLite transaction.
func setupUploadQueue(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
CREATE INDEX IF NOT EXISTS pending_source_chunks ON chunks(source_id,generation,offset) WHERE ack_at IS NULL;
CREATE TABLE IF NOT EXISTS upload_sources(source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,last_attempt INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS upload_sources_order ON upload_sources(last_attempt,source_id);
CREATE TRIGGER IF NOT EXISTS upload_source_added AFTER INSERT ON chunks WHEN NEW.ack_at IS NULL BEGIN
 INSERT OR IGNORE INTO upload_sources SELECT id,last_upload_attempt FROM sources WHERE id=NEW.source_id;
END;
CREATE TRIGGER IF NOT EXISTS upload_source_changed AFTER UPDATE OF ack_at ON chunks BEGIN
 INSERT OR IGNORE INTO upload_sources SELECT id,last_upload_attempt FROM sources WHERE id=NEW.source_id AND NEW.ack_at IS NULL;
 DELETE FROM upload_sources WHERE source_id=NEW.source_id AND NOT EXISTS(SELECT 1 FROM chunks INDEXED BY pending_source_chunks WHERE source_id=NEW.source_id AND ack_at IS NULL);
END;
CREATE TRIGGER IF NOT EXISTS upload_source_removed AFTER DELETE ON chunks WHEN OLD.ack_at IS NULL BEGIN
 DELETE FROM upload_sources WHERE source_id=OLD.source_id AND NOT EXISTS(SELECT 1 FROM chunks INDEXED BY pending_source_chunks WHERE source_id=OLD.source_id AND ack_at IS NULL);
END;
CREATE TRIGGER IF NOT EXISTS upload_source_attempt AFTER UPDATE OF last_upload_attempt ON sources BEGIN
 UPDATE upload_sources SET last_attempt=NEW.last_upload_attempt WHERE source_id=NEW.id;
END;
INSERT OR IGNORE INTO upload_sources SELECT s.id,s.last_upload_attempt FROM sources s WHERE EXISTS(SELECT 1 FROM chunks INDEXED BY pending_source_chunks WHERE source_id=s.id AND ack_at IS NULL);
`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
