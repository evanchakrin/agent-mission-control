package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRingsCivilWeeksWindowAndEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 4)
	ctx := context.Background()
	for i, at := range []string{"2026-11-02T15:00:00.000000000Z", "2026-11-01T15:00:00.000000000Z", "2026-06-01T15:00:00.000000000Z", ""} {
		if _, err := s.db.Exec(`UPDATE sessions SET project='p',last_activity=? WHERE id=?`, at, fmt.Sprintf("s-%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO query_event_agents SELECT id,source_id,generation,projection_revision,'main',7,7,7,1,0,last_activity,last_activity FROM query_sessions WHERE id='s-000001'`); err != nil {
		t.Fatal(err)
	}
	q := RhythmQuery{Timezone: "America/New_York", SessionQuery: SessionQuery{Project: "p"}}
	got, err := s.Rings(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Weeks) != 10 || got.Sessions != 4 || got.OlderSessions != 1 || got.UndatedSessions != 1 {
		t.Fatal(got)
	}
	prior, last := got.Weeks[8], got.Weeks[9]
	if prior.Week != "2026-10-26" || prior.To.Sub(prior.From) != 169*time.Hour || prior.Sessions != 1 || prior.SessionsWithErrors != 1 || prior.SessionsWithIncompleteResults != 1 || last.Week != "2026-11-02" || last.Sessions != 1 {
		t.Fatal(prior, last)
	}
	yes := true
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "ring-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Rings(ctx, q)
	if err != nil || got.Weeks[len(got.Weeks)-1].Week != "2026-10-26" || got.Sessions != 3 {
		t.Fatal(got, err)
	}
}

func TestRingsEmptyAndUnboundedRequests(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, q := range []RhythmQuery{{Timezone: "UTC"}, {Timezone: "Local", SessionQuery: SessionQuery{Project: "p"}}, {Timezone: "UTC", SessionQuery: SessionQuery{Project: "p", Limit: 100}}} {
		if _, err := s.Rings(ctx, q); err == nil {
			t.Fatal(q)
		}
	}
	got, err := s.Rings(ctx, RhythmQuery{Timezone: "UTC", SessionQuery: SessionQuery{UnassignedProject: true}})
	if err != nil || got.Sessions != 0 || len(got.Weeks) != 0 {
		t.Fatal(got, err)
	}
}
