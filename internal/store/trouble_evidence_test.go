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

func TestTroubleSparseEvidencePreservesUnknownAndFailures(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	type outcome struct {
		id             string
		bad, uncertain int
	}
	cases := []struct {
		name           string
		extra          []Event
		bad, uncertain int
	}{
		{"success", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":false}`)}}, 0, 0},
		{"failure", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":true}`)}}, 1, 0},
		{"missing-result-status", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit"}`)}}, 0, 1},
		{"non-boolean-status", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":"false"}`)}}, 0, 1},
		{"numeric-zero-status", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":0}`)}}, 0, 1},
		{"numeric-one-status", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":1}`)}}, 0, 1},
		{"non-tool-shared-id", []Event{{Kind: "user-text", Data: json.RawMessage(`{"toolUseId":"edit","error":false}`)}}, 0, 0},
		{"orphan", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"other","error":false}`)}}, 0, 1},
		{"duplicate-call", []Event{{Kind: "tool-call", Data: json.RawMessage(`{"toolUseId":"edit"}`)}}, 0, 1},
		{"duplicate-result", []Event{{Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":false}`)}, {Kind: "tool-result", Data: json.RawMessage(`{"toolUseId":"edit","error":false}`)}}, 0, 1},
		{"indexing-error", []Event{{Kind: "indexing-error", Data: json.RawMessage(`{}`)}}, 0, 1},
		{"non-tool-error", []Event{{Kind: "provider-error", Data: json.RawMessage(`{"error":true}`)}}, 1, 0},
		{"old-pending", nil, 0, 0},
	}
	want := []outcome{}
	for i, tc := range cases {
		src := testSource()
		src.SourceID = fmt.Sprintf("sparse-source-%02d", i)
		src.ModifiedAt = now.Add(-time.Hour)
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = tc.name
		b.Events = []Event{{ID: fmt.Sprintf("sparse-call-%02d", i), AgentID: "main", Kind: "tool-call", Timestamp: src.ModifiedAt, SourceLength: 2, SearchText: `{"file_path":"same.go"}`, Data: json.RawMessage(`{"tool":"Edit","toolUseId":"edit"}`)}}
		for j, event := range tc.extra {
			event.ID = fmt.Sprintf("sparse-extra-%02d-%02d", i, j)
			event.AgentID = "main"
			event.Timestamp = src.ModifiedAt
			event.SourceLength = 2
			b.Events = append(b.Events, event)
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		want = append(want, outcome{tc.name, tc.bad, tc.uncertain})
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	// Retained evidence from other source identities/generations/revisions must
	// not join current calls, even when the session, agent and call ID match.
	for _, column := range []string{"source_id", "generation", "projection_revision"} {
		columns := []string{"source_id", "generation", "projection_revision"}
		for i := range columns {
			if columns[i] == column {
				columns[i] = column + "||'-retained'"
			}
		}
		result, err := s.db.ExecContext(ctx, `INSERT INTO events(id,session_id,source_id,generation,projection_revision,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key)
 SELECT id||'-`+column+`',session_id,`+strings.Join(columns, ",")+`,agent_id,kind,timestamp,source_offset,raw_length,text,'{"toolUseId":"edit","error":true}',''
 FROM events WHERE session_id='success' AND kind='tool-result' AND id LIKE 'sparse-extra-%' AND source_id NOT LIKE '%-retained' AND generation NOT LIKE '%-retained' AND projection_revision NOT LIKE '%-retained'`)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("retained %s fixture: rows=%d err=%v", column, count, err)
		}
	}
	assertTroubleGroupingEvidence(t, s, now)
	baseline := `WITH trouble_scope AS NOT MATERIALIZED (SELECT * FROM query_sessions),` + referenceTroubleEvidenceBody
	for _, filter := range []string{
		" WHERE p.call_seq>0 OR p.uncertain=1",
		" WHERE error_type='true' OR kind='indexing-error' OR kind='tool-result' AND COALESCE(error_type,'') NOT IN ('true','false')",
	} {
		if strings.Count(baseline, filter) != 1 {
			t.Fatal("baseline filter anchor changed", filter)
		}
		baseline = strings.Replace(baseline, filter, "", 1)
	}
	read := func(query string) map[string]outcome {
		t.Helper()
		at := stamp(now)
		rows, err := s.db.QueryContext(ctx, query+`SELECT session_id,bad,uncertain FROM touches ORDER BY session_id`, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]outcome{}
		for rows.Next() {
			var item outcome
			if err := rows.Scan(&item.id, &item.bad, &item.uncertain); err != nil {
				t.Fatal(err)
			}
			out[item.id] = item
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got, previous := read(troubleEvidenceSQL), read(baseline)
	if !reflect.DeepEqual(got, previous) {
		t.Fatal("sparse evidence changed outcomes", got, previous)
	}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for _, expected := range want {
		if got[expected.id] != expected {
			t.Errorf("%s: got %+v, want %+v", expected.id, got[expected.id], expected)
		}
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+troubleEvidenceSQL+`SELECT machine_id,project,path,COUNT(*),SUM(bad) FROM touches GROUP BY machine_id,project,path`, stamp(now), stamp(now), stamp(now), stamp(now), stamp(now))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
