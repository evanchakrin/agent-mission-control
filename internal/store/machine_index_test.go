package store

import (
	"context"
	"strings"
	"testing"
)

func TestMachineAccountingV5UpgradeUsesCoveringIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DROP INDEX query_machine_accounting;CREATE INDEX preserved_machine_usage ON query_usage(id);UPDATE properties SET value='5' WHERE key='analytics_schema'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := s.db.QueryRow(`SELECT value FROM properties WHERE key='analytics_schema'`).Scan(&version); err != nil || version != "10" {
		t.Fatal(version, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='preserved_machine_usage'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("history index rebuilt", err)
	}
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT machine_id,count(*),sum(tokens_in),sum(tokens_cache),sum(tokens_write),sum(tokens_out),sum(priced_tokens),sum(unattributed_tokens),sum(pricing_known),sum(CASE WHEN pricing_known=0 THEN tokens_in+tokens_cache+tokens_write+tokens_out ELSE 0 END),sum(cost_estimate),max(last_activity) FROM query_sessions GROUP BY machine_id ORDER BY machine_id LIMIT 101`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	covered := false
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		covered = covered || strings.Contains(detail, "COVERING INDEX query_machine_accounting")
	}
	if err = rows.Err(); err != nil || !covered {
		t.Fatal("fleet query not covered", err)
	}
}
