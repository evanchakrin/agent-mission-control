package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestTroubleFilesPerFileDenominatorAndPaging(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("trouble-source-%d", i)
		src.ModifiedAt = now.Add(-time.Hour)
		if i == 6 {
			src.MachineID = "another-machine"
		}
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("trouble-session-%d", i)
		path := "C:/repo/shared.go"
		if i == 5 {
			path = "C:/repo/other.go"
		}
		input, _ := json.Marshal(map[string]string{"file_path": path})
		b.Events = []Event{{ID: fmt.Sprintf("call-%d", i), AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-time.Hour), SourceLength: 2, SearchText: string(input), Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
		if i < 2 {
			b.Events = append(b.Events, Event{ID: fmt.Sprintf("failure-%d", i), AgentID: "main", Kind: "tool-result", Timestamp: now.Add(-59 * time.Minute), SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"edit","error":true}`)})
		}
		if i == 0 {
			b.Events = append(b.Events, Event{ID: "repeat-edit", AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-58 * time.Minute), SourceLength: 2, SearchText: string(input), Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit-again"}`)})
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "trouble-session-0", MetadataPatch{Archived: &yes, OperationID: "trouble-archive"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.TroubleFiles(ctx, "", 1, now)
	if err != nil || len(page.Files) != 1 || page.TotalSessions != 7 || page.IndexedSessions != 7 {
		t.Fatal(page, err)
	}
	f := page.Files[0]
	for _, field := range []string{"path", "sessions", "badSessions", "unknownSessions", "rate", "lastTouched"} {
		for _, direction := range []string{"asc", "desc"} {
			whole, err := s.TroubleFilesSorted(ctx, "", 100, now, field, direction)
			if err != nil {
				t.Fatal(field, direction, err)
			}
			var all []TroubleFile
			cursor := ""
			for step := 0; step < 4; step++ {
				part, err := s.TroubleFilesSorted(ctx, cursor, 1, now, field, direction)
				if err != nil {
					t.Fatal(field, direction, err)
				}
				all = append(all, part.Files...)
				if part.NextCursor == "" {
					break
				}
				cursor = part.NextCursor
			}
			if !reflect.DeepEqual(all, whole.Files) {
				t.Fatal("sorting changed across pages", field, direction, all, whole.Files)
			}
			for i := 1; i < len(all); i++ {
				a, b := all[i-1], all[i]
				ordered := true
				switch field {
				case "path":
					if direction == "asc" {
						ordered = a.Path <= b.Path
					} else {
						ordered = a.Path >= b.Path
					}
				case "lastTouched":
					if direction == "asc" {
						ordered = a.LastTouched <= b.LastTouched
					} else {
						ordered = a.LastTouched >= b.LastTouched
					}
				default:
					value := func(f TroubleFile) float64 {
						switch field {
						case "sessions":
							return float64(f.Sessions)
						case "badSessions":
							return float64(f.BadSessions)
						case "unknownSessions":
							return float64(f.UnknownSessions)
						default:
							if f.Rate == nil {
								return -1
							}
							return *f.Rate
						}
					}
					if direction == "asc" {
						ordered = value(a) <= value(b)
					} else {
						ordered = value(a) >= value(b)
					}
				}
				if !ordered {
					t.Fatal("wrong sort direction", field, direction, all)
				}
			}
		}
	}
	if _, err := s.TroubleFilesSorted(ctx, page.NextCursor, 1, now, "path", "asc"); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("sort change accepted old cursor", err)
	}
	if _, err := s.TroubleFilesSorted(ctx, "", 1, now, "path;DROP TABLE sessions", "asc"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	refs, err := s.TroubleFileSessions(ctx, f.MachineID, f.Project, f.Path, "", 2, now)
	if err != nil || refs.Total != 5 || len(refs.Sessions) != 2 || refs.NextCursor == "" {
		t.Fatal(refs, err)
	}
	rest, err := s.TroubleFileSessions(ctx, f.MachineID, f.Project, f.Path, refs.NextCursor, 10, now.Add(time.Hour))
	if err != nil || rest.Total != 5 || len(rest.Sessions) != 3 || rest.Snapshot != refs.Snapshot {
		t.Fatal(rest, err)
	}
	if _, err = s.TroubleFileSessions(ctx, "another-machine", f.Project, f.Path, refs.NextCursor, 10, now); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("drilldown cursor crossed machine scope", err)
	}
	if f.Sessions != 5 || f.BadSessions != 2 || f.UnknownSessions != 0 || f.Rate == nil || *f.Rate != 40 || page.NextCursor == "" {
		t.Fatal(f, page.NextCursor)
	}
	next, err := s.TroubleFiles(ctx, page.NextCursor, 10, now.Add(time.Hour))
	if err != nil || len(next.Files) != 2 || next.Snapshot != page.Snapshot || !next.ObservedAt.Equal(now) {
		t.Fatal(next, err)
	}
	for _, f := range next.Files {
		if f.Sessions != 1 || f.Rate != nil {
			t.Fatal("small sample assigned a rate", f)
		}
	}
	// A changed outcome invalidates ranked continuation, even when no file touch
	// was added and membership counts did not move.
	if _, err = s.db.Exec(`UPDATE events SET data='{"toolUseId":"edit","error":false}' WHERE id='failure-0'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TroubleFiles(ctx, page.NextCursor, 10, now); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("changed outcome did not invalidate cursor", err)
	}
}

func TestTroubleFilesUnknownOutcomeSuppressesRate(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("unknown-source-%d", i)
		src.ModifiedAt = now.Add(-time.Hour)
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("unknown-session-%d", i)
		b.Events = []Event{{ID: fmt.Sprintf("unknown-call-%d", i), AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-time.Hour), SourceLength: 2, SearchText: `{"file_path":"same.go"}`, Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
		if i == 0 {
			b.Events = append(b.Events, Event{ID: "unknown-result", AgentID: "main", Kind: "tool-result", Timestamp: now.Add(-time.Hour), SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"edit"}`)})
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.TroubleFiles(ctx, "", 20, now)
	if err != nil || len(page.Files) != 1 {
		t.Fatal(page, err)
	}
	if f := page.Files[0]; f.Sessions != 5 || f.UnknownSessions != 1 || f.BadSessions != 0 || f.Rate != nil {
		t.Fatal(f)
	}
}

