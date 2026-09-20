package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in, query-only measurement. Do not print tool names or chat text.
func TestReadOnlyAgentToolsMeasurement(t *testing.T) {
	path := os.Getenv("AMC_TOOL_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no diagnostic ledger selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute ledger path required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var id, agent string
	var count, head int64
	err = db.QueryRowContext(ctx, `SELECT q.id,a.agent_id,a.events FROM query_event_agents a JOIN query_sessions q ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation AND q.projection_revision=a.projection_revision ORDER BY a.events DESC LIMIT 1`).Scan(&id, &agent, &count)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM events").Scan(&head); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		started := time.Now()
		rows, err := db.QueryContext(ctx, agentToolsSQL, id, agent, head, 6, 0)
		if err != nil {
			t.Fatal(err)
		}
		groups := 0
		for rows.Next() {
			var name string
			var calls int64
			if err = rows.Scan(&name, &calls); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			groups++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("sample=%d agentEvents=%d groupsReturned=%d elapsed=%s", i+1, count, groups, time.Since(started).Round(time.Millisecond))
	}
}
