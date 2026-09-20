package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit query-only comparison of retained projection evidence; no migrations.
func TestReadOnlyRebuildEventDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_REBUILD_EVENT_DIAGNOSTIC_DB")
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
	rows, err := db.QueryContext(ctx, `SELECT projection_revision,kind,count(*),MIN(source_offset),MAX(source_offset+raw_length) FROM events WHERE session_id=? AND projection_revision IN (?,?) GROUP BY projection_revision,kind ORDER BY projection_revision,kind`, os.Getenv("AMC_REBUILD_EVENT_SESSION"), os.Getenv("AMC_REBUILD_EVENT_OLD"), os.Getenv("AMC_REBUILD_EVENT_NEW"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var revision, kind string
		var count, first, end int64
		if err := rows.Scan(&revision, &kind, &count, &first, &end); err != nil {
			t.Fatal(err)
		}
		t.Logf("revision=%s kind=%s count=%d first=%d end=%d", revision, kind, count, first, end)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	// Fingerprint corresponding published-prefix event streams in commit order.
	// Only one event is retained at a time; no transcript copy or corpus sort.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var boundary int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(source_offset+raw_length),0) FROM events WHERE session_id=? AND projection_revision=?`, os.Getenv("AMC_REBUILD_EVENT_SESSION"), os.Getenv("AMC_REBUILD_EVENT_OLD")).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	var sums []string
	var counts []int
	for _, revision := range []string{os.Getenv("AMC_REBUILD_EVENT_OLD"), os.Getenv("AMC_REBUILD_EVENT_NEW")} {
		prefixRows, err := tx.QueryContext(ctx, `SELECT source_id,generation,source_offset,raw_length,agent_id,kind,timestamp,text,data,dedupe_key FROM events WHERE session_id=? AND projection_revision=? AND source_offset+raw_length<=? ORDER BY seq`, os.Getenv("AMC_REBUILD_EVENT_SESSION"), revision, boundary)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		enc := json.NewEncoder(h)
		count := 0
		for prefixRows.Next() {
			var source, generation, agent, kind, at, text, key string
			var offset, length int64
			var data []byte
			if err := prefixRows.Scan(&source, &generation, &offset, &length, &agent, &kind, &at, &text, &data, &key); err != nil {
				t.Fatal(err)
			}
			if err := enc.Encode([]any{source, generation, offset, length, agent, kind, at, text, data, key}); err != nil {
				t.Fatal(err)
			}
			count++
		}
		err = prefixRows.Err()
		prefixRows.Close()
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, hex.EncodeToString(h.Sum(nil)))
		counts = append(counts, count)
	}
	t.Logf("shared prefix end=%d old events=%d new events=%d fingerprints equal=%t old=%s new=%s", boundary, counts[0], counts[1], sums[0] == sums[1], sums[0], sums[1])
}
