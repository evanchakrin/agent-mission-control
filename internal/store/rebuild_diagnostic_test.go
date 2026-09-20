package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in, query-only diagnostics. Never initializes a store or prints chat text.
func TestReadOnlyRebuildDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_REBUILD_DIAGNOSTIC_DB")
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
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	revision := os.Getenv("AMC_REBUILD_DIAGNOSTIC_REVISION")
	if revision != "" && !validID(revision) {
		t.Fatal("invalid diagnostic revision")
	}
	rows, err := db.QueryContext(ctx, `SELECT revision,source_id,generation,state,indexed_offset,target_offset,length(parser_state),length(projection),error,retry_at FROM projection_revisions WHERE (state IN('building','verifying','ready') AND error<>'') OR revision=? ORDER BY updated_at DESC LIMIT 10`, revision)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var revision, source, generation, state, problem, retry string
		var indexed, target, parserBytes, projectionBytes int64
		if err = rows.Scan(&revision, &source, &generation, &state, &indexed, &target, &parserBytes, &projectionBytes, &problem, &retry); err != nil {
			t.Fatal(err)
		}
		t.Logf("revision=%s source=%s generation=%s state=%s indexed=%d target=%d parserBytes=%d projectionBytes=%d error=%q retry=%s", revision, source, generation, state, indexed, target, parserBytes, projectionBytes, problem, retry)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
