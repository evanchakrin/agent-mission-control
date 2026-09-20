package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestSessionStatsSummaryAndBoundsPreserveRawSemantics(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	if _, err := s.db.Exec(`UPDATE query_usage SET timestamp=''; INSERT INTO query_event_agents VALUES('s-000000','source-000000','g1','','event-only',1,0,0,0,0,'','')`); err != nil {
		t.Fatal(err)
	}
	for i, at := range []string{"", "0001-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z", "2026-02-01T00:00:00.000000000Z"} {
		if _, err := s.db.Exec(`INSERT INTO query_usage SELECT ?,session_id,source_id,generation,projection_revision,?,'model','token',?,0,0,0,0 FROM query_usage WHERE id='usage-s-000000'`, fmt.Sprint("bounds-", i), []string{"", "helper", "helper", "other"}[i], at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO query_usage SELECT 'old-scope',session_id,source_id,'old',projection_revision,'excluded','model','token','9999-01-01T00:00:00.000000000Z',0,0,0,0 FROM query_usage WHERE id='usage-s-000000'; DELETE FROM query_usage WHERE session_id='s-000002'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s-000000", "s-000001", "s-000002"} {
		var oldCount, newCount int
		oldSQL := strings.Replace(sessionAgentCountSQL, "FROM query_agent_usage u", "FROM query_usage u", 1)
		if err := s.db.QueryRow(oldSQL, id, id).Scan(&oldCount); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(sessionAgentCountSQL, id, id).Scan(&newCount); err != nil || oldCount != newCount {
			t.Fatal("agent count changed", id, oldCount, newCount, err)
		}
		var oldFirst, oldLast, newFirst, newLast sql.NullString
		if err := s.db.QueryRow(`SELECT MIN(NULLIF(u.timestamp,'')),MAX(NULLIF(u.timestamp,'')) FROM query_usage u JOIN query_sessions q ON q.id=u.session_id AND q.source_id=u.source_id AND q.generation=u.generation AND q.projection_revision=u.projection_revision WHERE q.id=?`, id).Scan(&oldFirst, &oldLast); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(sessionUsageBoundsSQL, id).Scan(&newFirst, &newLast); err != nil || oldFirst != newFirst || oldLast != newLast {
			t.Fatal("timestamp bounds changed", id, err)
		}
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+sessionUsageBoundsSQL, "s-000000")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seeks := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "query_usage_scope_time") && strings.Contains(detail, "timestamp>?") {
			seeks++
		}
	}
	if err := rows.Err(); err != nil || seeks != 2 {
		t.Fatal("expected two timestamp endpoint seeks", seeks, err)
	}
}
