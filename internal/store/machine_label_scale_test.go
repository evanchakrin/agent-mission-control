package store

import (
	"context"
	"sort"
	"testing"
	"time"
)

// Catalog scale only: this is not the 20 GiB source/fleet/resource soak gate.
func TestMachineLabels100001SessionCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-session machine-label catalog fixture")
	}
	s := openTestStore(t, Options{})
	ctx := context.Background()
	start := time.Now()
	seedAnalyticsCatalog(t, s, 100001)
	t.Logf("seed100001=%s", time.Since(start))
	start = time.Now()
	if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", DisplayName: "Owner scale fixture", OperationID: "scale-rename"}); err != nil {
		t.Fatal(err)
	}
	t.Logf("rename4001Sessions=%s", time.Since(start))
	totals, err := s.CatalogTotals(ctx, SessionQuery{Text: "Owner scale fixture"})
	if err != nil || totals.Sessions != 4001 || totals.TokensIn != 4001 || totals.TokensOut != 8002 {
		t.Fatal("renamed history lost", totals, err)
	}
	// Upgrade the disposable search schema without touching source projections.
	if _, err = s.db.Exec(`UPDATE properties SET value='2' WHERE key='catalog_search_schema'`); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err = s.ensureCatalogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("catalogSearchUpgrade100001=%s", time.Since(start))
	after, err := s.CatalogTotals(ctx, SessionQuery{Text: "Owner scale fixture"})
	if err != nil || after.Sessions != totals.Sessions || after.RecordedTokens != totals.RecordedTokens {
		t.Fatal("upgrade changed coverage", after, err)
	}
	var timings []time.Duration
	for i := 0; i < 12; i++ {
		start = time.Now()
		page, e := s.QuerySessions(ctx, SessionQuery{Text: "Owner scale fixture", Limit: 100})
		timings = append(timings, time.Since(start))
		if e != nil || len(page.Sessions) != 100 || page.NextCursor == "" {
			t.Fatal("bounded search page", len(page.Sessions), e)
		}
		for _, row := range page.Sessions {
			if row.MachineID != "m0" || row.MachineName != "Owner scale fixture" {
				t.Fatal(row.ID, row.MachineID, row.MachineName)
			}
		}
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	p95 := timings[len(timings)-1] // nearest-rank p95 for 12 samples
	t.Logf("labeledSearchPage100 p95=%s samples=%v", p95, timings)
	if p95 > 500*time.Millisecond {
		t.Errorf("indexed first-page target exceeded: %s > 500ms", p95)
	}
	if _, err = s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", Revision: 1, OperationID: "scale-clear"}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.CatalogTotals(ctx, SessionQuery{Text: "Owner scale fixture"})
	if err != nil || empty.Sessions != 0 {
		t.Fatal("cleared label still searchable", empty, err)
	}
	all, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil || all.Sessions != 100001 || all.RecordedTokens != 400004 {
		t.Fatal("global history changed", all, err)
	}
}
