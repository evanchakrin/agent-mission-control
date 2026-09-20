package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit, bounded, query-only inspection; no initialization or transcript text.
func TestReadOnlyAccountingDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_ACCOUNTING_DIAGNOSTIC_DB")
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
	rows, err := db.QueryContext(ctx, `SELECT i.source_id,json_extract(s.meta_json,'$.path'),json_extract(s.meta_json,'$.nativeId'),json_extract(s.parser_state,'$.nativeId'),s.indexed_offset
	FROM source_identity i JOIN sources s ON s.source_id=i.source_id AND s.generation=i.active_generation
	WHERE json_extract(s.parser_state,'$.nativeId') IN (
	 SELECT json_extract(parser_state,'$.nativeId') FROM sources WHERE json_extract(parser_state,'$.nativeId') IS NOT NULL
	 GROUP BY json_extract(parser_state,'$.nativeId') HAVING count(*)>1 ORDER BY count(*) DESC LIMIT 1)
	ORDER BY i.source_id LIMIT 12`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var path, native, parsed sql.NullString
		var offset int64
		if err := rows.Scan(&id, &path, &native, &parsed, &offset); err != nil {
			t.Fatal(err)
		}
		t.Logf("source=%s path=%s catalogNative=%s parsedNative=%s indexed=%d", id, path.String, native.String, parsed.String, offset)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
