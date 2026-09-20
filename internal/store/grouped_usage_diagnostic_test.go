package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOnlyGroupedUsageDiagnostic(t *testing.T) {
	path := os.Getenv("AMC_GROUPED_DIAGNOSTIC_DB")
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
	if err = db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&guard); err != nil || guard != 1 {
		t.Fatal("read-only guard", err)
	}
	// Do not run accounting initialization or open/migrate Store. All SQL uses
	// the read-only connection; incompatible schemas fail instead of upgrading.
	s := &Store{db: db}
	s.accountingReady.Store(true)
	dimension := os.Getenv("AMC_GROUPED_DIAGNOSTIC_DIMENSION")
	if dimension == "" {
		dimension = "model"
	}
	var observations, sessions, priced int64
	var definition string
	if err = db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE name='query_usage'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	t.Logf("usage schema: %s", definition)
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM query_usage LIMIT 1000)").Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(pricing_known),0) FROM query_sessions").Scan(&sessions, &priced); err != nil {
		t.Fatal(err)
	}
	t.Logf("usageProbeRowsAtMost1000=%d sessionRows=%d pricedSessions=%d", observations, sessions, priced)
	planSQL := `EXPLAIN QUERY PLAN SELECT DISTINCT COALESCE(u.model,'') AS model FROM query_sessions q LEFT JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision ORDER BY model LIMIT 101`
	if dimension == "day" {
		planSQL = `EXPLAIN QUERY PLAN SELECT COALESCE(m.day,''),COUNT(DISTINCT q.id),SUM(m.observations) FROM query_sessions q LEFT JOIN query_daily_usage m ON m.session_id=q.id AND m.source_id=q.source_id AND m.generation=q.generation AND m.projection_revision=q.projection_revision GROUP BY COALESCE(m.day,'') ORDER BY COALESCE(m.day,'') LIMIT 101`
		var modelCount, dailyCount, modelTokens, dailyTokens int64
		countsTx, e := db.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer countsTx.Rollback()
		for _, entry := range []struct {
			table         string
			count, tokens *int64
		}{{"query_model_usage", &modelCount, &modelTokens}, {"query_daily_usage", &dailyCount, &dailyTokens}} {
			if err = countsTx.QueryRowContext(ctx, "SELECT SUM(m.observations),SUM(m.tokens_in+m.tokens_cache+m.tokens_write+m.tokens_out) FROM "+entry.table+" m JOIN query_sessions q ON q.id=m.session_id AND q.source_id=m.source_id AND q.generation=m.generation AND q.projection_revision=m.projection_revision").Scan(entry.count, entry.tokens); err != nil {
				t.Fatal(err)
			}
		}
		if err = countsTx.Commit(); err != nil {
			t.Fatal(err)
		}
		if modelCount != dailyCount || modelTokens != dailyTokens {
			t.Fatal("summary mismatch", modelCount, dailyCount, modelTokens, dailyTokens)
		}
		t.Logf("current model/day summaries agree: observations=%d recordedTokens=%d", dailyCount, dailyTokens)
	}
	plan, err := db.QueryContext(ctx, planSQL)
	if err != nil {
		t.Fatal(err)
	}
	for plan.Next() {
		var a, b, c int
		var detail string
		if err = plan.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
	}
	err = plan.Err()
	plan.Close()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	page, err := s.GroupedUsage(ctx, GroupQuery{Dimension: dimension, SessionQuery: SessionQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dimension=%s groups=%d nextPage=%t elapsed=%s; single read, not p95", dimension, len(page.Groups), page.NextCursor != "", time.Since(started))
	for _, g := range page.Groups {
		t.Logf("group=%q observations=%d recorded=%d unknownAttribution=%d priced=%d", g.Key, g.Observations, g.TokensIn+g.TokensCache+g.TokensCacheWrite+g.TokensOut, *g.UnknownModelTokens, g.PricedTokens)
	}
}
