package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Shares the 100001-session query fixture. These directly inserted records are
// intentionally incomplete evidence, not a source-capture or soak certification.
func testBehaviorFlowsWorkload(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'helper-'||id,id,source_id,generation,'helper','tool-call','2026-01-01T00:00:00Z',0,0,'synthetic read','{"tool":"Read"}','','' FROM sessions`,
		`INSERT INTO agent_names SELECT id,'helper',printf('Role %02d',CAST(substr(id,3) AS INTEGER)%31) FROM sessions`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	request, cancel := context.WithTimeout(ctx, 10*time.Second)
	start := time.Now()
	patterns, err := s.BehaviorPatterns(request, SessionQuery{Limit: 20})
	t.Logf("100001-session pattern page=%s", time.Since(start))
	cancel()
	if err != nil {
		t.Fatal("pattern query failed owner deadline", err)
	}
	if patterns.TotalSessions != 100001 || patterns.TotalPatterns != 2 || len(patterns.Patterns) != 2 {
		t.Fatal(patterns)
	}
	var patternSessions int64
	for _, p := range patterns.Patterns {
		patternSessions += p.Sessions
		if p.Fanout != "small-team" || (p.Outcome != "unknown" && p.Outcome != "reported-errors") {
			t.Fatal(p)
		}
	}
	if patternSessions != 100001 {
		t.Fatal("pattern history lost", patternSessions)
	}
	var cursor, snapshot string
	seen := map[string]bool{}
	var appearances, unknown int64
	for pageNumber := 0; pageNumber < 2; pageNumber++ {
		request, cancel = context.WithTimeout(ctx, 10*time.Second)
		start = time.Now()
		page, err := s.BehaviorRoles(request, SessionQuery{Limit: 20, Cursor: cursor})
		t.Logf("100001-session role page %d=%s", pageNumber+1, time.Since(start))
		cancel()
		if err != nil {
			t.Fatal("role query failed owner deadline", err)
		}
		if page.TotalAppearances != 100001 || page.TotalRoles != 31 || len(page.Roles) == 0 || len(page.Roles) > 20 {
			t.Fatal(page)
		}
		if snapshot != "" && snapshot != page.Snapshot {
			t.Fatal("unchanged fixture changed snapshot")
		}
		snapshot = page.Snapshot
		for _, r := range page.Roles {
			if seen[r.Name] || r.Appearances != r.Sessions || r.Machines != 25 || r.KnownCost != nil || r.WithEstimate != 0 {
				t.Fatal("lost grouping or invented cost", r)
			}
			seen[r.Name] = true
			appearances += r.Appearances
			unknown += r.Unknown
		}
		cursor = page.NextCursor
		if (pageNumber == 0) != (cursor != "") {
			t.Fatal("wrong role continuation", pageNumber, cursor)
		}
	}
	if len(seen) != 31 || appearances != 100001 || unknown != 100001 {
		t.Fatal("lost role history or incomplete evidence", len(seen), appearances, unknown)
	}
	// Same database, same records: compare the previous late-only filtering
	// against the pushed-down predicate, including every result field.
	lateSQL := behaviorRolesSQL
	for _, alias := range []string{"a", "u"} {
		predicate := fmt.Sprintf(" WHERE %[1]s.agent_id<>'' AND (%[1]s.agent_id<>'main' OR q.parent_native_id<>'' OR (q.provider='claude' AND q.native_agent_id=%[1]s.agent_id))", alias)
		if strings.Count(lateSQL, predicate) != 1 {
			t.Fatal("reference predicate changed", alias)
		}
		lateSQL = strings.Replace(lateSQL, predicate, "", 1)
	}
	readRows := func(label, statement string) []string {
		t.Helper()
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		start := time.Now()
		rows, err := s.db.QueryContext(request, fmt.Sprintf(statement, "1=1"))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var result []string
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, string(encoded))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("same-fixture %s SQL=%s", label, time.Since(start))
		return result
	}
	late := readRows("late-filter", lateSQL)
	early := readRows("early-filter", behaviorRolesSQL)
	if !reflect.DeepEqual(late, early) {
		t.Fatal("early filtering changed grouped evidence")
	}
	reference := readRows("pattern-window-reference", windowBehaviorPatternsSQL)
	grouped := readRows("pattern-grouped-experiment", groupedBehaviorPatternsSQL())
	if !reflect.DeepEqual(reference, grouped) {
		t.Fatal("grouped pattern experiment changed result fields")
	}
}
