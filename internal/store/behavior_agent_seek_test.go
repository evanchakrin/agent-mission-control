package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBehaviorAgentSeeksPreservePatternsAndUnknownIdentity(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	for i, agent := range []string{"main", "main", "helper", "Ω", "", "wrong-source", "wrong-revision"} {
		source, revision := "source-000000", ""
		if agent == "wrong-source" {
			source = "other"
		}
		if agent == "wrong-revision" {
			revision = "other"
		}
		if _, err := s.db.Exec(`INSERT INTO query_usage VALUES(?,'s-000000',?,'g1',?,?,?,'token','2026-01-01',1,0,0,0)`, fmt.Sprint("seek-", i), source, revision, agent, fmt.Sprint("model-", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO query_event_agents VALUES('s-000000','source-000000','g1','','event-only',1,0,0,0,0,'','');
 INSERT INTO query_event_agents VALUES('s-000001','source-000001','g1','','',1,0,0,0,0,'','')`); err != nil {
		t.Fatal(err)
	}
	oldTeam := `identities AS (SELECT session_id,agent_id FROM current_agents UNION SELECT u.session_id,u.agent_id FROM query_usage u JOIN selected q ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision),
 team AS (SELECT session_id,SUM(agent_id<>'') AS agents,MAX(agent_id='') AS unattributed FROM identities GROUP BY session_id)`
	query := fmt.Sprintf(behaviorPatternsSQL, "1")
	read := func(query string) []string {
		t.Helper()
		rows, err := s.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var results []string
		for rows.Next() {
			var fanout, tools, outcome, id, at string
			var count int64
			if err := rows.Scan(&fanout, &tools, &outcome, &count, &id, &at); err != nil {
				t.Fatal(err)
			}
			results = append(results, fmt.Sprint(fanout, "|", tools, "|", outcome, "|", count, "|", id, "|", at))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return results
	}
	before := read(strings.Replace(query, behaviorTeamSQL, oldTeam, 1))
	after := read(query)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("patterns changed", before, after)
	}
	if !strings.Contains(strings.Join(after, "\n"), "incomplete-attribution") {
		t.Fatal("unknown identity omitted", after)
	}
}
