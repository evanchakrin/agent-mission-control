package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func undoFixtureBatch(t *testing.T, s *Store, count int) IndexBatch {
	t.Helper()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	for i := 0; i < count; i++ {
		b.Events = append(b.Events, Event{ID: fmt.Sprintf("undo-%d", i), Kind: "tool-call", SearchText: `{"command":"git restore -- file.js"}`, Data: json.RawMessage(`{"tool":"Bash"}`), SourceLength: 2})
	}
	return b
}

func TestUndoEvidenceStagingPublicationAndRestart(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 1)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "archive-undo", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "undo-rebuild", "3")
	if err != nil {
		t.Fatal(err)
	}
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	b.Events = append(b.Events, Event{ID: "second", Kind: "tool-call", SearchText: `{"command":"git revert abc"}`, Data: json.RawMessage(`{"tool":"Bash"}`), SourceLength: 2})
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	staged, err := s.GitUndoHistory(ctx, "session-1", before.Snapshot, 0, 10)
	if err != nil || !reflect.DeepEqual(before, staged) {
		t.Fatal("staged evidence leaked", staged, err)
	}
	dir := s.dir
	s.Close()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := s.GitUndoHistory(ctx, "session-1", "", 0, 10)
	if err != nil || after.Attempts != 2 || len(after.Events) != 2 || after.Snapshot == before.Snapshot {
		t.Fatal(after, err)
	}
	owner, err := s.GetSession(ctx, "session-1")
	if err != nil || !owner.Metadata.Archived {
		t.Fatal("publication changed archive", err)
	}
	if _, err := s.GitUndoHistory(ctx, "session-1", before.Snapshot, 0, 10); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
}

func TestUndoEvidenceDamageBlocksPublication(t *testing.T) {
	for _, damage := range []string{"checkpoint", "offset", "count", "rows"} {
		t.Run(damage, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			b := undoFixtureBatch(t, s, 1)
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetSession(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.BeginRebuild(ctx, "session-1", "undo-damage", "3")
			if err != nil {
				t.Fatal(err)
			}
			b.ProjectionRevision = r.Revision
			b.ParserState = json.RawMessage(`{"version":"3"}`)
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			if err := s.ReadyRebuild(ctx, r.Revision); err != nil {
				t.Fatal(err)
			}
			statements := map[string]string{"checkpoint": "DELETE FROM git_undo_checkpoints WHERE revision=?", "offset": "UPDATE git_undo_checkpoints SET indexed_offset=0 WHERE revision=?", "count": "UPDATE git_undo_checkpoints SET attempts=0 WHERE revision=?", "rows": "DELETE FROM git_undo_events WHERE revision=?"}
			if _, err := s.db.Exec(statements[damage], r.Revision); err != nil {
				t.Fatal(err)
			}
			if err := s.PublishRebuild(ctx, r.Revision); !errors.Is(err, ErrInvalid) {
				t.Fatal("damaged undo evidence published", err)
			}
			after, err := s.GetSession(ctx, "session-1")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed publication changed history", err)
			}
		})
	}
}

func TestUndoEvidenceAtomicDeduplicatedAndPaged(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 4)
	b.Events = append(b.Events, b.Events[0], Event{ID: "preview", Kind: "tool-call", Text: "truncated", Data: json.RawMessage(`{"tool":"Bash"}`), SourceLength: 2})
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, err := s.GitUndoHistory(ctx, "session-1", "", 0, 2)
	if err != nil || page.State != "incomplete-indexing" || page.Attempts != 4 || page.Unsupported != 1 || len(page.Events) != 2 || page.NextSequence == 0 {
		t.Fatal(page, err)
	}
	last, err := s.GitUndoHistory(ctx, "session-1", page.Snapshot, page.NextSequence, 2)
	if err != nil || len(last.Events) != 2 || last.NextSequence != 0 || last.Events[0].Sequence <= page.NextSequence {
		t.Fatal(last, err)
	}
	if _, err := s.GitUndoHistory(ctx, "session-1", "", page.NextSequence, 2); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.GitUndoHistory(ctx, "session-1", "stale", 0, 2); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
}

func TestUndoEvidenceRollbackAndOldAppendRemainExplicit(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 1)
	b.Events = append(b.Events, Event{ID: "invalid", SourceOffset: 99})
	if err := s.CommitIndex(ctx, b); err == nil {
		t.Fatal("invalid batch committed")
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM git_undo_events`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal(rows, err)
	}
	b.Events = b.Events[:1]
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM git_undo_checkpoints`); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	ingest(t, s, src, 2, "x\n")
	appendBatch := batch(src, 2, 4)
	appendBatch.Events = []Event{{ID: "later", Kind: "tool-call", SearchText: `{"command":"git reset --hard"}`, Data: json.RawMessage(`{"tool":"Bash"}`), SourceOffset: 2, SourceLength: 2}}
	if err := s.CommitIndex(ctx, appendBatch); err != nil {
		t.Fatal(err)
	}
	page, err := s.GitUndoHistory(ctx, "session-1", "", 0, 2)
	if err != nil || page.State != "needs-rebuild" || len(page.Events) != 0 {
		t.Fatal(page, err)
	}
}
