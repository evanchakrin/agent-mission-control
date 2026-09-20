package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"time"
)

// SetupIndexing creates a durable ready queue. Caught-up sources and partial
// records are not scanned on each idle poll. New durable bytes wake their job.
func (s *Store) SetupIndexing(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS index_work(
 source_id TEXT NOT NULL,generation TEXT NOT NULL,observed_offset INTEGER NOT NULL,
 ready_at TEXT NOT NULL,state TEXT NOT NULL,error TEXT NOT NULL,PRIMARY KEY(source_id,generation));
CREATE INDEX IF NOT EXISTS index_work_ready ON index_work(ready_at,source_id,generation);
CREATE INDEX IF NOT EXISTS index_work_catalog ON index_work(source_id,generation,ready_at);
CREATE INDEX IF NOT EXISTS index_work_failed ON index_work(ready_at,source_id,generation) WHERE state='blocked';
CREATE TRIGGER IF NOT EXISTS index_source_insert AFTER INSERT ON sources
 WHEN NEW.indexed_offset<NEW.durable_offset AND NOT EXISTS(SELECT 1 FROM properties WHERE key='external_index:'||NEW.source_id AND value='true')
 BEGIN INSERT OR REPLACE INTO index_work VALUES(NEW.source_id,NEW.generation,NEW.durable_offset,'','ready',''); END;
CREATE TRIGGER IF NOT EXISTS index_source_capture AFTER UPDATE OF durable_offset ON sources
 WHEN NEW.durable_offset>OLD.durable_offset AND NEW.indexed_offset<NEW.durable_offset AND NOT EXISTS(SELECT 1 FROM properties WHERE key='external_index:'||NEW.source_id AND value='true')
 BEGIN INSERT OR REPLACE INTO index_work VALUES(NEW.source_id,NEW.generation,NEW.durable_offset,'','ready',''); END;
CREATE TRIGGER IF NOT EXISTS index_source_progress AFTER UPDATE OF indexed_offset ON sources
 WHEN NEW.indexed_offset>OLD.indexed_offset
 BEGIN
 DELETE FROM index_work WHERE source_id=NEW.source_id AND generation=NEW.generation AND NEW.indexed_offset>=NEW.durable_offset;
 INSERT INTO properties VALUES('last_index_progress',strftime('%Y-%m-%dT%H:%M:%fZ','now')) ON CONFLICT(key) DO UPDATE SET value=excluded.value;
 END;
CREATE TRIGGER IF NOT EXISTS index_source_retry_progress AFTER UPDATE OF indexed_offset ON sources
 WHEN NEW.indexed_offset>OLD.indexed_offset AND NEW.indexed_offset<NEW.durable_offset
 BEGIN
 UPDATE index_work SET ready_at='',state='ready',error='' WHERE source_id=NEW.source_id AND generation=NEW.generation;
 END;
CREATE TRIGGER IF NOT EXISTS index_external_insert AFTER INSERT ON properties
 WHEN NEW.value='true' AND substr(NEW.key,1,15)='external_index:'
 BEGIN DELETE FROM index_work WHERE 'external_index:'||source_id=NEW.key; END;
CREATE TRIGGER IF NOT EXISTS index_external_update AFTER UPDATE ON properties
 WHEN NEW.value='true' AND substr(NEW.key,1,15)='external_index:'
 BEGIN DELETE FROM index_work WHERE 'external_index:'||source_id=NEW.key; END;
INSERT OR IGNORE INTO index_work SELECT source_id,generation,durable_offset,'','ready','' FROM sources s
 WHERE indexed_offset<durable_offset AND NOT EXISTS(SELECT 1 FROM properties WHERE key='external_index:'||s.source_id AND value='true');`)
	return err
}

const pendingSourcesSQL = `SELECT s.meta_json,s.durable_offset,s.indexed_offset,'{}'
 FROM index_work w INDEXED BY index_work_catalog JOIN sources s USING(source_id,generation)
 WHERE w.ready_at<=? AND (w.source_id,w.generation)>(?,?)
 AND NOT EXISTS(SELECT 1 FROM projection_revisions r WHERE r.source_id=w.source_id AND r.generation=w.generation AND r.state IN('building','verifying','ready'))
 ORDER BY w.source_id,w.generation LIMIT ?`

func (s *Store) PendingSources(ctx context.Context, after string, limit int) ([]SourceState, error) {
	id, generation := "", ""
	if after != "" {
		b, err := base64.RawURLEncoding.DecodeString(after)
		var c []string
		if err != nil || json.Unmarshal(b, &c) != nil || len(c) != 2 {
			return nil, ErrInvalid
		}
		id, generation = c[0], c[1]
	}
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.maintenanceMu.Unlock()
	rows, err := s.db.QueryContext(ctx, pendingSourcesSQL, stamp(time.Now()), id, generation, pageLimit(limit))
	return scanPendingSources(rows, err)
}

// RetrySources bypasses the catalog cursor only for due failures. A partial
// index bounds this periodic lookup to failed jobs, not all historical sources.
func (s *Store) RetrySources(ctx context.Context, limit int) ([]SourceState, error) {
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.maintenanceMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT s.meta_json,s.durable_offset,s.indexed_offset,'{}'
 FROM index_work w INDEXED BY index_work_failed JOIN sources s USING(source_id,generation)
 WHERE w.state='blocked' AND w.ready_at<=?
 AND NOT EXISTS(SELECT 1 FROM projection_revisions r WHERE r.source_id=w.source_id AND r.generation=w.generation AND r.state IN('building','verifying','ready'))
 ORDER BY w.ready_at,w.source_id,w.generation LIMIT ?`, stamp(time.Now()), pageLimit(limit))
	return scanPendingSources(rows, err)
}

