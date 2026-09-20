package store

import (
	"context"
	"strings"
	"testing"
)

func TestCatalogAttributionWithoutPricingSnapshot(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	before, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE query_usage SET model='',kind='incomplete-attribution' WHERE id='usage-s-000000';
 INSERT INTO query_usage SELECT 'excluded-old',session_id,source_id,'old',projection_revision,agent_id,'','incomplete-attribution',timestamp,99999,0,0,0 FROM query_usage WHERE id='usage-s-000000';`); err != nil {
		t.Fatal(err)
	}
	var want int64
	if err = s.db.QueryRow(`SELECT tokens_in+tokens_cache+tokens_write+tokens_out FROM query_usage WHERE id='usage-s-000000'`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	after, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if want == 0 || after.KnownUnattributedTokens != want || after.RecordedTokens != before.RecordedTokens || after.PricedTokens != before.PricedTokens {
		t.Fatalf("missing independent attribution: want=%d before=%+v after=%+v", want, before, after)
	}
	machines, err := s.CatalogMachines(ctx, SessionQuery{})
	if err != nil || len(machines.Items) != 1 || machines.Items[0].Accounting.KnownUnattributedTokens != want {
		t.Fatalf("machine attribution: %+v %v", machines, err)
	}
	// A pricing snapshot must not erase independently recorded missing attribution.
	if _, err = s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.pricing',json('{"snapshotId":"fixture","pricedTokens":0,"unattributedTokens":0}'))`); err != nil {
		t.Fatal(err)
	}
	after, err = s.CatalogTotals(ctx, SessionQuery{})
	if err != nil || after.KnownUnattributedTokens != want {
		t.Fatalf("snapshot erased attribution: %+v %v", after, err)
	}
}

func TestCatalogAttributionUsesPublishedModelIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT SUM(` + catalogUnattributed + `) FROM query_sessions q`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH m USING INDEX") && strings.Contains(detail, "session_id=? AND source_id=? AND generation=? AND projection_revision=?") {
			found = true
		}
		if strings.Contains(detail, "SCAN m") || strings.Contains(detail, "query_usage") {
			t.Fatalf("history scan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("missing indexed lookup of published model summary")
	}
}
