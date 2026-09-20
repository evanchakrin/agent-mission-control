package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const referenceTroubleEvidenceBody = `current_events AS (
 SELECT e.seq,e.session_id,e.agent_id,e.kind,json_extract(e.data,'$.toolUseId') AS call_id,json_type(e.data,'$.error') AS error_type FROM events e JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision),
 pairs AS (SELECT session_id,agent_id,call_id,
 SUM(kind='tool-call') AS calls,SUM(kind='tool-result') AS results,
 MAX(CASE WHEN kind='tool-call' THEN seq ELSE 0 END) AS call_seq,
 MAX(CASE WHEN kind='tool-result' THEN seq ELSE 0 END) AS result_seq
 FROM current_events WHERE kind IN ('tool-call','tool-result') GROUP BY session_id,agent_id,call_id),
 pending AS (SELECT session_id,agent_id,MAX(CASE WHEN calls=1 AND results=0 AND typeof(call_id)='text' AND call_id<>'' AND agent_id<>'' THEN call_seq ELSE 0 END) AS call_seq,
 MAX(calls<>1 OR results>1 OR typeof(call_id)<>'text' OR call_id='' OR agent_id='' OR results=1 AND result_seq<=call_seq) AS uncertain
 FROM pairs GROUP BY session_id,agent_id),
 stalls AS (SELECT p.session_id,MAX(CASE WHEN p.call_seq>0 AND julianday(e.timestamp)<julianday(?)-2.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?)-10.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))<=julianday(?) THEN 1 ELSE 0 END) AS stalled,
 MAX(p.uncertain OR p.call_seq>0 AND (julianday(e.timestamp) IS NULL OR julianday(e.timestamp)>julianday(?) OR julianday(json_extract(so.meta_json,'$.modifiedAt')) IS NULL OR julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?))) AS uncertain
 FROM pending p JOIN trouble_scope q ON q.id=p.session_id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation LEFT JOIN events e ON e.seq=p.call_seq WHERE p.call_seq>0 OR p.uncertain=1 GROUP BY p.session_id),
 outcomes AS (SELECT session_id,MAX(error_type='true') AS failed,
 MAX(kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false')) AS uncertain FROM current_events WHERE error_type='true' OR kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false') GROUP BY session_id),
 touches AS (SELECT DISTINCT q.id AS session_id,q.machine_id,s.project,f.path,COALESCE(NULLIF(json_extract(so.meta_json,'$.modifiedAt'),''),q.last_activity) AS last_activity,
 CASE WHEN COALESCE(o.failed,0)=1 OR COALESCE(st.stalled,0)=1 THEN 1 ELSE 0 END AS bad,
 CASE WHEN COALESCE(o.uncertain,0)=1 OR COALESCE(st.uncertain,0)=1 OR c.indexed_offset IS NULL OR c.indexed_offset<>so.indexed_offset OR c.diagnostics>0 THEN 1 ELSE 0 END AS uncertain
 FROM file_edit_events f JOIN events e ON e.seq=f.event_seq
 JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=f.source_id AND q.generation=f.generation AND q.projection_revision=f.revision
 JOIN sessions s ON s.id=q.id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation
 LEFT JOIN file_edit_checkpoints c ON c.source_id=f.source_id AND c.generation=f.generation AND c.revision=f.revision
 LEFT JOIN outcomes o ON o.session_id=q.id LEFT JOIN stalls st ON st.session_id=q.id)
`

const experimentalTroubleEvidenceBody = `current_events AS (
 SELECT e.seq,e.session_id,e.agent_id,e.kind,json_extract(e.data,'$.toolUseId') AS call_id,json_type(e.data,'$.error') AS error_type FROM events e JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=e.source_id AND q.generation=e.generation AND q.projection_revision=e.projection_revision),
 pairs AS MATERIALIZED (SELECT session_id,agent_id,call_id,
 SUM(kind='tool-call') AS calls,SUM(kind='tool-result') AS results,
 MAX(CASE WHEN kind='tool-call' THEN seq ELSE 0 END) AS call_seq,
 MAX(CASE WHEN kind='tool-result' THEN seq ELSE 0 END) AS result_seq,
 MAX(error_type='true') AS failed,
 MAX(kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false')) AS outcome_uncertain
 FROM current_events GROUP BY session_id,agent_id,call_id),
 pending AS (SELECT session_id,agent_id,MAX(CASE WHEN calls=1 AND results=0 AND typeof(call_id)='text' AND call_id<>'' AND agent_id<>'' THEN call_seq ELSE 0 END) AS call_seq,
 MAX(calls<>1 OR results>1 OR typeof(call_id)<>'text' OR call_id='' OR agent_id='' OR results=1 AND result_seq<=call_seq) AS uncertain
 FROM pairs WHERE calls>0 OR results>0 GROUP BY session_id,agent_id),
 stalls AS (SELECT p.session_id,MAX(CASE WHEN p.call_seq>0 AND julianday(e.timestamp)<julianday(?)-2.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?)-10.0/1440 AND julianday(json_extract(so.meta_json,'$.modifiedAt'))<=julianday(?) THEN 1 ELSE 0 END) AS stalled,
 MAX(p.uncertain OR p.call_seq>0 AND (julianday(e.timestamp) IS NULL OR julianday(e.timestamp)>julianday(?) OR julianday(json_extract(so.meta_json,'$.modifiedAt')) IS NULL OR julianday(json_extract(so.meta_json,'$.modifiedAt'))>julianday(?))) AS uncertain
 FROM pending p JOIN trouble_scope q ON q.id=p.session_id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation LEFT JOIN events e ON e.seq=p.call_seq WHERE p.call_seq>0 OR p.uncertain=1 GROUP BY p.session_id),
 outcomes AS (SELECT session_id,MAX(failed) AS failed,MAX(outcome_uncertain) AS uncertain FROM pairs GROUP BY session_id),
 touches AS (SELECT DISTINCT q.id AS session_id,q.machine_id,s.project,f.path,COALESCE(NULLIF(json_extract(so.meta_json,'$.modifiedAt'),''),q.last_activity) AS last_activity,
 CASE WHEN COALESCE(o.failed,0)=1 OR COALESCE(st.stalled,0)=1 THEN 1 ELSE 0 END AS bad,
 CASE WHEN COALESCE(o.uncertain,0)=1 OR COALESCE(st.uncertain,0)=1 OR c.indexed_offset IS NULL OR c.indexed_offset<>so.indexed_offset OR c.diagnostics>0 THEN 1 ELSE 0 END AS uncertain
 FROM file_edit_events f JOIN events e ON e.seq=f.event_seq
 JOIN trouble_scope q ON q.id=e.session_id AND q.source_id=f.source_id AND q.generation=f.generation AND q.projection_revision=f.revision
 JOIN sessions s ON s.id=q.id JOIN sources so ON so.source_id=q.source_id AND so.generation=q.generation
 LEFT JOIN file_edit_checkpoints c ON c.source_id=f.source_id AND c.generation=f.generation AND c.revision=f.revision
 LEFT JOIN outcomes o ON o.session_id=q.id LEFT JOIN stalls st ON st.session_id=q.id)
`

