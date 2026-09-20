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

// Diagnostic experiment only: no production schema/query change. Large session
// projections should not require loading their bodies merely to classify a
// flow's completeness. Compare the exact same complete result set, not a sample.
func TestBehaviorProjectionCoveringIndexExperiment(t *testing.T) {
	if testing.Short() {
		t.Skip("100001-session projection/index comparison")
	}
	runBehaviorProjectionCoveringIndex(t, 100001)
}

func TestBehaviorProjectionCoveringIndexFixture(t *testing.T) {
	runBehaviorProjectionCoveringIndex(t, 101)
}

func runBehaviorProjectionCoveringIndex(t *testing.T, count int) {
	t.Helper()
	s := openTestStore(t, Options{})
	seedStart := time.Now()
	seedAnalyticsCatalog(t, s, count)
	t.Logf("base catalog seeded in %s", time.Since(seedStart))
	ctx := context.Background()
	// Synthetic query evidence, not captured raw history or parser certification.
	// Preserve multiple grouping/outcome branches instead of timing one empty
	// all-unknown group. Source offsets are zero because no bytes were ingested.
	for stage, statement := range []string{
		`UPDATE sessions SET projection=json_set(projection,'$.completeness',CASE WHEN CAST(substr(id,3) AS INTEGER)%5=0 THEN 'indexed-source' ELSE 'unknown' END)`,
		`INSERT INTO source_identity SELECT source_id,machine_id,provider,id,generation FROM sessions`,
		`INSERT INTO sources(source_id,generation,meta_json,created_at,updated_at) SELECT source_id,generation,json_object('machineId',machine_id,'sourceId',source_id,'generation',generation,'provider',provider),'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM sessions`,
		`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'flow-'||id,id,source_id,generation,printf('agent-%d',CAST(substr(id,3) AS INTEGER)%31),'tool-call','2026-01-01T00:00:00Z',0,0,'synthetic flow',json_object('tool',printf('Tool%d',CAST(substr(id,3) AS INTEGER)%7),'toolUseId','call','error',json(CASE WHEN CAST(substr(id,3) AS INTEGER)%11=0 THEN 'true' ELSE 'false' END)),'','' FROM sessions`,
		`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'result-'||id,session_id,source_id,generation,agent_id,'tool-result','2026-01-01T00:00:01Z',0,0,'synthetic result',data,'',projection_revision FROM events WHERE kind='tool-call' AND id LIKE 'flow-%'`,
	} {
		start := time.Now()
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture stage %d completed in %s", stage, time.Since(start))
	}
	// Add padding last: event-maintenance triggers should not repeatedly parse
	// the deliberately inflated bodies during setup. Final query data is equal.
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.fixturePadding',?)`, strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	t.Logf("complete padded fixture prepared in %s", time.Since(seedStart))
	if err := s.ensureBehaviorTools(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(label, statement string) []string {
		request, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		start := time.Now()
		rows, err := s.db.QueryContext(request, fmt.Sprintf(statement, "1=1"))
		if err != nil {
			t.Fatal(label, err)
		}
		defer rows.Close()
		var result []string
		var total int64
		outcomes := map[string]bool{}
		for rows.Next() {
			var fanout, names, outcome, id, activity string
			var count int64
			if err := rows.Scan(&fanout, &names, &outcome, &count, &id, &activity); err != nil {
				t.Fatal(err)
			}
			value, err := json.Marshal([]any{fanout, names, outcome, count, id, activity})
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, string(value))
			total += count
			outcomes[outcome] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(label, err)
		}
		if total != int64(count) {
			t.Fatal("comparison omitted sessions", total)
		}
		for _, outcome := range []string{"reported-errors", "unknown", "no-reported-errors"} {
			if !outcomes[outcome] {
				t.Fatal("fixture did not exercise outcome", outcome)
			}
		}
		t.Logf("%s: %s, groups=%d, sessions=%d", label, time.Since(start), len(result), total)
		return result
	}
	want := read("original-before-index", behaviorPatternsSQL)
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX experiment_behavior_projection ON sessions(id,COALESCE(json_extract(projection,'$.completeness'),'unknown'),json_extract(projection,'$.pricing.snapshotId'))`); err != nil {
		t.Fatal(err)
	}
	variant := strings.Replace(behaviorPatternsSQL, "JOIN sessions s ON s.id=q.id", "JOIN sessions s INDEXED BY experiment_behavior_projection ON s.id=q.id", 1)
	if variant == behaviorPatternsSQL {
		t.Fatal("experiment did not select its index")
	}
	for _, statement := range []struct{ label, sql string }{{"explicit-covering-index", variant}, {"original-after-index", behaviorPatternsSQL}, {"explicit-covering-index-repeat", variant}} {
		if got := read(statement.label, statement.sql); !reflect.DeepEqual(got, want) {
			t.Fatal("index changed complete grouping evidence", statement.label)
		}
	}
}
