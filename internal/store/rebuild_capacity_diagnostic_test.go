package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in, read-only capacity evidence. No migrations, checkpoint, vacuum, pruning
// or source-body reads. Free database pages are reusable, not free volume space.
func TestReadOnlyRebuildCapacity(t *testing.T) {
	path := os.Getenv("AMC_REBUILD_CAPACITY_DB")
	if path == "" {
		t.Skip("no capacity diagnostic ledger selected")
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
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var pageSize, pageCount, freePages int64
	for _, item := range []struct {
		query string
		value *int64
	}{{"PRAGMA page_size", &pageSize}, {"PRAGMA page_count", &pageCount}, {"PRAGMA freelist_count", &freePages}} {
		if err := tx.QueryRowContext(ctx, item.query).Scan(item.value); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("pageSize=%d allocatedPages=%d reusablePages=%d allocatedBytes=%d reusableBytes=%d occupiedBytes=%d",
		pageSize, pageCount, freePages, pageSize*pageCount, pageSize*freePages, pageSize*(pageCount-freePages))
	rows, err := tx.QueryContext(ctx, `SELECT state,count(*),coalesce(sum(indexed_offset),0),coalesce(sum(target_offset),0) FROM projection_revisions GROUP BY state ORDER BY state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count, indexed, target int64
		if err := rows.Scan(&state, &count, &indexed, &target); err != nil {
			t.Fatal(err)
		}
		t.Logf("projectionState=%s revisions=%d indexedSourceBytes=%d targetSourceBytes=%d", state, count, indexed, target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