// Current scope admits only one source/generation/revision per session. Adding
// those identity columns to GROUP BY preserves groups while matching the
// existing events_tool_match index order; no new index or write cost is added.
var alignedTroubleEvidenceBody = strings.Replace(strings.Replace(referenceTroubleEvidenceBody,
	"SELECT e.seq,e.session_id,e.agent_id", "SELECT e.seq,e.session_id,e.source_id,e.generation,e.projection_revision,e.agent_id", 1),
	"GROUP BY session_id,agent_id,call_id", "GROUP BY session_id,source_id,generation,projection_revision,agent_id,call_id", 1)

func assertTroubleGroupingEvidence(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	at := stamp(now)
	read := func(body string) []string {
		rows, err := s.db.QueryContext(context.Background(), `WITH trouble_scope AS NOT MATERIALIZED (SELECT * FROM query_sessions),`+body+`SELECT session_id,machine_id,project,path,last_activity,bad,uncertain FROM touches ORDER BY session_id,machine_id,project,path`, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var session, machine, project, path, activity string
			var bad, unknown int64
			if err := rows.Scan(&session, &machine, &project, &path, &activity, &bad, &unknown); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal([]any{session, machine, project, path, activity, bad, unknown})
			result = append(result, string(raw))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if a, b := read(referenceTroubleEvidenceBody), read(experimentalTroubleEvidenceBody); !reflect.DeepEqual(a, b) {
		t.Fatalf("grouping changed trouble evidence: %v vs %v", a, b)
	}
	if a, b := read(referenceTroubleEvidenceBody), read(alignedTroubleEvidenceBody); !reflect.DeepEqual(a, b) {
		t.Fatalf("index-aligned grouping changed evidence: %v vs %v", a, b)
	}
	for _, variant := range []struct{ name, body string }{{"reference", referenceTroubleEvidenceBody}, {"index-aligned", alignedTroubleEvidenceBody}} {
		rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN WITH trouble_scope AS NOT MATERIALIZED (SELECT * FROM query_sessions),"+variant.body+"SELECT COUNT(*) FROM touches", at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		temporary := 0
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				temporary++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s temporary B-tree operations=%d", variant.name, temporary)
	}
}

func testTroubleGroupingComparison(t *testing.T, s *Store, now time.Time) {
	at := stamp(now)
	var reference []string
	for _, variant := range []struct{ name, body string }{{"reference", referenceTroubleEvidenceBody}, {"index-aligned", alignedTroubleEvidenceBody}, {"index-aligned-repeat", alignedTroubleEvidenceBody}, {"reference-repeat", referenceTroubleEvidenceBody}} {
		// Diagnostic timeout only; preceding owner-query checks still use 10s.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		rows, err := s.db.QueryContext(ctx, `WITH trouble_scope AS NOT MATERIALIZED (SELECT * FROM query_sessions),`+variant.body+`SELECT machine_id,project,path,COUNT(*),SUM(bad),SUM(CASE WHEN uncertain=1 AND bad=0 THEN 1 ELSE 0 END),strftime('%Y-%m-%dT%H:%M:%fZ',MAX(julianday(last_activity))) FROM touches GROUP BY machine_id,project,path ORDER BY COUNT(*) DESC,machine_id COLLATE BINARY,project COLLATE BINARY,path COLLATE BINARY`, at, at, at, at, at)
		if err != nil {
			cancel()
			t.Fatal(variant.name, err)
		}
		var result []string
		var total int64
		for rows.Next() {
			var machine, project, path, activity string
			var count, bad, unknown int64
			if err := rows.Scan(&machine, &project, &path, &count, &bad, &unknown, &activity); err != nil {
				rows.Close()
				cancel()
				t.Fatal(err)
			}
			total += count
			raw, _ := json.Marshal([]any{machine, project, path, count, bad, unknown, activity})
			result = append(result, string(raw))
		}
		err = rows.Err()
		rows.Close()
		cancel()
		t.Logf("100001-session trouble %s SQL=%s; total=%d groups=%d", variant.name, time.Since(start), total, len(result))
		if err != nil {
			t.Fatal(err)
		}
		if total != 100001 {
			t.Fatal("touch history lost", total)
		}
		if reference == nil {
			reference = result
		} else if !reflect.DeepEqual(reference, result) {
			t.Fatal("trouble grouping changed complete output", variant.name)
		}
	}
}
