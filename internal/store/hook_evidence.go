package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/evanchakrin/agent-mission-control/internal/hookideas"
)

const hookEvidenceSchema = `CREATE TABLE IF NOT EXISTS hook_javascript_evidence (
 source_id TEXT NOT NULL, generation TEXT NOT NULL, revision TEXT NOT NULL,
 indexed_offset INTEGER NOT NULL, edited INTEGER NOT NULL CHECK(edited IN(0,1)),
 errors INTEGER NOT NULL CHECK(errors>=0), diagnostics INTEGER NOT NULL CHECK(diagnostics>=0),
 PRIMARY KEY(source_id,generation,revision));`

type hookIndexEvidence struct {
	available           bool
	session             hookideas.JavaScriptSession
	errors, diagnostics int64
}

func validateHookCheckpoint(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, r ProjectionRevision) error {
	if err := validateFileEditCheckpoint(ctx, q, r); err != nil {
		return err
	}
	if err := validateUndoCheckpoint(ctx, q, r); err != nil {
		return err
	}
	var offset, count, diagnostics int64
	var edited bool
	err := q.QueryRowContext(ctx, `SELECT indexed_offset,edited,errors,diagnostics FROM hook_javascript_evidence WHERE source_id=? AND generation=? AND revision=?`, r.SourceID, r.Generation, r.Revision).Scan(&offset, &edited, &count, &diagnostics)
	if err == sql.ErrNoRows && r.IndexedOffset == 0 && r.Session.EventCount == 0 {
		return nil
	}
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: rebuilt hook evidence checkpoint missing", ErrInvalid)
	}
	if err != nil {
		return err
	}
	if offset != r.IndexedOffset || count < 0 || diagnostics < 0 || count > r.Session.EventCount || diagnostics > r.Session.EventCount || count > 0 && !edited {
		return fmt.Errorf("%w: rebuilt hook evidence checkpoint inconsistent", ErrInvalid)
	}
	return nil
}

func loadHookEvidence(ctx context.Context, tx *sql.Tx, b IndexBatch) (hookIndexEvidence, error) {
	var h hookIndexEvidence
	var offset int64
	var edited bool
	err := tx.QueryRowContext(ctx, `SELECT indexed_offset,edited,errors,diagnostics FROM hook_javascript_evidence WHERE source_id=? AND generation=? AND revision=?`, b.SourceID, b.Generation, b.ProjectionRevision).Scan(&offset, &edited, &h.errors, &h.diagnostics)
	if err == sql.ErrNoRows {
		h.available = b.FromOffset == 0
		return h, nil // An old partial projection needs a rebuild, never guessed zeroes.
	}
	if err != nil {
		return h, err
	}
	if offset != b.FromOffset {
		return h, fmt.Errorf("%w: hook evidence checkpoint", ErrConflict)
	}
	h.available = true
	if edited {
		h.session.Observe("tool-call", "Edit", "checkpoint.js")
	}
	return h, nil
}

func (h *hookIndexEvidence) observe(e Event, fullText string) {
	if !h.available {
		return
	}
	if e.Kind == "indexing-error" {
		h.diagnostics++
	}
	if (e.Kind == "tool-call" || e.Kind == "tool-result") && e.SearchText == "" && e.Text != "" {
		// Imported/display-only events cannot establish full-text absence.
		h.diagnostics++
		return
	}
	var data struct {
		Tool string `json:"tool"`
	}
	_ = json.Unmarshal(e.Data, &data)
	h.session.Observe(e.Kind, data.Tool, fullText)
}

func (h hookIndexEvidence) save(ctx context.Context, tx *sql.Tx, b IndexBatch) error {
	if !h.available {
		return nil
	}
	var evidence hookideas.JavaScriptEvidence
	evidence.Add(b.Session.ID, h.session)
	if evidence.Errors > (1<<63-1)-h.errors {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO hook_javascript_evidence VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_id,generation,revision) DO UPDATE SET indexed_offset=excluded.indexed_offset,edited=excluded.edited,errors=excluded.errors,diagnostics=excluded.diagnostics`, b.SourceID, b.Generation, b.ProjectionRevision, b.ToOffset, evidence.SessionsEdited > 0, h.errors+evidence.Errors, h.diagnostics)
	return err
}

type JavaScriptHookEvidence struct {
	SessionID     string `json:"sessionId"`
	State         string `json:"state"`
	Snapshot      string `json:"snapshot"`
	IndexedOffset int64  `json:"indexedOffset"`
	Edited        bool   `json:"edited"`
	Errors        int64  `json:"errors"`
	Diagnostics   int64  `json:"diagnostics"`
}

// JavaScriptHookEvidence reads the current interpretation only. Readiness refers
// to indexed bytes, not uncaptured source history or pending upload/index work.
func (s *Store) JavaScriptHookEvidence(ctx context.Context, id, snapshot string) (JavaScriptHookEvidence, error) {
	var out JavaScriptHookEvidence
	token, err := s.historySnapshot(ctx, id, snapshot)
	if err != nil {
		return out, err
	}
	out.State, out.Snapshot, out.SessionID = "needs-rebuild", token, id
	err = s.db.QueryRowContext(ctx, `SELECT h.indexed_offset,h.edited,h.errors,h.diagnostics FROM sessions s JOIN hook_javascript_evidence h ON h.source_id=s.source_id AND h.generation=s.generation AND h.revision=COALESCE(json_extract(s.projection,'$.projectionRevision'),'') WHERE s.id=?`, id).Scan(&out.IndexedOffset, &out.Edited, &out.Errors, &out.Diagnostics)
	if err != nil && err != sql.ErrNoRows {
		return JavaScriptHookEvidence{}, err
	}
	if err == nil {
		out.State = "indexed-history"
		if out.Diagnostics > 0 {
			out.State = "incomplete-indexing"
		}
	}
	if _, err = s.historySnapshot(ctx, id, token); err != nil {
		return JavaScriptHookEvidence{}, err
	}
	return out, nil
}