func TestTroubleFilesCurrentStallAndOrganizationIndependence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Now().UTC()
	src := testSource()
	src.ModifiedAt = now.Add(-time.Minute)
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{{ID: "stalled-edit", AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-3 * time.Minute), SourceLength: 2, SearchText: `{"file_path":"same.go"}`, Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	organization := "different-owner-project"
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{Project: &organization, OperationID: "move-file-session"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.TroubleFiles(ctx, "", 20, now)
	if err != nil || len(page.Files) != 1 || page.Files[0].BadSessions != 1 || page.Files[0].Project != b.Session.Project {
		t.Fatal(page, err)
	}
	quiet, err := s.TroubleFiles(ctx, "", 20, now.Add(time.Hour))
	if err != nil || quiet.Files[0].BadSessions != 0 {
		t.Fatal("quiet source labelled currently stalled", quiet, err)
	}
}

func TestTroubleFilesLatestTimeUsesChronologyNotText(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(100 * time.Millisecond), base.Add(100 * time.Millisecond).In(time.FixedZone("fixture", 7200))}
	for i, modified := range times {
		src := testSource()
		src.SourceID = fmt.Sprintf("time-source-%d", i)
		src.ModifiedAt = modified
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("time-session-%d", i)
		b.Events = []Event{{ID: fmt.Sprintf("time-event-%d", i), AgentID: "main", Kind: "tool-call", Timestamp: base, SourceLength: 2, SearchText: `{"file_path":"time.go"}`, Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.TroubleFilesSorted(ctx, "", 20, base.Add(time.Hour), "lastTouched", "desc")
	if err != nil || len(page.Files) != 1 {
		t.Fatal(page, err)
	}
	got, err := time.Parse(time.RFC3339Nano, page.Files[0].LastTouched)
	if err != nil || !got.Equal(base.Add(100*time.Millisecond)) {
		t.Fatal("latest fractional-second source was lost", got, err)
	}
	f := page.Files[0]
	refs, err := s.TroubleFileSessions(ctx, f.MachineID, f.Project, f.Path, "", 1, base.Add(time.Hour))
	if err != nil || len(refs.Sessions) != 1 || refs.Sessions[0].ID != "time-session-1" || refs.NextCursor == "" {
		t.Fatal("file sessions did not put the newer fractional timestamp first", refs, err)
	}
	next, err := s.TroubleFileSessions(ctx, f.MachineID, f.Project, f.Path, refs.NextCursor, 1, base.Add(time.Hour))
	if err != nil || len(next.Sessions) != 1 || next.Sessions[0].ID != "time-session-2" || next.NextCursor == "" {
		t.Fatal("chronological continuation skipped or repeated a session", next, err)
	}
	last, err := s.TroubleFileSessions(ctx, f.MachineID, f.Project, f.Path, next.NextCursor, 1, base.Add(time.Hour))
	if err != nil || len(last.Sessions) != 1 || last.Sessions[0].ID != "time-session-0" || last.NextCursor != "" {
		t.Fatal("timezone tie changed continuation", last, err)
	}
}

func TestTroubleFileDrilldownScopesSessionsNotTheirOutcomeEvents(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	var machine, project string
	for i := 0; i < 4; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("scope-source-%d", i)
		src.ModifiedAt = now.Add(-time.Hour)
		if i == 3 {
			src.MachineID = "different-machine"
		}
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("scope-session-%d", i)
		path := "selected.go"
		if i == 2 {
			path = "unrelated.go"
		}
		input, _ := json.Marshal(map[string]string{"file_path": path})
		b.Events = []Event{
			{ID: fmt.Sprintf("scope-edit-%d", i), AgentID: "main", Kind: "tool-call", Timestamp: src.ModifiedAt, SourceLength: 2, SearchText: string(input), Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)},
			{ID: fmt.Sprintf("scope-edit-result-%d", i), AgentID: "main", Kind: "tool-result", Timestamp: src.ModifiedAt, SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"edit","error":false}`)},
		}
		if i != 1 {
			// Failure is outside the selected edit and even outside its agent.
			b.Events = append(b.Events,
				Event{ID: fmt.Sprintf("scope-read-%d", i), AgentID: "child", Kind: "tool-call", Timestamp: src.ModifiedAt, SourceLength: 2, Data: json.RawMessage(`{"tool":"Read","toolUseId":"read"}`)},
				Event{ID: fmt.Sprintf("scope-read-result-%d", i), AgentID: "child", Kind: "tool-result", Timestamp: src.ModifiedAt, SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"read","error":true}`)},
			)
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			machine, project = src.MachineID, b.Session.Project
		}
	}
	page, err := s.TroubleFileSessions(ctx, machine, project, "selected.go", "", 1, now)
	if err != nil || page.Total != 2 || len(page.Sessions) != 1 || page.Sessions[0].ID != "scope-session-0" || !page.Sessions[0].Bad || page.Sessions[0].Unknown || page.NextCursor == "" {
		t.Fatal("lost cross-agent outcome or included an unrelated session", page, err)
	}
	next, err := s.TroubleFileSessions(ctx, machine, project, "selected.go", page.NextCursor, 1, now)
	if err != nil || len(next.Sessions) != 1 || next.Sessions[0].ID != "scope-session-1" || next.Sessions[0].Bad || next.Sessions[0].Unknown || next.NextCursor != "" {
		t.Fatal("scoped continuation changed evidence", next, err)
	}
}
