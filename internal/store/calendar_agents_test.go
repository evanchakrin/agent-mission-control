package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestCalendarAgentSeeksPreserveCompleteScopedUnion(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	for i, agent := range []string{"main", "main", "a", "a", "b", "Ω", "", "wrong-source", "wrong-generation", "wrong-revision"} {
		source, generation, revision := "source-000000", "g1", ""
		if agent == "wrong-source" {
			source = "other"
		}
		if agent == "wrong-generation" {
			generation = "old"
		}
		if agent == "wrong-revision" {
			revision = "old"
		}
		if _, err := s.db.Exec(`INSERT INTO query_usage VALUES(?,'s-000000',?,?,?,?,?,'message-final','2026-01-01',1,0,0,0)`, fmt.Sprintf("extra-%d", i), source, generation, revision, agent, fmt.Sprintf("model-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, agent := range []string{"a", "event-only", ""} {
		if _, err := s.db.Exec(`INSERT INTO query_event_agents VALUES('s-000000','source-000000','g1','',?,1,0,0,0,0,'','')`, agent); err != nil {
			t.Fatal(err)
		}
	}
	old := `(SELECT COUNT(*) FROM (SELECT a.agent_id FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision AND a.agent_id<>'' UNION SELECT u.agent_id FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.agent_id<>''))`
	var before, after int
	if err := s.db.QueryRow(`SELECT `+old+`,`+calendarAgentScopeCount+` FROM query_sessions q WHERE q.id='s-000000'`).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if before != 5 || after != before {
		t.Fatal("agent scopes omitted or multiplied", before, after)
	}
	if !strings.Contains(calendarSQL([]string{"(?,?,?)"}, "1"), calendarAgentScopeCount) {
		t.Fatal("calendar bypasses bounded agent seeks")
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT ` + calendarAgentScopeCount + ` FROM query_sessions q WHERE q.id='s-000000'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(plan, "agent_id>?") < 2 {
		t.Fatal("agent lookup no longer seeks between IDs", plan)
	}
}
