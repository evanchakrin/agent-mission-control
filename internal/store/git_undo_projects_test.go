package store

import (
	"context"
	"errors"
	"testing"
)

func TestUndoProjectRankingAndChangedCounts(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 3)
	b.Session.Project = "C:/z"
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"C:/a", "C:/b", ""} {
		src := testSource()
		src.SourceID = "source-" + name
		ingest(t, s, src, 0, "x\n")
		other := batch(src, 0, 2)
		other.Session.ID = "session-" + name
		other.Session.Project = name
		other.Events = append(other.Events, b.Events[0])
		other.Events[0].ID = "undo-" + name
		if err := s.CommitIndex(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "rank-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	first, err := s.GitUndoProjects(ctx, "", 2)
	if err != nil || len(first.Projects) != 2 || first.Projects[0].Project != "C:/z" || first.Projects[0].Attempts != 3 || first.Projects[0].Sessions != 1 || first.Projects[1].Project != "" || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	second, err := s.GitUndoProjects(ctx, first.NextCursor, 2)
	if err != nil || len(second.Projects) != 2 || second.Projects[0].Project != "C:/a" || second.Projects[1].Project != "C:/b" || second.NextCursor != "" {
		t.Fatal(second, err)
	}
	// Newly indexed evidence changes counts without requiring reindex publication.
	src := testSource()
	src.SourceID = "late-source"
	ingest(t, s, src, 0, "x\n")
	late := batch(src, 0, 2)
	late.Session.ID = "late-session"
	late.Session.Project = "C:/z"
	late.Events = append(late.Events, b.Events[0])
	late.Events[0].ID = "late-undo"
	if err := s.CommitIndex(ctx, late); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GitUndoProjects(ctx, first.NextCursor, 2); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("accepted stale ranking", err)
	}
	updated, err := s.GitUndoProjects(ctx, "", 2)
	if err != nil || updated.Projects[0].Attempts != 4 || updated.Projects[0].Sessions != 2 {
		t.Fatal(updated, err)
	}
	for _, cursor := range []string{"bad", "e30"} {
		if _, err := s.GitUndoProjects(ctx, cursor, 1); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := s.GitUndoProjects(ctx, "", 101); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
