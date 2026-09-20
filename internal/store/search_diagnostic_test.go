package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in diagnostic: never initialize or migrate the selected ledger.
// Reads are query-only, deadline bounded and emit no transcript text.
func TestReadOnlySearchDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_SEARCH_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("explicit read-only diagnostic ledger not selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("diagnostic requires an absolute ledger path")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocked, err := db.QueryContext(ctx, `SELECT w.source_id,w.ready_at,s.durable_offset,s.indexed_offset,w.error FROM index_work w JOIN sources s USING(source_id,generation) WHERE w.state='blocked' LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	for blocked.Next() {
		var id, retry, problem string
		var durable, indexed int64
		if err = blocked.Scan(&id, &retry, &durable, &indexed, &problem); err != nil {
			t.Fatal(err)
		}
		t.Log("blocked source", id, "retry", retry, "durable", durable, "indexed", indexed, "problem", problem)
	}
	err = blocked.Err()
	blocked.Close()
	if err != nil {
		t.Fatal(err)
	}
	columns := `e.seq,e.id,e.session_id,e.agent_id,e.kind,e.timestamp,e.source_offset,e.raw_length,e.text,e.data,e.dedupe_key,e.projection_revision,e.source_id,e.generation`
	active := `i.id=e.session_id AND i.source_id=e.source_id AND i.generation=e.generation AND i.projection_revision=e.projection_revision`
	queries := []struct{ name, sql string }{
		{"current", `SELECT ` + columns + ` FROM events e JOIN query_sessions i ON ` + active + ` JOIN events_fts ON events_fts.rowid=e.seq WHERE events_fts MATCH ? AND e.seq>? ORDER BY e.seq LIMIT ?`},
		{"fts-first", `SELECT ` + columns + ` FROM events_fts CROSS JOIN events e ON e.seq=events_fts.rowid CROSS JOIN query_sessions i ON ` + active + ` WHERE events_fts MATCH ? AND events_fts.rowid>? ORDER BY events_fts.rowid LIMIT ?`},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			plan, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q.sql, `"error"`, 0, 21)
			if err != nil {
				t.Fatal(err)
			}
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err = plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Log(detail)
			}
			err = plan.Err()
			plan.Close()
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			rows, err := db.QueryContext(ctx, q.sql, `"error"`, 0, 21)
			if err != nil {
				t.Log("query failed", time.Since(start), err)
				return
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				count++
			}
			t.Log("rows", count, "elapsed", time.Since(start), "error", rows.Err())
		})
	}
}
