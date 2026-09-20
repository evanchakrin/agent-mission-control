package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestFileEditRebuildRejectsDamagedCheckpoint(t *testing.T) {
	for _, version := range []string{"6", "7"} {
		for _, damage := range []string{"missing", "offset", "count"} {
			t.Run(version+"/"+damage, func(t *testing.T) {
				s := openTestStore(t, Options{})
				ctx := context.Background()
				b := undoFixtureBatch(t, s, 1)
				if err := s.CommitIndex(ctx, b); err != nil {
					t.Fatal(err)
				}
				r, err := s.BeginRebuild(ctx, "session-1", "damaged-edits", version)
				if err != nil {
					t.Fatal(err)
				}
				b.ProjectionRevision = r.Revision
				b.ParserState = json.RawMessage(`{"version":"` + version + `"}`)
				if err = s.CommitIndex(ctx, b); err != nil {
					t.Fatal(err)
				}
				statements := map[string]string{"missing": `DELETE FROM file_edit_checkpoints WHERE revision=?`, "offset": `UPDATE file_edit_checkpoints SET indexed_offset=1 WHERE revision=?`, "count": `UPDATE file_edit_checkpoints SET attempts=1 WHERE revision=?`}
				if _, err = s.db.Exec(statements[damage], r.Revision); err != nil {
					t.Fatal(err)
				}
				if err = s.ReadyRebuild(ctx, r.Revision); !errors.Is(err, ErrInvalid) {
					t.Fatal("damaged evidence accepted", err)
				}
			})
		}
	}
}

func TestPriorUndoEditsArePinnedEarlierAndPaginated(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 1)
	undo := b.Events[0]
	edit := func(id, tool, input string) Event {
		return Event{ID: id, Kind: "tool-call", SearchText: input, Data: json.RawMessage(`{"tool":"` + tool + `"}`), SourceLength: 2}
	}
	b.Events = []Event{edit("first", "Edit", `{"file_path":"C:\\repo\\<a>.js","new_string":"ignored"}`), edit("second", "NotebookEdit", `{"notebook_path":"relative.ipynb"}`), undo, edit("later", "Write", `{"file_path":"later.txt"}`)}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	history, err := s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	anchor := history.Events[0].Sequence
	page, err := s.PriorUndoEdits(ctx, "session-1", history.Snapshot, anchor, 0, 1)
	if err != nil || page.State != "indexed-tool-calls" || len(page.Edits) != 1 || page.Edits[0].Path != "relative.ipynb" || page.NextBefore == 0 {
		t.Fatal(page, err)
	}
	next, err := s.PriorUndoEdits(ctx, "session-1", history.Snapshot, anchor, page.NextBefore, 1)
	if err != nil || len(next.Edits) != 1 || next.Edits[0].Path != `C:\repo\<a>.js` || next.NextBefore != 0 {
		t.Fatal(next, err)
	}
	if _, err := s.PriorUndoEdits(ctx, "session-1", "stale", anchor, 0, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	if _, err := s.PriorUndoEdits(ctx, "session-1", history.Snapshot, anchor+1, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.PriorUndoEdits(ctx, "session-1", "", anchor, 0, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// Removing only synthetic derived metadata must not imply absent prior edits.
	if _, err := s.db.Exec(`DELETE FROM file_edit_checkpoints`); err != nil {
		t.Fatal(err)
	}
	missing, err := s.PriorUndoEdits(ctx, "session-1", history.Snapshot, anchor, 0, 10)
	if err != nil || missing.State != "needs-rebuild" || len(missing.Edits) != 0 {
		t.Fatal(missing, err)
	}
}

func TestFileEditsRejectPreviewFallbackAndPreserveRebuildIsolation(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 1)
	b.Events = append([]Event{{ID: "bad-edit", Kind: "tool-call", Text: `{"file_path":"preview.js"}`, Data: json.RawMessage(`{"tool":"Edit"}`), SourceLength: 2}}, b.Events...)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	history, err := s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.PriorUndoEdits(ctx, "session-1", history.Snapshot, history.Events[0].Sequence, 0, 10)
	if err != nil || page.State != "incomplete-indexing" || page.Diagnostics != 1 || len(page.Edits) != 0 {
		t.Fatal(page, err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "edit-rebuild", "6")
	if err != nil {
		t.Fatal(err)
	}
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"6"}`)
	b.Events[0].SearchText = `{"file_path":"full.js"}`
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, err = s.PriorUndoEdits(ctx, "session-1", history.Snapshot, history.Events[0].Sequence, 0, 10)
	if err != nil || len(page.Edits) != 0 {
		t.Fatal("staged evidence leaked", page, err)
	}
	if err = s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PriorUndoEdits(ctx, "session-1", history.Snapshot, history.Events[0].Sequence, 0, 10); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	history, err = s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	page, err = s.PriorUndoEdits(ctx, "session-1", history.Snapshot, history.Events[0].Sequence, 0, 10)
	if err != nil || len(page.Edits) != 1 || page.Edits[0].Path != "full.js" {
		t.Fatal(page, err)
	}
}
