package store

import (
	"context"
	"strings"
	"testing"
)

func TestCostFlowRanksBeyondFirstAgentPageAndCurrentEvidenceOnly(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<250)
 INSERT INTO query_usage SELECT printf('rank-%03d',i),u.session_id,u.source_id,u.generation,u.projection_revision,printf('z-%03d',i),u.model,u.kind,u.timestamp,0,0,0,i FROM n CROSS JOIN query_usage u WHERE u.id='usage-s-000000';
 INSERT INTO query_usage SELECT 'obsolete',session_id,source_id,'old-generation',projection_revision,'obsolete',model,kind,timestamp,0,0,0,1000000 FROM query_usage WHERE id='usage-s-000000';
 INSERT INTO accounting_estimates VALUES('selected','s-000000','now','{"attributionVersion":1}'),('unselected','s-000000','now','{"attributionVersion":1}');
 INSERT INTO observation_prices SELECT 'selected',id,agent_id,model,json_object('cost',tokens_out/1000.0,'pricedTokens',tokens_out) FROM query_usage WHERE id LIKE 'rank-%';
 INSERT INTO observation_prices VALUES('unselected','wrong','wrong','model','{"cost":999,"pricedTokens":1}');
 UPDATE sessions SET projection=json_set(projection,'$.pricing',json('{"snapshotId":"selected"}')) WHERE id='s-000000';`)
	if err != nil {
		t.Fatal(err)
	}
	for _, cost := range []bool{false, true} {
		top, err := s.topFlow(ctx, "s-000000", cost)
		if err != nil {
			t.Fatal(err)
		}
		if len(top) != 14 || top[0].ID != "z-250" || top[13].ID != "z-237" {
			t.Fatal(top)
		}
		want := 250.0
		if cost {
			want = .25
		}
		if top[0].Value != want {
			t.Fatal(top[0])
		}
	}
	for _, item := range []struct{ sql, index string }{{topFlowOutputSQL, "query_usage_session_agent"}, {topFlowCostSQL, "observation_prices_agent"}} {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+item.sql, "s-000000")
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), item.index) {
			t.Fatal("missing scoped ranking index", plan.String())
		}
	}
}
