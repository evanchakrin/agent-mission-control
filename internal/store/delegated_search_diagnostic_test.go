package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in, query-only investigation. Never emits transcript text or migrates data.
func TestReadOnlyDelegatedSearchPlan(t *testing.T) {
	path := os.Getenv("AMC_DELEGATED_DIAGNOSTIC_DB")
	if path == "" {
		t.Skip("no read-only ledger selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute ledger required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	where := `events_fts MATCH ? AND events_fts.rowid>? AND e.kind='tool-call' AND json_extract(e.data,'$.tool') IN ('Task','Agent','spawn_agent','functions.spawn_agent','collaboration.spawn_agent')`
	for _, ranked := range []bool{false, true} {
		q := eventQuerySQL(where, true, ranked)
		q = strings.Replace(q, "events_fts CROSS JOIN events e ON e.seq=events_fts.rowid", "events e INDEXED BY events_tool_call_page CROSS JOIN events_fts ON events_fts.rowid=e.seq", 1)
		if ranked {
			q = strings.Replace(q, "ORDER BY events_fts.rank", "ORDER BY bm25(events_fts)", 1)
		} else {
			q = strings.Replace(q, "ORDER BY events_fts.rowid", "ORDER BY e.seq", 1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		rows, err := db.QueryContext(ctx, q, `"test"`, 0, 6)
		if err != nil {
			cancel()
			t.Errorf("ranked=%t elapsed=%s error=%v", ranked, time.Since(start), err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		cancel()
		t.Logf("call-first ranked=%t rows=%d elapsed=%s error=%v", ranked, n, time.Since(start), err)
		if err != nil {
			t.Error(err)
		}
	}
}
