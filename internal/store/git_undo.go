package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
)

const gitUndoSchema = `CREATE TABLE IF NOT EXISTS git_undo_checkpoints (
 source_id TEXT NOT NULL,generation TEXT NOT NULL,revision TEXT NOT NULL,
 indexed_offset INTEGER NOT NULL,attempts INTEGER NOT NULL CHECK(attempts>=0),
 unsupported INTEGER NOT NULL CHECK(unsupported>=0),diagnostics INTEGER NOT NULL CHECK(diagnostics>=0),
 PRIMARY KEY(source_id,generation,revision));
CREATE TABLE IF NOT EXISTS git_undo_events (
 event_seq INTEGER PRIMARY KEY REFERENCES events(seq) ON DELETE CASCADE,
 source_id TEXT NOT NULL,generation TEXT NOT NULL,revision TEXT NOT NULL,
 evidence BLOB NOT NULL,timestamp TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS git_undo_scope ON git_undo_events(source_id,generation,revision,event_seq);`

func (s *Store) setupGitUndo() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(gitUndoSchema + fileEditSchema); err != nil {
		return err
	}
	var columns int
	if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('git_undo_events') WHERE name='timestamp'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		if _, err = tx.Exec(`ALTER TABLE git_undo_events ADD COLUMN timestamp TEXT NOT NULL DEFAULT '';
UPDATE git_undo_events SET timestamp=(SELECT timestamp FROM events e WHERE e.seq=git_undo_events.event_seq);`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS git_undo_time ON git_undo_events(timestamp DESC,event_seq DESC)`); err != nil {
		return err
	}
	return tx.Commit()
}

type undoIndexEvidence struct {
	available                          bool
	attempts, unsupported, diagnostics int64
}

func loadUndoEvidence(ctx context.Context, tx *sql.Tx, b IndexBatch) (undoIndexEvidence, error) {
	var out undoIndexEvidence
	var offset int64
	err := tx.QueryRowContext(ctx, `SELECT indexed_offset,attempts,unsupported,diagnostics FROM git_undo_checkpoints WHERE source_id=? AND generation=? AND revision=?`, b.SourceID, b.Generation, b.ProjectionRevision).Scan(&offset, &out.attempts, &out.unsupported, &out.diagnostics)
	if err == sql.ErrNoRows {
		out.available = b.FromOffset == 0
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if offset != b.FromOffset {
		return out, fmt.Errorf("%w: undo evidence checkpoint", ErrConflict)
	}
	out.available = true
	return out, nil
}

func (h *undoIndexEvidence) observe(ctx context.Context, tx *sql.Tx, b IndexBatch, e Event, seq int64) error {
	if !h.available {
		return nil
	}
	if e.Kind == "indexing-error" {
		h.diagnostics++
		return nil
	}
	if e.Kind != "tool-call" {
		return nil
	}
	var data struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(validJSON(e.Data), &data); err != nil {
		return err
	}
	hit, state := hookideas.GitUndoToolCall(data.Tool, e.SearchText)
	if state == "unsupported-input" || state == "incomplete-shell" {
		h.unsupported++
		return nil
	}
	if hit == nil {
		return nil
	}
	raw, err := json.Marshal(hit)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO git_undo_events(event_seq,source_id,generation,revision,evidence,timestamp) VALUES(?,?,?,?,?,?)`, seq, b.SourceID, b.Generation, b.ProjectionRevision, raw, stamp(e.Timestamp)); err != nil {
		return err
	}
	h.attempts++
	return nil
}

func (h undoIndexEvidence) save(ctx context.Context, tx *sql.Tx, b IndexBatch) error {
	if !h.available {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO git_undo_checkpoints VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_id,generation,revision) DO UPDATE SET indexed_offset=excluded.indexed_offset,attempts=excluded.attempts,unsupported=excluded.unsupported,diagnostics=excluded.diagnostics`, b.SourceID, b.Generation, b.ProjectionRevision, b.ToOffset, h.attempts, h.unsupported, h.diagnostics)
	return err
}

func validateUndoCheckpoint(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, r ProjectionRevision) error {
	var offset, attempts, unsupported, diagnostics int64
	err := q.QueryRowContext(ctx, `SELECT indexed_offset,attempts,unsupported,diagnostics FROM git_undo_checkpoints WHERE source_id=? AND generation=? AND revision=?`, r.SourceID, r.Generation, r.Revision).Scan(&offset, &attempts, &unsupported, &diagnostics)
	if err == sql.ErrNoRows && r.IndexedOffset == 0 && r.Session.EventCount == 0 {
		return nil
	}
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: rebuilt undo evidence checkpoint missing", ErrInvalid)
	}
	if err != nil {
		return err
	}
	if offset != r.IndexedOffset || attempts < 0 || unsupported < 0 || diagnostics < 0 || attempts > r.Session.EventCount || unsupported > r.Session.EventCount-attempts || diagnostics > r.Session.EventCount-attempts-unsupported {
		return fmt.Errorf("%w: rebuilt undo evidence checkpoint inconsistent", ErrInvalid)
	}
	var rows, mismatches int64
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(e.seq IS NULL OR u.timestamp<>e.timestamp),0) FROM git_undo_events u LEFT JOIN events e ON e.seq=u.event_seq AND e.source_id=u.source_id AND e.generation=u.generation AND e.projection_revision=u.revision WHERE u.source_id=? AND u.generation=? AND u.revision=?`, r.SourceID, r.Generation, r.Revision).Scan(&rows, &mismatches); err != nil {
		return err
	}
	if rows != attempts || mismatches != 0 {
		return fmt.Errorf("%w: rebuilt undo evidence count inconsistent", ErrInvalid)
	}
	return nil
}
