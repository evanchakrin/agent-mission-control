package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestModelSummaryCatalogGroupsMatchRawPages(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 40)
	ctx := context.Background()
	// Multiple models in one session must not multiply its session count.
	_, err := s.db.Exec(`INSERT INTO query_usage SELECT 'extra-model',session_id,source_id,generation,projection_revision,agent_id,'', 'incomplete-attribution',timestamp,7,1,2,3 FROM query_usage WHERE id='usage-s-000000';
 INSERT INTO query_usage SELECT 'old-generation',session_id,source_id,'old',projection_revision,agent_id,model,kind,timestamp,99999,0,0,0 FROM query_usage WHERE id='usage-s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, dimension := range []string{"provider", "machine", "project", "model"} {
		for _, provider := range []string{"", "claude", "codex"} {
			q := GroupQuery{Dimension: dimension, SessionQuery: SessionQuery{Limit: 2, Provider: provider}}
			for {
				fast, e := s.GroupedUsage(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				rawQuery := q
				rawQuery.From = &from // Force the unchanged observation-level query.
				raw, e := s.GroupedUsage(ctx, rawQuery)
				if e != nil {
					t.Fatal(e)
				}
				a, _ := json.Marshal(fast)
				b, _ := json.Marshal(raw)
				if string(a) != string(b) {
					t.Fatalf("%s/%s: summary %s raw %s", dimension, provider, a, b)
				}
				if fast.NextCursor == "" {
					break
				}
				q.Cursor = fast.NextCursor
			}
		}
	}
	// Sessions without any usage still appear, with no invented cost or tokens.
	if _, err = s.db.Exec("DELETE FROM query_usage"); err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []string{"provider", "machine", "project"} {
		page, e := s.GroupedUsage(ctx, GroupQuery{Dimension: dimension, SessionQuery: SessionQuery{Limit: 100}})
		if e != nil {
			t.Fatal(e)
		}
		var sessions int64
		for _, g := range page.Groups {
			sessions += g.Sessions
			if g.Observations != 0 || g.CostEstimate != nil || g.PricedTokens != 0 || g.PricingCoverage != "no-recorded-usage" {
				t.Fatalf("empty usage: %+v", g)
			}
		}
		if sessions != 40 {
			t.Fatal(dimension, sessions)
		}
	}
}

func TestModelUsageBackfillRollsBackOnStorageLossAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage", true: "cancel"}[cancelRun], func(t *testing.T) {
			s := openTestStore(t, Options{})
			seedAnalyticsCatalog(t, s, 1)
			_, err := s.db.Exec(`DROP TRIGGER model_usage_insert;DROP TRIGGER model_usage_update;DROP TRIGGER model_usage_delete;
 WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<5001)
 INSERT INTO query_usage SELECT printf('backfill-%d',i),u.session_id,u.source_id,u.generation,u.projection_revision,u.agent_id,'later-model',u.kind,u.timestamp,1,0,0,0 FROM n CROSS JOIN query_usage u WHERE u.id='usage-s-000000';
 INSERT INTO query_usage(rowid,id,session_id,source_id,generation,projection_revision,agent_id,model,kind,timestamp,tokens_in,tokens_cache,tokens_write,tokens_out)
 SELECT -1,'negative-rowid',session_id,source_id,generation,projection_revision,agent_id,'negative',kind,timestamp,1,0,0,0 FROM query_usage WHERE id='usage-s-000000';`)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checks := 0
			s.options.ReserveBytes = 1
			s.options.AvailableBytes = func(string) (int64, error) {
				checks++
				if checks == 3 {
					if cancelRun {
						cancel()
					} else {
						return 1, nil
					}
				}
				return 1 << 30, nil
			}
			err = s.ensureModelUsageProjection(ctx)
			want := ErrCapacity
			if cancelRun {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			var observations int64
			if err = s.db.QueryRow("SELECT SUM(observations) FROM query_model_usage").Scan(&observations); err != nil || observations != 1 {
				t.Fatal("partial backfill published", observations, err)
			}
			if err = s.db.QueryRow("SELECT COUNT(*) FROM query_usage").Scan(&observations); err != nil || observations != 5003 {
				t.Fatal("source usage changed", observations, err)
			}
			s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
			if err = s.ensureModelUsageProjection(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRow("SELECT SUM(observations) FROM query_model_usage").Scan(&observations); err != nil || observations != 5003 {
				t.Fatal("retry incomplete", observations, err)
			}
		})
	}
}

func TestModelUsageDenseHistorySummaryMatchesRawAggregation(t *testing.T) {
	if testing.Short() {
		t.Skip("dense 100001-observation diagnostic")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	_, err := s.db.Exec(`WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<=100000)
 INSERT INTO query_usage SELECT printf('dense-%d',i),u.session_id,u.source_id,u.generation,u.projection_revision,u.agent_id,'dense-model',u.kind,u.timestamp,1,0,0,2 FROM n CROSS JOIN query_usage u WHERE u.id='usage-s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TRIGGER model_usage_insert"); err != nil {
		t.Fatal(err)
	}
	backfillStart := time.Now()
	if err = s.ensureModelUsageProjection(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("dense bounded backfill=%s", time.Since(backfillStart))
	q := GroupQuery{Dimension: "model", SessionQuery: SessionQuery{Limit: 100}}
	started := time.Now()
	fast, err := s.GroupedUsage(ctx, q)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	q.From = &from
	raw, err := s.GroupedUsage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fast)
	b, _ := json.Marshal(raw)
	if string(a) != string(b) {
		t.Fatalf("summary mismatch: %s versus %s", a, b)
	}
	var models int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM query_model_usage").Scan(&models); err != nil || models != 2 {
		t.Fatal(models, err)
	}
	t.Logf("100001 dense observations plus seed, %d summary rows, first-page=%s; diagnostic not p95 certification", models, elapsed)
}

