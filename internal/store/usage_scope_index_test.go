package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestUsageScopeIndexUpgradePreservesCalendarAndOrganization(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO query_usage SELECT 'scope-probe',id,source_id,generation,projection_revision,'usage-only','model','token',last_activity,1,0,0,1 FROM query_sessions WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "index-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP INDEX query_usage_session_agent;CREATE INDEX query_usage_session_agent ON query_usage(session_id,generation,projection_revision,agent_id,model,id)`); err != nil {
		t.Fatal(err)
	}
	q := CalendarQuery{Start: "2026-01-01", End: "2026-01-04", Timezone: "UTC"}
	before, err := s.Calendar(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.ensureUsageScopeIndex(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.SetupAnalytics(ctx); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Calendar(ctx, q)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("index upgrade changed calendar", err, before, after)
	}
	var archived int
	if err = s.db.QueryRow(`SELECT archived FROM query_sessions WHERE id='s-000000'`).Scan(&archived); err != nil || archived != 1 {
		t.Fatal("archive changed", err, archived)
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+calendarSQL([]string{"(?,?,?)"}, "1"), "2026-01-01", "2026-01-01T00:00:00Z", "2026-01-04T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	covered := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH u USING COVERING INDEX query_usage_session_agent") {
			covered = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("calendar agent scopes fetch full usage rows")
	}
}
