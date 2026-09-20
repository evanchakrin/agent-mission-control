package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicitly opted-in, query-only inspection of one current projection. Never
// opens Store (which would migrate), emits transcript text, or changes evidence.
func TestReadOnlyHookDiagnosticCounts(t *testing.T) {
	path, id := os.Getenv("AMC_HOOK_DIAGNOSTIC_DB"), os.Getenv("AMC_HOOK_DIAGNOSTIC_SESSION")
	if path == "" && id == "" {
		t.Skip("no read-only ledger and session selected")
	}
	if _, err := hex.DecodeString(id); err != nil || len(id) != 64 || !filepath.IsAbs(path) {
		t.Fatal("absolute ledger and 64-character hex session ID required")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var provider string
	var diagnostics, offset int64
	err = tx.QueryRowContext(ctx, `SELECT q.provider,h.diagnostics,h.indexed_offset FROM query_sessions q JOIN hook_javascript_evidence h ON h.source_id=q.source_id AND h.generation=q.generation AND h.revision=q.projection_revision WHERE q.id=?`, id).Scan(&provider, &diagnostics, &offset)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "claude" && provider != "codex" {
		provider = "other"
	}
	t.Logf("provider=%s hook_diagnostics=%d indexed_offset=%d", provider, diagnostics, offset)
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN json_extract(e.data,'$.code') IN ('record_too_large','malformed_json','ambiguous_claude_identity','invalid_usage_field','embedded_thread_metadata','unsupported_provider','metadata_exceeds_budget','invalid_usage_counter','counter_discontinuity') THEN json_extract(e.data,'$.code') ELSE 'unknown' END,count(*) FROM query_sessions q JOIN events e ON e.session_id=q.id AND e.source_id=q.source_id AND e.generation=q.generation AND e.projection_revision=q.projection_revision WHERE q.id=? AND e.kind='indexing-error' GROUP BY 1 ORDER BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var code string
		var count int64
		if err := rows.Scan(&code, &count); err != nil {
			t.Fatal(err)
		}
		total += count
		t.Logf("code=%s count=%d", code, count)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("parser_diagnostics=%d remaining_preview_only_diagnostics=%d", total, diagnostics-total)
	if total > diagnostics {
		t.Fatal("parser diagnostics exceed hook checkpoint")
	}
	rows.Close()
	tx.Rollback()
	if provider == "codex" {
		inspectCounterDiscontinuities(t, ctx, db, filepath.Dir(path), id)
	}
}

func inspectCounterDiscontinuities(t *testing.T, ctx context.Context, db *sql.DB, dir, id string) {
	t.Helper()
	type sample struct {
		source, generation, revision, scope string
		offset, length                      int64
	}
	rows, err := db.QueryContext(ctx, `SELECT e.source_id,e.generation,e.projection_revision,json_extract(e.data,'$.counterScope'),e.source_offset,e.raw_length FROM query_sessions q JOIN events e ON e.session_id=q.id AND e.source_id=q.source_id AND e.generation=q.generation AND e.projection_revision=q.projection_revision WHERE q.id=? AND e.kind='indexing-error' AND json_extract(e.data,'$.code')='counter_discontinuity' ORDER BY e.seq LIMIT 3`, id)
	if err != nil {
		t.Fatal(err)
	}
	var samples []sample
	for rows.Next() {
		var v sample
		if err := rows.Scan(&v.source, &v.generation, &v.revision, &v.scope, &v.offset, &v.length); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		samples = append(samples, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Only the existing raw reader is used, on the same query-only connection.
	reader := &Store{dir: dir, db: db}
	for i, v := range samples {
		if v.length <= 0 || v.length > 8<<20 {
			t.Fatal("unexpected sample record size")
		}
		r, err := reader.OpenSource(ctx, v.source, v.generation, v.offset)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(io.LimitReader(r, v.length))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Payload struct {
				Info struct {
					Total struct {
						Input  int64 `json:"input_tokens"`
						Cache  int64 `json:"cached_input_tokens"`
						Output int64 `json:"output_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal("sample is not a complete JSON record")
		}
		var observation []byte
		err = db.QueryRowContext(ctx, `SELECT observation FROM usage_observations WHERE session_id=? AND source_id=? AND generation=? AND projection_revision=? AND json_extract(observation,'$.counterScope')=? AND json_extract(observation,'$.evidence.offset')<? ORDER BY json_extract(observation,'$.evidence.offset') DESC LIMIT 1`, id, v.source, v.generation, v.revision, "thread:"+v.scope, v.offset).Scan(&observation)
		if err != nil {
			t.Fatal(err)
		}
		var previous struct {
			Evidence struct {
				Counter struct{ Input, Cache, Output int64 }
			}
		}
		if err := json.Unmarshal(observation, &previous); err != nil {
			t.Fatal("invalid stored usage evidence")
		}
		next, prev := record.Payload.Info.Total, previous.Evidence.Counter
		t.Logf("sample=%d offset=%d previous_input=%d cache=%d output=%d next_input=%d cache=%d output=%d", i, v.offset, prev.Input, prev.Cache, prev.Output, next.Input, next.Cache, next.Output)
	}
}