func TestModelUsageProjectionFollowsObservationRevisionsAndRollback(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	check := func(model string, observations, tokens, unknown int64) {
		t.Helper()
		var count, total, unattributed int64
		err := s.db.QueryRow(`SELECT observations,tokens_in+tokens_cache+tokens_write+tokens_out,unknown_tokens FROM query_model_usage WHERE session_id='s-000000' AND model=?`, model).Scan(&count, &total, &unattributed)
		if err != nil || count != observations || total != tokens || unattributed != unknown {
			t.Fatal(count, total, unattributed, err)
		}
	}
	check("model-0000", 1, 4, 0)
	u := UsageObservation{ID: "usage-s-000000", SessionID: "s-000000", AgentID: "main", Model: "", Kind: "incomplete-attribution", TokensIn: 7}
	b, _ := json.Marshal(u)
	if _, err := s.db.Exec(`UPDATE usage_observations SET observation=? WHERE id=?`, b, u.ID); err != nil {
		t.Fatal(err)
	}
	check("", 1, 7, 7)
	var groups int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_model_usage").Scan(&groups); err != nil || groups != 1 {
		t.Fatal(groups, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE query_usage SET tokens_in=999"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check("", 1, 7, 7)
	if _, err = s.db.Exec("DELETE FROM usage_observations WHERE id=?", u.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM query_model_usage").Scan(&groups); err != nil || groups != 0 {
		t.Fatal(groups, err)
	}
}

func TestModelUsageProjectionBackfillsAndSeparatesGenerations(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	_, err := s.db.Exec(`DROP TRIGGER model_usage_insert;DROP TRIGGER model_usage_update;DROP TRIGGER model_usage_delete;DELETE FROM query_model_usage;
 INSERT INTO query_usage SELECT 'old',session_id,source_id,'old',projection_revision,agent_id,model,kind,timestamp,100,0,0,0 FROM query_usage WHERE id='usage-s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ensureModelUsageProjection(ctx); err != nil {
		t.Fatal(err)
	}
	var groups, total int64
	if err = s.db.QueryRow(`SELECT COUNT(*),SUM(tokens_in+tokens_cache+tokens_write+tokens_out) FROM query_model_usage`).Scan(&groups, &total); err != nil || groups != 2 || total != 104 {
		t.Fatal(groups, total, err)
	}
	if err = s.db.QueryRow(`SELECT SUM(m.tokens_in+m.tokens_cache+m.tokens_write+m.tokens_out) FROM query_model_usage m JOIN query_sessions q ON q.id=m.session_id AND q.source_id=m.source_id AND q.generation=m.generation AND q.projection_revision=m.projection_revision`).Scan(&total); err != nil || total != 4 {
		t.Fatal(total, err)
	}
	if err = s.ensureModelUsageProjection(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM query_model_usage").Scan(&groups); err != nil || groups != 2 {
		t.Fatal(groups, err)
	}
}
