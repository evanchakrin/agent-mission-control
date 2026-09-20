package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in, read-only inventory of organization dependencies before identity
// reconciliation. Counts are not permission to merge or discard audit records.
// This does not open Store, initialize schemas, or copy source history.
func TestReadOnlyDuplicateOrganizationDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_DUPLICATE_ORGANIZATION_DB")
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
	var guard int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&guard); err != nil || guard != 1 {
		t.Fatal(guard, err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const duplicateCTE = `WITH duplicate_keys AS (
 SELECT machine_id,provider,native_id FROM query_sessions WHERE native_id<>''
 GROUP BY machine_id,provider,native_id HAVING COUNT(*)>1
), duplicate_sessions AS (
 SELECT q.id FROM query_sessions q JOIN duplicate_keys d
 ON q.machine_id=d.machine_id AND q.provider=d.provider AND q.native_id=d.native_id
) `
	var groups, sessions int64
	if err := tx.QueryRowContext(ctx, duplicateCTE+`SELECT (SELECT COUNT(*) FROM duplicate_keys),(SELECT COUNT(*) FROM duplicate_sessions)`).Scan(&groups, &sessions); err != nil {
		t.Fatal(err)
	}
	t.Logf("duplicate identity groups=%d session rows=%d", groups, sessions)
	// Fixed names only. Never scan event/usage payloads for an organization check.
	for _, table := range []string{"session_metadata", "agent_names", "metadata_operations", "organization_audit", "legacy_aliases"} {
		var linkedRows, linkedSessions int64
		if err := tx.QueryRowContext(ctx, duplicateCTE+`SELECT COUNT(*),COUNT(DISTINCT session_id) FROM `+table+` WHERE session_id IN (SELECT id FROM duplicate_sessions)`).Scan(&linkedRows, &linkedSessions); err != nil {
			t.Fatal(table, err)
		}
		t.Logf("%s linked rows=%d affected sessions=%d", table, linkedRows, linkedSessions)
	}
}
