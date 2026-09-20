package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFleetUndoChronologyCoverageAndPublication(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 4)
	newer := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	b.Events[0].Timestamp = newer
	b.Events[1].Timestamp = newer.Add(-time.Hour)
	b.Events[2].Timestamp = newer
	// The last inserted event has no timestamp, and must sort last.
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	yes := true
	name := "Owner title"
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "fleet-archive", Archived: &yes, Name: &name}); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: current.MachineID, DisplayName: "Owner machine label", OperationID: "fleet-label"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.FleetGitUndoHistory(ctx, "", 2)
	if err != nil || len(first.Events) != 2 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	if first.Events[0].MachineName != "Owner machine label" || first.Events[0].MachineID != current.MachineID {
		t.Fatal("machine identity/display", first.Events[0])
	}
	if first.Events[0].Sequence <= first.Events[1].Sequence || !first.Events[0].Timestamp.Equal(newer) || !first.Events[0].Archived || first.Events[0].Title != name {
		t.Fatal(first)
	}
	second, err := s.FleetGitUndoHistory(ctx, first.NextCursor, 2)
	if err != nil || len(second.Events) != 2 || second.NextCursor != "" || second.Events[1].Timestamp != nil || !second.Events[0].Timestamp.Equal(newer.Add(-time.Hour)) {
		t.Fatal(second, err)
	}
	summary, err := s.GitUndoSummary(ctx)
	if err != nil || summary.Sessions != 1 || summary.Ready != 1 || summary.ReadyAttempts != 4 || summary.Attempts != 4 {
		t.Fatal(summary, err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "fleet-rebuild", "3")
	if err != nil {
		t.Fatal(err)
	}
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	staged, err := s.FleetGitUndoHistory(ctx, "", 100)
	if err != nil || len(staged.Events) != 4 || staged.Snapshot != first.Snapshot {
		t.Fatal(staged, err)
	}
	if err = s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FleetGitUndoHistory(ctx, first.NextCursor, 2); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM git_undo_checkpoints WHERE revision=?`, r.Revision); err != nil {
		t.Fatal(err)
	}
	summary, err = s.GitUndoSummary(ctx)
	if err != nil || summary.NeedsRebuild != 1 || summary.Attempts != 0 || summary.Ready != 0 {
		t.Fatal(summary, err)
	}
}

func TestFleetUndoTimestampMigrationAndQueryPlan(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 2)
	b.Events[0].Timestamp = time.Date(2026, 9, 6, 12, 0, 0, 123, time.UTC)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	// Recreate the deployed predecessor schema, preserving evidence in this fixture.
	_, err := s.db.Exec(`DROP INDEX git_undo_time; ALTER TABLE git_undo_events DROP COLUMN timestamp;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.setupGitUndo(); err != nil {
		t.Fatal(err)
	}
	if err = s.setupGitUndo(); err != nil {
		t.Fatal("migration not repeatable", err)
	}
	page, err := s.FleetGitUndoHistory(ctx, "", 1)
	if err != nil || len(page.Events) != 1 || !page.Events[0].Timestamp.Equal(b.Events[0].Timestamp) {
		t.Fatal(page, err)
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT u.event_seq `+undoFleetFrom+`WHERE (u.timestamp,u.event_seq)<(?,?) ORDER BY u.timestamp DESC,u.event_seq DESC LIMIT ?`, stamp(b.Events[0].Timestamp), 100, 26)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatal("unbounded sort", detail)
		}
		if strings.Contains(detail, "SEARCH u USING INDEX git_undo_time") || strings.Contains(detail, "SEARCH u USING COVERING INDEX git_undo_time") {
			indexed = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("continuation must seek timestamp index")
	}
	for _, cursor := range []string{"broken", "e30"} {
		if _, err = s.FleetGitUndoHistory(ctx, cursor, 10); !errors.Is(err, ErrInvalid) {
			t.Fatal(cursor, err)
		}
	}
	if _, err = s.FleetGitUndoHistory(ctx, "", 101); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestFleetUndoProjectFilterPrecedesPagingAndPinsScope(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	b := undoFixtureBatch(t, s, 2)
	b.Session.Project = "C:/repo <one>"
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	src.SourceID = "other-source"
	ingest(t, s, src, 0, "x\n")
	other := batch(src, 0, 2)
	other.Session.ID = "other-session"
	other.Session.Project = "C:/other"
	other.Events = append(other.Events, b.Events[0])
	other.Events[0].ID = "other-undo"
	other.Events[0].Timestamp = time.Now().UTC()
	if err := s.CommitIndex(ctx, other); err != nil {
		t.Fatal(err)
	}
	project := b.Session.Project
	first, err := s.FleetGitUndoHistoryForProject(ctx, "", 1, &project)
	if err != nil || len(first.Events) != 1 || first.Events[0].SessionID != "session-1" || first.Events[0].Project != project || first.Project == nil || *first.Project != project || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	last, err := s.FleetGitUndoHistoryForProject(ctx, first.NextCursor, 1, &project)
	if err != nil || len(last.Events) != 1 || last.Events[0].Sequence == first.Events[0].Sequence || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	if _, err = s.FleetGitUndoHistory(ctx, first.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("project cursor accepted for fleet", err)
	}
	missing := ""
	empty, err := s.FleetGitUndoHistoryForProject(ctx, "", 1, &missing)
	if err != nil || len(empty.Events) != 0 || empty.Project == nil {
		t.Fatal(empty, err)
	}
	oversized := strings.Repeat("x", 4097)
	if _, err = s.FleetGitUndoHistoryForProject(ctx, "", 1, &oversized); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
