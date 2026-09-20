package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDailyUsageSummaryMatchesRawCalendarPages(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 40)
	ctx := context.Background()
	_, err := s.db.Exec(`UPDATE query_usage SET timestamp=CASE CAST(substr(session_id,3) AS INTEGER)%6
 WHEN 0 THEN '0001-01-01T00:00:00.000000000Z'
 WHEN 1 THEN '2025-12-28T23:59:59.999999999Z'
 WHEN 2 THEN '2025-12-29T00:00:00.000000000Z'
 WHEN 3 THEN '2026-01-01T00:00:00.000000000Z'
 WHEN 4 THEN '2026-02-01T12:00:00.000000000Z'
 ELSE '2028-02-29T12:00:00.000000000Z' END;
 INSERT INTO query_usage SELECT 'extra-day',session_id,source_id,generation,projection_revision,agent_id,'','incomplete-attribution','2026-01-02T00:00:00.000000000Z',7,2,3,4 FROM query_usage WHERE id='usage-s-000000';
 INSERT INTO query_usage SELECT 'old-day',session_id,source_id,'old',projection_revision,agent_id,model,kind,timestamp,99999,0,0,0 FROM query_usage WHERE id='usage-s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	to := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	check := func() {
		t.Helper()
		for _, dimension := range []string{"day", "week", "month", "year"} {
			for _, provider := range []string{"", "claude", "codex"} {
				q := GroupQuery{Dimension: dimension, SessionQuery: SessionQuery{Limit: 2, Provider: provider}}
				for {
					fast, e := s.GroupedUsage(ctx, q)
					if e != nil {
						t.Fatal(e)
					}
					rawQuery := q
					rawQuery.To = &to
					raw, e := s.GroupedUsage(ctx, rawQuery)
					if e != nil {
						t.Fatal(e)
					}
					a, _ := json.Marshal(fast)
					b, _ := json.Marshal(raw)
					if string(a) != string(b) {
						t.Fatalf("%s/%s summary=%s raw=%s", dimension, provider, a, b)
					}
					if fast.NextCursor == "" {
						break
					}
					q.Cursor = fast.NextCursor
				}
			}
		}
	}
	check()
	if _, err = s.db.Exec("DROP TRIGGER daily_usage_insert"); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureUsageProjection(ctx, true); err != nil {
		t.Fatal(err)
	}
	check() // Backfill must match incremental writes exactly.
	if _, err = s.db.Exec("DELETE FROM query_usage WHERE id='extra-day';UPDATE query_usage SET timestamp='2026-03-01T00:00:00.000000000Z',tokens_in=8 WHERE id='usage-s-000000'"); err != nil {
		t.Fatal(err)
	}
	check() // Updated timestamp removes the old day's contribution.
	if _, err = s.db.Exec("DELETE FROM query_usage"); err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []string{"day", "week", "month", "year"} {
		page, e := s.GroupedUsage(ctx, GroupQuery{Dimension: dimension})
		if e != nil || len(page.Groups) != 1 || page.Groups[0].Key != "" || page.Groups[0].Sessions != 40 || page.Groups[0].Observations != 0 || page.Groups[0].CostEstimate != nil {
			t.Fatalf("empty sessions %s %+v %v", dimension, page, e)
		}
	}
}

func TestDailyUsageBackfillStorageFailurePreservesSummary(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	_, err := s.db.Exec(`DROP TRIGGER daily_usage_insert;
 WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<5001)
 INSERT INTO query_usage SELECT printf('daily-%d',i),u.session_id,u.source_id,u.generation,u.projection_revision,u.agent_id,u.model,u.kind,'2026-02-01T00:00:00.000000000Z',1,0,0,0 FROM n CROSS JOIN query_usage u WHERE u.id='usage-s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	s.options.ReserveBytes = 1
	s.options.AvailableBytes = func(string) (int64, error) {
		checks++
		if checks == 3 {
			return 1, nil
		}
		return 1 << 30, nil
	}
	if err = s.ensureUsageProjection(context.Background(), true); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	var n int64
	if err = s.db.QueryRow("SELECT SUM(observations) FROM query_daily_usage").Scan(&n); err != nil || n != 1 {
		t.Fatal("partial summary published", n, err)
	}
	s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
	if err = s.ensureUsageProjection(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT SUM(observations) FROM query_daily_usage").Scan(&n); err != nil || n != 5002 {
		t.Fatal("retry lost usage", n, err)
	}
}
