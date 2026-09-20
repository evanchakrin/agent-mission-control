package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUnsavedCandidatesPreservePerRecordDirectory(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Session.Project = "C:/final-project-is-not-record-context"
	b.Events = nil
	for i, path := range []string{"relative.go", "relative.go", "relative.go", "C:/fixed.go", "C:/fixed.go"} {
		cwd := []string{"C:/first", "C:/second", "", "C:/first", "C:/second"}[i]
		data, _ := json.Marshal(map[string]string{"tool": "Edit", "workingDirectory": cwd})
		input, _ := json.Marshal(map[string]string{"file_path": path})
		b.Events = append(b.Events, Event{ID: fmt.Sprint("cwd-", i), Kind: "tool-call", SourceLength: 2, SearchText: string(input), Data: data})
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := s.UnsavedCandidates(ctx, src.MachineID, cursor, 1)
		if err != nil || page.TotalFiles != 4 || len(page.Files) != 1 {
			t.Fatal(page, err)
		}
		f := page.Files[0]
		key := f.Path + "|" + f.WorkingDirectory
		if seen[key] || f.Sessions != 1 {
			t.Fatal("merged or repeated file context", f)
		}
		seen[key] = true
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	for _, key := range []string{"relative.go|C:/first", "relative.go|C:/second", "relative.go|", "C:/fixed.go|"} {
		if !seen[key] {
			t.Fatal("lost context", key, seen)
		}
	}
}

func TestUnsavedCandidatesWholeHistoryScopeAndContinuation(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	var machine string
	for i := 0; i < 4; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("candidate-source-%d", i)
		if i == 3 {
			src.MachineID = "other-machine"
		} else {
			machine = src.MachineID
		}
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("candidate-session-%d", i)
		path := "shared.go"
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i == 2 {
			path = "other.go"
			at = base.Add(-time.Hour)
		}
		input, _ := json.Marshal(map[string]string{"file_path": path})
		b.Events = []Event{{ID: fmt.Sprintf("candidate-edit-%d", i), AgentID: "main", Kind: "tool-call", Timestamp: at, SourceLength: 2, SearchText: string(input), Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
		if i == 0 {
			b.Events = append(b.Events, Event{ID: "candidate-repeat", AgentID: "main", Kind: "tool-call", Timestamp: base.Add(50 * time.Millisecond), SourceLength: 2, SearchText: string(input), Data: json.RawMessage(`{"tool":"Edit","toolUseId":"repeat"}`)})
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	project := "owner-organization-only"
	if _, err := s.PatchMetadata(ctx, "candidate-session-1", MetadataPatch{Archived: &yes, Project: &project, OperationID: "candidate-archive"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.UnsavedCandidates(ctx, machine, "", 1)
	if err != nil || len(page.Files) != 1 || page.TotalFiles != 2 || page.TotalSessions != 3 || page.IndexedSessions != 3 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	f := page.Files[0]
	if f.Path != "shared.go" || f.Sessions != 2 || f.SessionID != "candidate-session-1" || f.Project == project || f.MachineID != machine || f.LastTouched != "2026-01-01T10:00:00.100Z" {
		t.Fatal(f)
	}
	next, err := s.UnsavedCandidates(ctx, machine, page.NextCursor, 1)
	if err != nil || len(next.Files) != 1 || next.Files[0].Path != "other.go" || next.NextCursor != "" || next.Snapshot != page.Snapshot {
		t.Fatal(next, err)
	}
	if _, err := s.UnsavedCandidates(ctx, "other-machine", page.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cross-machine cursor accepted", err)
	}
	// A damaged/missing checkpoint must reduce visible coverage, not hide history.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM file_edit_checkpoints WHERE source_id='candidate-source-0'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnsavedCandidates(ctx, machine, page.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed coverage accepted an old cursor", err)
	}
	incomplete, err := s.UnsavedCandidates(ctx, machine, "", 100)
	if err != nil || incomplete.IndexedSessions != 2 || incomplete.TotalFiles != 2 {
		t.Fatal(incomplete, err)
	}
}
