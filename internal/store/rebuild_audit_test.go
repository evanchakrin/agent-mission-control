package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An explicitly selected candidate ledger, opened without migrations or writes.
// This checks publication invariants, not whether provider accounting is correct.
func TestReadOnlyCompletedRebuildAudit(t *testing.T) {
	path, version := os.Getenv("AMC_REBUILD_AUDIT_DB"), os.Getenv("AMC_REBUILD_AUDIT_VERSION")
	if path == "" || version == "" {
		t.Skip("candidate ledger and expected parser version not selected")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("absolute ledger path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	checks := []struct {
		name, query string
		args        []any
	}{
		{"unfinished_rebuilds", `SELECT count(*) FROM projection_revisions WHERE state IN ('building','verifying','ready','blocked')`, nil},
		{"invalid_active_references", `SELECT count(*) FROM active_projection a LEFT JOIN projection_revisions r ON r.revision=a.revision WHERE r.revision IS NULL OR r.state!='active' OR r.source_id!=a.source_id OR r.generation!=a.generation OR r.parser_version!=?`, []any{version}},
		{"publication_offset_mismatch", `SELECT count(*) FROM active_projection a JOIN projection_revisions r ON r.revision=a.revision JOIN sources s ON s.source_id=a.source_id AND s.generation=a.generation WHERE r.indexed_offset!=s.indexed_offset OR s.indexed_offset>s.durable_offset OR r.target_offset>s.durable_offset`, nil},
		{"current_checkpoint_mismatch", `SELECT count(*) FROM sources s JOIN source_identity i ON i.source_id=s.source_id AND i.active_generation=s.generation WHERE s.indexed_offset>0 AND NOT EXISTS(SELECT 1 FROM properties p WHERE p.key='external_index:'||s.source_id AND p.value='true') AND CASE WHEN json_valid(s.parser_state) THEN COALESCE(json_extract(s.parser_state,'$.version')!=?,1) ELSE 1 END`, []any{version}},
	}
	for _, check := range checks {
		var n int64
		if err = tx.QueryRowContext(ctx, check.query, check.args...).Scan(&n); err != nil {
			t.Fatal(check.name, err)
		}
		if n != 0 {
			t.Fatalf("%s=%d", check.name, n)
		}
		t.Logf("%s=0", check.name)
	}
	var sources, active int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM source_identity").Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM active_projection").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if sources == 0 || active == 0 {
		t.Fatal("empty catalog cannot certify rebuild publication")
	}
	t.Logf("snapshot sources=%d publishedRevisions=%d expectedParser=%s", sources, active, version)
}
