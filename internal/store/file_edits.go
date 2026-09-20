package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const fileEditSchema = `CREATE TABLE IF NOT EXISTS file_edit_checkpoints (
 source_id TEXT NOT NULL,generation TEXT NOT NULL,revision TEXT NOT NULL,
 indexed_offset INTEGER NOT NULL,attempts INTEGER NOT NULL,diagnostics INTEGER NOT NULL,
 PRIMARY KEY(source_id,generation,revision));
CREATE TABLE IF NOT EXISTS file_edit_events (
 event_seq INTEGER PRIMARY KEY REFERENCES events(seq) ON DELETE CASCADE,
 source_id TEXT NOT NULL,generation TEXT NOT NULL,revision TEXT NOT NULL,path TEXT NOT NULL,tool TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS file_edit_scope ON file_edit_events(source_id,generation,revision,event_seq);`

type fileEditIndex struct {
	available             bool
	attempts, diagnostics int64
}

func loadFileEdits(ctx context.Context, tx *sql.Tx, b IndexBatch) (fileEditIndex, error) {
	var out fileEditIndex
	var offset int64
	err := tx.QueryRowContext(ctx, `SELECT indexed_offset,attempts,diagnostics FROM file_edit_checkpoints WHERE source_id=? AND generation=? AND revision=?`, b.SourceID, b.Generation, b.ProjectionRevision).Scan(&offset, &out.attempts, &out.diagnostics)
	if err == sql.ErrNoRows {
		out.available = b.FromOffset == 0
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if offset != b.FromOffset {
		return out, fmt.Errorf("%w: file edit checkpoint", ErrConflict)
	}
	out.available = true
	return out, nil
}

func (h *fileEditIndex) observe(ctx context.Context, tx *sql.Tx, b IndexBatch, e Event, seq int64) error {
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
	switch data.Tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
	default:
		return nil
	}
	var input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if e.SearchText == "" || len(e.SearchText) > 8<<20 || json.Unmarshal([]byte(e.SearchText), &input) != nil {
		h.diagnostics++
		return nil
	}
	path := input.FilePath
	if data.Tool == "NotebookEdit" {
		path = input.NotebookPath
	}
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
		h.diagnostics++
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO file_edit_events VALUES(?,?,?,?,?,?)`, seq, b.SourceID, b.Generation, b.ProjectionRevision, path, data.Tool)
	if err == nil {
		h.attempts++
	}
	return err
}

func (h fileEditIndex) save(ctx context.Context, tx *sql.Tx, b IndexBatch) error {
	if !h.available {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO file_edit_checkpoints VALUES(?,?,?,?,?,?) ON CONFLICT(source_id,generation,revision) DO UPDATE SET indexed_offset=excluded.indexed_offset,attempts=excluded.attempts,diagnostics=excluded.diagnostics`, b.SourceID, b.Generation, b.ProjectionRevision, b.ToOffset, h.attempts, h.diagnostics)
	return err
}

func validateFileEditCheckpoint(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, r ProjectionRevision) error {
	var offset, attempts, diagnostics int64
	err := q.QueryRowContext(ctx, `SELECT indexed_offset,attempts,diagnostics FROM file_edit_checkpoints WHERE source_id=? AND generation=? AND revision=?`, r.SourceID, r.Generation, r.Revision).Scan(&offset, &attempts, &diagnostics)
	// Preserve pre-v6 history and backups; an absent older projection is shown as
	// needing rebuilding, never as an empty list of prior edits.
	version, _ := strconv.Atoi(r.ParserVersion)
	if err == sql.ErrNoRows && (version < 6 || r.IndexedOffset == 0 && r.Session.EventCount == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: file edit checkpoint: %v", ErrInvalid, err)
	}
	var count int64
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_edit_events f JOIN events e ON e.seq=f.event_seq AND e.source_id=f.source_id AND e.generation=f.generation AND e.projection_revision=f.revision WHERE f.source_id=? AND f.generation=? AND f.revision=?`, r.SourceID, r.Generation, r.Revision).Scan(&count); err != nil {
		return err
	}
	if offset != r.IndexedOffset || attempts < 0 || diagnostics < 0 || attempts > r.Session.EventCount || diagnostics > r.Session.EventCount-attempts || count != attempts {
		return fmt.Errorf("%w: inconsistent file edit checkpoint", ErrInvalid)
	}
	return nil
}

type PriorEdit struct {
	Sequence int64  `json:"sequence"`
	Path     string `json:"path"`
	Tool     string `json:"tool"`
}
type PriorEditPage struct {
	State        string      `json:"state"`
	Scope        string      `json:"scope"`
	Snapshot     string      `json:"snapshot"`
	UndoSequence int64       `json:"undoSequence"`
	Diagnostics  int64       `json:"diagnostics"`
	Edits        []PriorEdit `json:"edits"`
	NextBefore   int64       `json:"nextBefore,omitempty"`
}

// PriorUndoEdits returns earlier recorded write attempts within the same source
// interpretation, not files proven affected by Git. Repeated paths retain their
// separate evidence links. No command execution, path resolution or disk scan.
func (s *Store) PriorUndoEdits(ctx context.Context, id, snapshot string, undo, before int64, limit int) (PriorEditPage, error) {
	out := PriorEditPage{State: "needs-rebuild", Scope: "Edit-Write-MultiEdit-NotebookEdit-in-this-session", UndoSequence: undo, Edits: []PriorEdit{}}
	if undo < 1 || before < 0 || before > undo || limit < 0 || limit > 100 || snapshot == "" {
		return out, ErrInvalid
	}
	if limit == 0 {
		limit = 25
	}
	if before == 0 {
		before = undo
	}
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return out, err
	}
	out.Snapshot = token
	var source, generation, revision string
	err = s.db.QueryRowContext(ctx, `SELECT s.source_id,s.generation,u.revision FROM sessions s JOIN git_undo_events u ON u.source_id=s.source_id AND u.generation=s.generation AND u.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'') WHERE s.id=? AND u.event_seq=?`, id, undo).Scan(&source, &generation, &revision)
	if err == sql.ErrNoRows {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var indexed, editOffset int64
	err = s.db.QueryRowContext(ctx, `SELECT c.indexed_offset,c.diagnostics,r.indexed_offset FROM file_edit_checkpoints c JOIN sources r ON r.source_id=c.source_id AND r.generation=c.generation WHERE c.source_id=? AND c.generation=? AND c.revision=?`, source, generation, revision).Scan(&editOffset, &out.Diagnostics, &indexed)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil && editOffset == indexed {
		out.State = "indexed-tool-calls"
		if out.Diagnostics > 0 {
			out.State = "incomplete-indexing"
		}
		rows, err := s.db.QueryContext(ctx, `SELECT event_seq,path,tool FROM file_edit_events WHERE source_id=? AND generation=? AND revision=? AND event_seq<? ORDER BY event_seq DESC LIMIT ?`, source, generation, revision, before, limit+1)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var item PriorEdit
			if err = rows.Scan(&item.Sequence, &item.Path, &item.Tool); err != nil {
				rows.Close()
				return PriorEditPage{}, err
			}
			if len(out.Edits) == limit {
				out.NextBefore = out.Edits[len(out.Edits)-1].Sequence
				break
			}
			out.Edits = append(out.Edits, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return PriorEditPage{}, err
		}
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return PriorEditPage{}, err
	}
	return out, nil
}
