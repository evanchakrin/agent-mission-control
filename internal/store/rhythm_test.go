package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRhythmUsesEarliestEvidenceCivilHoursAndCurrentScopes(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	for i, at := range []string{"2026-11-01T05:30:00.000000000Z", "2026-11-01T06:30:00.000000000Z", "0001-01-01T00:00:00.000000000Z"} {
		if _, err := s.db.Exec(`INSERT INTO query_event_agents SELECT id,source_id,generation,projection_revision,'main',1,1,0,0,0,?,? FROM query_sessions WHERE id=?`, at, at, fmt.Sprintf("s-%06d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE query_usage SET timestamp=? WHERE session_id=?`, at, fmt.Sprintf("s-%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE query_event_agents SET errors=3,unknown_results=1 WHERE session_id='s-000000';
 INSERT INTO query_usage SELECT 'earlier',id,source_id,generation,projection_revision,'usage-agent','model','message','2026-11-01T05:15:00.000000000Z',1,0,0,1 FROM query_sessions WHERE id='s-000000';
 INSERT INTO query_usage SELECT 'wrong-source',id,'wrong-source',generation,projection_revision,'other','model','message','2020-01-01T00:00:00.000000000Z',1,0,0,1 FROM query_sessions WHERE id='s-000000';
 INSERT INTO query_event_agents SELECT session_id,source_id,generation,'stale-revision',agent_id,events,tool_calls,99,0,0,'2020-01-01T00:00:00.000000000Z',last_at FROM query_event_agents WHERE session_id='s-000001'`); err != nil {
		t.Fatal(err)
	}
	q := RhythmQuery{Timezone: "America/New_York"}
	result, err := s.Rhythm(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	hour := result.Hours[1]
	if len(result.Hours) != 24 || len(result.Weekdays) != 7 || result.Sessions != 2 || result.UndatedSessions != 1 || hour.Sessions != 2 || hour.RecordedTokens != 8 || hour.SessionsWithErrors != 1 || hour.SessionsWithIncompleteResults != 1 || hour.TopTierCost != nil || result.Weekdays[6].Sessions != 2 {
		t.Fatalf("wrong rhythm %+v %+v", result, hour)
	}
	from := time.Date(2026, 11, 1, 5, 20, 0, 0, time.UTC)
	q.From = &from
	result, err = s.Rhythm(ctx, q)
	if err != nil || result.Sessions != 1 || result.OutsideRangeSessions != 1 || result.Hours[1].SessionsWithErrors != 0 {
		t.Fatal("start filtering ignored earliest usage", result, err)
	}
	yes, no := true, false
	if _, err = s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "rhythm-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q.Archived = &no
	result, err = s.Rhythm(ctx, q)
	if err != nil || result.Sessions != 0 {
		t.Fatal("archive ignored", result, err)
	}
}

func TestRhythmRejectsUnboundedOrInvalidParameters(t *testing.T) {
	s := openTestStore(t, Options{})
	for _, q := range []RhythmQuery{{}, {Timezone: "Local"}, {Timezone: "bad-zone"}, {Timezone: "UTC", SessionQuery: SessionQuery{Cursor: "cursor"}}, {Timezone: "UTC", SessionQuery: SessionQuery{Limit: 100}}} {
		if _, err := s.Rhythm(context.Background(), q); !errors.Is(err, ErrInvalid) {
			t.Fatal(q, err)
		}
	}
}

func TestRhythm100001SessionsKeepsBoundedOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("100001 session native scale fixture")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 100001)
	started := time.Now()
	result, err := s.Rhythm(context.Background(), RhythmQuery{Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	var hours, weekdays, tokens int64
	for _, b := range result.Hours {
		hours += b.Sessions
		tokens += b.RecordedTokens
	}
	for _, b := range result.Weekdays {
		weekdays += b.Sessions
	}
	if hours != 100001 || weekdays != hours || result.Sessions != hours || tokens != 400004 || len(result.Hours) != 24 || len(result.Weekdays) != 7 {
		t.Fatal("history omitted or multiplied", result)
	}
	t.Logf("sessions=%d buckets=%d elapsed=%s", result.Sessions, len(result.Hours)+len(result.Weekdays), time.Since(started))
}