func scanPendingSources(rows *sql.Rows, err error) ([]SourceState, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceState{}
	for rows.Next() {
		var state SourceState
		var meta, checkpoint []byte
		if err = rows.Scan(&meta, &state.DurableOffset, &state.IndexedOffset, &checkpoint); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(meta, &state.Source); err != nil {
			return nil, err
		}
		state.ParserState = json.RawMessage(checkpoint)
		out = append(out, state)
	}
	return out, rows.Err()
}
func (s *Store) RecordIndexAttempt(ctx context.Context, sourceID, generation string, observedOffset int64, progress bool, problem error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if progress && problem == nil {
		// A successful retry may advance only part of a large source. Clear
		// the previous failure now, not only when the whole source finishes.
		// Completed work stays removed by its checkpoint trigger; a newer
		// upload/attempt must not be overwritten by this dispatch's result.
		_, err := s.db.ExecContext(ctx, `UPDATE index_work SET ready_at='',state='ready',error='' WHERE source_id=? AND generation=? AND observed_offset=? AND state<>'ready'`, sourceID, generation, observedOffset)
		return err
	}
	state, message, retry := "blocked", "", time.Now().Add(5*time.Minute)
	if problem == nil {
		state, message, retry = "awaiting-record", "Awaiting complete record", time.Now().AddDate(100, 0, 0)
	} else {
		message = problem.Error()
	}
	if len(message) > 2048 {
		message = message[:2048]
	}
	// A concurrent upload must not have its wakeup overwritten by an old result.
	_, err := s.db.ExecContext(ctx, `UPDATE index_work SET ready_at=?,state=?,error=? WHERE source_id=? AND generation=? AND observed_offset=?`, stamp(retry), state, message, sourceID, generation, observedOffset)
	return err
}

type IndexingProgress struct {
	LastProgress   time.Time `json:"lastProgress"`
	Ready          int64     `json:"readySources"`
	AwaitingRecord int64     `json:"awaitingRecordSources"`
	Blocked        int64     `json:"blockedSources"`
	Rebuilding     int64     `json:"rebuildingSources"`
	Problem        string    `json:"problem,omitempty"`
}

func (s *Store) IndexingProgress(ctx context.Context) (IndexingProgress, error) {
	var p IndexingProgress
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return p, err
	}
	defer s.maintenanceMu.Unlock()
	var at string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM properties WHERE key='last_index_progress'),'')`).Scan(&at); err != nil {
		return p, err
	}
	if at != "" {
		var err error
		p.LastProgress, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return p, err
		}
	}
	// Keep the durable normal queue intact, but report work owned by a staged
	// rebuild separately. Its actual failures are exposed by RebuildProgress.
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(state='ready' AND NOT rebuilding),0),COALESCE(SUM(state='awaiting-record' AND NOT rebuilding),0),COALESCE(SUM(state='blocked' AND NOT rebuilding),0),COALESCE(MAX(CASE WHEN state='blocked' AND NOT rebuilding THEN error ELSE '' END),''),COALESCE(SUM(rebuilding),0)
 FROM (SELECT w.*,EXISTS(SELECT 1 FROM projection_revisions r WHERE r.source_id=w.source_id AND r.generation=w.generation AND r.state IN('building','verifying','ready')) AS rebuilding FROM index_work w)`).Scan(&p.Ready, &p.AwaitingRecord, &p.Blocked, &p.Problem, &p.Rebuilding)
	return p, err
}
