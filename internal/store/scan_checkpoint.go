package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SaveParserScan stores byte-discovery progress without claiming any additional
// indexed records or changing a published session/price/organization projection.
func (s *Store) SaveParserScan(ctx context.Context, src SourceState, checkpoint json.RawMessage) error {
	var scan struct {
		Offset int64 `json:"scanOffset"`
	}
	if len(checkpoint) > 4<<20 || json.Unmarshal(checkpoint, &scan) != nil || scan.Offset <= src.IndexedOffset || scan.Offset > src.DurableOffset {
		return ErrInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if src.ProjectionRevision != "" {
		var state string
		err = tx.QueryRowContext(ctx, `UPDATE projection_revisions SET parser_state=?,updated_at=? WHERE revision=? AND source_id=? AND generation=? AND indexed_offset=? AND CAST(parser_state AS BLOB)=? AND state IN('building','active') RETURNING state`, []byte(checkpoint), stamp(time.Now()), src.ProjectionRevision, src.Source.SourceID, src.Source.Generation, src.IndexedOffset, []byte(src.ParserState)).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if state == "building" {
			return tx.Commit()
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE sources SET parser_state=?,updated_at=? WHERE source_id=? AND generation=? AND indexed_offset=? AND CAST(parser_state AS BLOB)=? AND COALESCE((SELECT revision FROM active_projection WHERE source_id=sources.source_id AND generation=sources.generation),'')=?`, []byte(checkpoint), stamp(time.Now()), src.Source.SourceID, src.Source.Generation, src.IndexedOffset, []byte(src.ParserState), src.ProjectionRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

func (s *Store) PendingParserScan(ctx context.Context, sourceID, generation string) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(parser_state,'$.scanOffset')>indexed_offset AND json_extract(parser_state,'$.scanOffset')<durable_offset,0) FROM sources WHERE source_id=? AND generation=?`, sourceID, generation).Scan(&pending)
	return pending, err
}
