package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestCalendarRevisionRejectsEditsAndRestoreButNotUnchangedHeartbeat(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	h := protocol.Heartbeat{MachineID: "m0", Name: "Desk"}
	if err := s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	q := CalendarQuery{Start: "2026-01-01", End: "2026-01-04", Timezone: "UTC", SessionQuery: SessionQuery{Limit: 1}}
	first, err := s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	q.Snapshot = first.Snapshot
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Calendar(ctx, q); err != nil {
		t.Fatal("ordinary heartbeat invalidated calendar", err)
	}
	h.Name = "Renamed"
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Calendar(ctx, q); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("rename retained old calendar revision", err)
	}
	q.Cursor = ""
	q.Snapshot = ""
	first, err = s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	q.Snapshot = first.Snapshot
	yes := true
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "snapshot-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Calendar(ctx, q); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("archive retained old calendar revision", err)
	}
	q.Cursor = ""
	q.Snapshot = ""
	first, err = s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	q.Snapshot = first.Snapshot
	if _, err = s.db.ExecContext(ctx, "UPDATE properties SET value='restored-calendar' WHERE key='recovery_epoch'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Calendar(ctx, q); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("restore retained old calendar revision", err)
	}
}

func TestCalendarCivilDaysIncludeDSTAndPaginate(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, tc := range []struct {
		day   string
		hours int
	}{{"2026-03-08", 23}, {"2026-11-01", 25}} {
		at, _ := time.Parse("2006-01-02", tc.day)
		q := CalendarQuery{Start: tc.day, End: at.AddDate(0, 0, 1).Format("2006-01-02"), Timezone: "America/New_York"}
		page, err := s.Calendar(ctx, q)
		if err != nil || len(page.Days) != 1 {
			t.Fatal(page, err)
		}
		d := page.Days[0]
		if d.To.Sub(d.From) != time.Duration(tc.hours)*time.Hour {
			t.Fatal("DST boundary wrong", d)
		}
	}
	q := CalendarQuery{Start: "2024-01-01", End: "2025-01-01", Timezone: "UTC", SessionQuery: SessionQuery{Limit: 100}}
	seen := map[string]bool{}
	for {
		page, err := s.Calendar(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Days) > 100 {
			t.Fatal("unbounded day page")
		}
		for _, d := range page.Days {
			if seen[d.Day] {
				t.Fatal("repeated day")
			}
			seen[d.Day] = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
		q.Snapshot = page.Snapshot
	}
	if len(seen) != 366 || !seen["2024-02-29"] {
		t.Fatal("leap year incomplete", len(seen))
	}
}

func TestCalendarMidnightDSTUsesFirstExistingInstant(t *testing.T) {
	s := openTestStore(t, Options{})
	page, err := s.Calendar(context.Background(), CalendarQuery{Start: "2018-11-04", End: "2018-11-05", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	d := page.Days[0]
	if d.From.Format(time.RFC3339) != "2018-11-04T03:00:00Z" || d.To.Sub(d.From) != 23*time.Hour {
		t.Fatal("midnight DST misbucketed", d)
	}
	_, err = s.Calendar(context.Background(), CalendarQuery{Start: "2011-12-30", End: "2011-12-31", Timezone: "Pacific/Apia"})
	if err == nil {
		t.Fatal("nonexistent civil date was silently normalized")
	}
}

func TestCalendar100001SessionsPreservesWholeCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("100001-session calendar fixture")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 100001)
	q := CalendarQuery{Start: "2026-01-01", End: "2026-05-01", Timezone: "UTC", SessionQuery: SessionQuery{Limit: 100}}
	var sessions, tokens, agents, estimated int64
	for {
		started := time.Now()
		page, err := s.Calendar(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("calendar days=%d elapsed=%s", len(page.Days), time.Since(started))
		if len(page.Days) > 100 {
			t.Fatal("unbounded calendar")
		}
		for _, d := range page.Days {
			sessions += d.Sessions
			tokens += d.RecordedTokens
			agents += d.AgentScopes
			estimated += d.SessionsWithEstimate
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
		q.Snapshot = page.Snapshot
	}
	if sessions != 100001 || tokens != 400004 || agents != 100001 || estimated != 33334 {
		t.Fatal("calendar omitted or multiplied history", sessions, tokens, agents, estimated)
	}
}

func TestCalendarFiltersAndBoundariesPreserveKnownCosts(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	// midnight local, final nanosecond before it, following midnight.
	for i, at := range []string{"2026-03-08T05:00:00.000000000Z", "2026-03-08T04:59:59.999999999Z", "2026-03-09T04:00:00.000000000Z"} {
		if _, err := s.db.ExecContext(ctx, "UPDATE sessions SET projection=json_set(projection,'$.lastActivity',?),last_activity=? WHERE id=?", at, at, fmt.Sprintf("s-%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	q := CalendarQuery{Start: "2026-03-08", End: "2026-03-09", Timezone: "America/New_York"}
	page, err := s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	d := page.Days[0]
	if d.Sessions != 1 || d.RecordedTokens != 4 || d.AgentScopes != 1 || d.CostEstimate == nil || *d.CostEstimate != 0 || d.SessionsWithEstimate != 1 {
		t.Fatalf("boundary or zero-price lost: %+v", d)
	}
	yes, no := true, false
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "calendar-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q.Archived = &no
	page, err = s.Calendar(ctx, q)
	if err != nil || page.Days[0].Sessions != 0 {
		t.Fatal("archive ignored", page, err)
	}
	q.Archived = &yes
	q.Text = "Transcript"
	q.MachineID = "m0"
	page, err = s.Calendar(ctx, q)
	if err != nil || page.Days[0].Sessions != 1 {
		t.Fatal("combined filters ignored", page, err)
	}
}
