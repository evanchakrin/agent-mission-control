package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Retain the pre-change SQL as a differential reference. The production query
// groups counts/latest activity, then selects the binary-smallest tied ID.
func groupedBehaviorPatternsSQL() string { return behaviorPatternsSQL }

const windowBehaviorPatternsSQL = `WITH selected AS MATERIALIZED (SELECT q.id,q.last_activity,q.source_id,q.generation,q.projection_revision,COALESCE(json_extract(s.projection,'$.completeness'),'unknown') AS completeness FROM query_sessions q JOIN sessions s ON s.id=q.id WHERE %s),
 current_agents AS MATERIALIZED (SELECT a.session_id,a.agent_id,a.events,a.errors,a.unknown_results,a.indexing_errors FROM query_event_agents a JOIN selected q ON a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision),
 identities AS (SELECT session_id,agent_id FROM current_agents UNION SELECT u.session_id,u.agent_id FROM query_usage u JOIN selected q ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision),
 team AS (SELECT session_id,SUM(agent_id<>'') AS agents,MAX(agent_id='') AS unattributed FROM identities GROUP BY session_id),
 outcomes AS (SELECT session_id,SUM(events) AS events,SUM(errors) AS errors,SUM(unknown_results)+SUM(indexing_errors) AS unknowns FROM current_agents GROUP BY session_id),
 tools AS (SELECT q.id,e.tool AS name,e.calls FROM selected q JOIN query_flow_tools_v1 e ON e.session_id=q.id AND e.source_id=q.source_id AND e.generation=q.generation AND e.projection_revision=q.projection_revision),
 ranked_tools AS (SELECT *,ROW_NUMBER() OVER(PARTITION BY id ORDER BY calls DESC,name COLLATE BINARY) AS position FROM tools),
 tool_lists AS (SELECT id,json_group_array(name ORDER BY position) AS names FROM ranked_tools WHERE position<=3 GROUP BY id),
 signatures AS (SELECT q.id,q.last_activity,
 CASE WHEN COALESCE(t.unattributed,0)>0 THEN 'incomplete-attribution' WHEN COALESCE(t.agents,0)=0 THEN 'unobserved' WHEN t.agents=1 THEN 'solo' WHEN t.agents<=5 THEN 'small-team' WHEN t.agents<=30 THEN 'team' ELSE 'fleet' END AS fanout,
 COALESCE(l.names,'[]') AS names,
 CASE WHEN COALESCE(o.errors,0)>0 THEN 'reported-errors' WHEN COALESCE(o.events,0)=0 THEN 'no-indexed-events' WHEN COALESCE(o.unknowns,0)>0 OR q.completeness NOT IN ('complete','indexed-source') OR so.source_id IS NULL OR so.indexed_offset<so.durable_offset THEN 'unknown' ELSE 'no-reported-errors' END AS outcome
 FROM selected q LEFT JOIN team t ON t.session_id=q.id LEFT JOIN outcomes o ON o.session_id=q.id LEFT JOIN tool_lists l ON l.id=q.id LEFT JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation),
 ranked AS (SELECT *,COUNT(*) OVER(PARTITION BY fanout,names,outcome) AS sessions,ROW_NUMBER() OVER(PARTITION BY fanout,names,outcome ORDER BY last_activity DESC,id COLLATE BINARY) AS position FROM signatures)
 SELECT fanout,names,outcome,sessions,id,last_activity FROM ranked WHERE position=1 ORDER BY sessions DESC,fanout COLLATE BINARY,names COLLATE BINARY,outcome COLLATE BINARY`

func assertGroupedPatternEvidence(t *testing.T, s *Store, q SessionQuery) {
	t.Helper()
	where, args, err := catalogWhere(q)
	if err != nil {
		t.Fatal(err)
	}
	read := func(statement string) []string {
		rows, err := s.db.QueryContext(context.Background(), fmt.Sprintf(statement, where), args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var fanout, names, outcome, id, activity string
			var count int64
			if err := rows.Scan(&fanout, &names, &outcome, &count, &id, &activity); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal([]any{fanout, names, outcome, count, id, activity})
			result = append(result, string(raw))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if a, b := read(windowBehaviorPatternsSQL), read(groupedBehaviorPatternsSQL()); !reflect.DeepEqual(a, b) {
		t.Fatalf("grouped pattern evidence differs: %v vs %v", a, b)
	}
}

func TestPatternGroupingManyDistinctGroups(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1001)
	_, err := s.db.Exec(`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision)
 SELECT 'call-'||id,id,source_id,generation,'main','tool-call','2026-01-01T00:00:00Z',0,0,'synthetic',json_object('tool',printf('Tool%04d',CAST(substr(id,3) AS INTEGER)%997)),'','' FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ensureBehaviorTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	assertGroupedPatternEvidence(t, s, SessionQuery{})
	t.Logf("1001-session many-group differential check: %s", time.Since(start))
}

// Diagnostic only: the normal request deadlines and release targets remain
// unchanged. Run separately so an earlier Trouble Files failure cannot mask it.
func testPatternGroupingExperiment(t *testing.T, s *Store) {
	ctx := context.Background()
	if err := s.ensureAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureBehaviorTools(ctx); err != nil {
		t.Fatal(err)
	}
	var expected []string
	for _, variant := range []struct{ name, sql string }{{"window-reference", windowBehaviorPatternsSQL}, {"grouped-experiment", groupedBehaviorPatternsSQL()}, {"grouped-experiment-repeat", groupedBehaviorPatternsSQL()}, {"window-reference-repeat", windowBehaviorPatternsSQL}} {
		request, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		rows, err := s.db.QueryContext(request, fmt.Sprintf(variant.sql, "1=1"))
		if err != nil {
			cancel()
			t.Fatal(variant.name, err)
		}
		var result []string
		var total int64
		for rows.Next() {
			var fanout, names, outcome, id, activity string
			var count int64
			if err := rows.Scan(&fanout, &names, &outcome, &count, &id, &activity); err != nil {
				rows.Close()
				cancel()
				t.Fatal(err)
			}
			total += count
			raw, _ := json.Marshal([]any{fanout, names, outcome, count, id, activity})
			result = append(result, string(raw))
		}
		err = rows.Err()
		rows.Close()
		cancel()
		t.Logf("diagnostic %s: %s; groups=%d sessions=%d", variant.name, time.Since(start), len(result), total)
		if err != nil {
			t.Fatal(variant.name, err)
		}
		if total != 100001 {
			t.Fatal("history lost", total)
		}
		if expected == nil {
			expected = result
		} else if !reflect.DeepEqual(expected, result) {
			t.Fatal("different grouped evidence", variant.name)
		}
	}
}
