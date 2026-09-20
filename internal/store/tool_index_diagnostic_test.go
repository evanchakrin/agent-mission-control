package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in real-ledger preflight. Never opens Store, migrates, or emits chat text.
func TestReadOnlyToolIndexDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_TOOL_INDEX_DIAGNOSTIC_DB")
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var queryOnly, pages, pageSize int64
	if err = db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatal("read-only guard missing", err)
	}
	if err = db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger pages=%d pageSize=%d allocatedBytes=%d", pages, pageSize, pages*pageSize)
	var totalEvents int64
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&totalEvents); err != nil {
		t.Fatal(err)
	}
	t.Logf("totalEventRows=%d", totalEvents)
	started := time.Now()
	rows, err := db.QueryContext(ctx, `SELECT kind,COUNT(*),COALESCE(SUM(length(CAST(session_id||source_id||generation||projection_revision AS BLOB))),0),COALESCE(SUM(length(CAST(agent_id||kind AS BLOB))+length(CAST(COALESCE(json_extract(data,'$.toolUseId'),'') AS BLOB))),0)
 FROM (SELECT kind,session_id,source_id,generation,projection_revision,agent_id,data FROM events WHERE kind IN ('tool-call','tool-result') ORDER BY seq LIMIT 4096) GROUP BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	var estimate int64
	var sampled int64
	for rows.Next() {
		var kind string
		var count, scopeBytes, matchBytes int64
		if err = rows.Scan(&kind, &count, &scopeBytes, &matchBytes); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		// Heuristic only: key payload plus 64 bytes per entry, doubled for page
		// slack, with a second key set for the ordered call-only index.
		keyBudget := scopeBytes + matchBytes + count*64
		if kind == "tool-call" {
			keyBudget += scopeBytes + count*64
		}
		estimate += keyBudget * 2
		sampled += count
		t.Logf("kind=%s rows=%d scopeKeyBytes=%d matchKeyBytes=%d", kind, count, scopeBytes, matchBytes)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sampled > 0 {
		estimate = estimate * totalEvents / sampled
	}
	t.Logf("sampleRows=%d allRowsExtrapolatedIndexBytes=%d temporaryAndIndexAllowanceBytes=%d elapsed=%s; bounded sample assumes every event has sampled tool-key sizes, not a guaranteed bound or capacity certification", sampled, estimate, estimate*3, time.Since(started))
}
