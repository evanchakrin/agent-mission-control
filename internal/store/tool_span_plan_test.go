package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestToolCallPagesUseOrderedPartialIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+eventQuerySQL(toolCallWhere, false), "session", 0, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "events_tool_call_page") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatal("tool page scans unrelated events or sorts whole history", plan.String())
	}
}

func TestToolSpansBehindHundredThousandOrdinaryRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic sparse-history diagnostic")
	}
	s := openTestStore(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{{ID: "seed", AgentID: "main", Kind: "source-record", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100001)
 INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision)
 SELECT printf('ordinary-%d',n.i),e.session_id,e.source_id,e.generation,e.agent_id,e.kind,e.timestamp,0,2,'ordinary','{}','',e.projection_revision FROM n CROSS JOIN events e WHERE e.id='seed'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tool-call", "tool-result"} {
		_, err = s.db.ExecContext(ctx, `INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision)
 SELECT ?,session_id,source_id,generation,agent_id,?,timestamp,0,2,'tool','{"toolUseId":"sparse"}','',projection_revision FROM events WHERE id='seed'`, kind, kind)
		if err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	page, err := s.ToolSpans(ctx, "session-1", 0, 100, "")
	elapsed := time.Since(started)
	if err != nil || len(page.Spans) != 1 || page.Spans[0].State != "matched" || page.Spans[0].Call.Sequence <= 100001 || page.NextSequence != 0 {
		t.Fatal(page, err)
	}
	t.Logf("sparse history: 100001 ordinary records before tool call, page=%s, spans=%d", elapsed, len(page.Spans))
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100), kinds(kind) AS (VALUES('tool-call'),('tool-result'))
 INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision)
 SELECT printf('dense-%d-%s',n.i,k.kind),e.session_id,e.source_id,e.generation,e.agent_id,k.kind,e.timestamp,0,2,'tool',json_object('toolUseId',printf('dense-%d',n.i)),'',e.projection_revision FROM n CROSS JOIN kinds k CROSS JOIN events e WHERE e.id='seed' ORDER BY n.i,k.kind`)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	page, err = s.ToolSpans(ctx, "session-1", 0, 100, "")
	elapsed = time.Since(started)
	if err != nil || len(page.Spans) != 100 || page.NextSequence == 0 {
		t.Fatal(page, err)
	}
	for _, span := range page.Spans {
		if span.State != "matched" {
			t.Fatal(span)
		}
	}
	next, err := s.ToolSpans(ctx, "session-1", page.NextSequence, 100, page.Snapshot)
	if err != nil || len(next.Spans) != 1 || next.NextSequence != 0 {
		t.Fatal(next, err)
	}
	t.Logf("100 matched spans: page=%s, remaining=%d", elapsed, len(next.Spans))
}
