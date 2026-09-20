package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestCatalogSearchIncludesLegacyFieldsAndUpdatedNotes(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	if err := s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "m1", Name: "Office Étage"}); err != nil {
		t.Fatal(err)
	}
	note := `Shared résumé 50%_\notes`
	for _, id := range []string{"s-000000", "s-000002"} {
		if _, err := s.PatchMetadata(ctx, id, MetadataPatch{OperationID: "note-" + id, Note: &note}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ query, id string }{
		{"Transcript 000001", "s-000001"}, {"p001", "s-000001"}, {"m2", "s-000002"},
		{"OFFICE ÉTAGE", "s-000001"}, {"s-000000", "s-000000"},
		{"000001 p001", "s-000001"},
	} {
		page, err := s.ListSessions(ctx, SessionQuery{Text: tc.query})
		if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != tc.id {
			t.Fatalf("search %q: %v %+v", tc.query, err, page)
		}
		totals, err := s.CatalogTotals(ctx, SessionQuery{Text: tc.query})
		if err != nil || totals.Sessions != 1 {
			t.Fatal("search totals disagree", tc.query, err, totals.Sessions)
		}
	}
	q := SessionQuery{Text: `RÉSUMÉ 50%_\notes`, Limit: 1}
	first, err := s.ListSessions(ctx, q)
	if err != nil || len(first.Sessions) != 1 || first.NextCursor == "" {
		t.Fatal("matching notes were restricted to one page", err)
	}
	q.Cursor = first.NextCursor
	second, err := s.ListSessions(ctx, q)
	if err != nil || len(second.Sessions) != 1 || first.Sessions[0].ID == second.Sessions[0].ID || second.NextCursor != "" {
		t.Fatal("note search pagination incorrect", err)
	}
	totals, err := s.CatalogTotals(ctx, q)
	if err != nil || totals.Sessions != 2 {
		t.Fatal("note search aggregate omitted later page", err)
	}
	replacement := "new note"
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "replace-note", Revision: 1, Note: &replacement}); err != nil {
		t.Fatal(err)
	}
	q.Cursor = ""
	totals, err = s.CatalogTotals(ctx, q)
	if err != nil || totals.Sessions != 1 {
		t.Fatal("search retained obsolete owner note", err)
	}
	q.Text = "new note"
	page, err := s.ListSessions(ctx, q)
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "s-000000" {
		t.Fatal("updated note not immediately searchable", err)
	}
	if err = s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "m1", Name: "Renamed workshop"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		term  string
		count int64
	}{{"ÉTAGE", 0}, {"workshop", 1}, {"m1", 1}, {"%_", 1}, {`"quoted"`, 0}} {
		value, e := s.CatalogTotals(ctx, SessionQuery{Text: tc.term})
		if e != nil || value.Sessions != tc.count {
			t.Fatal("updated or short search mismatch", tc, e, value.Sessions)
		}
	}
}

func TestIndexedCatalogSearch100001Sessions(t *testing.T) {
	if testing.Short() {
		t.Skip("100001-session indexed search fixture")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 100001)
	ctx := context.Background()
	note := "unique résumé needle"
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "indexed-search-note", Note: &note}); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"absent-catalog-probe-4a6b498b", "unique RÉSUMÉ needle", "Transcript 100000", "Transcript", "m0", "p"} {
		for sample := 0; sample < 3; sample++ {
			started := time.Now()
			page, err := s.ListSessions(ctx, SessionQuery{Text: term, Limit: 100, PinnedFirst: true})
			if err != nil {
				t.Fatal(err)
			}
			pageTime := time.Since(started)
			started = time.Now()
			totals, err := s.CatalogTotals(ctx, SessionQuery{Text: term})
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(page.Sessions)) != min(totals.Sessions, 100) {
				t.Fatal("indexed count mismatch")
			}
			t.Logf("term=%q sample=%d page=%s totals=%s matches=%d", term, sample, pageTime, time.Since(started), totals.Sessions)
			if pageTime >= 200*time.Millisecond {
				t.Errorf("indexed page exceeded 200 ms target: %s", pageTime)
			}
		}
	}
}

func TestCatalogSearchRebuildPreservesOrganizationAndSearch(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	note := `quoted "needle" 100%_`
	yes := true
	want, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "preserve-search-note", Note: &note, Archived: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE properties SET value='1' WHERE key='analytics_schema'`); err != nil {
		t.Fatal(err)
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSessions(ctx, SessionQuery{Text: `"needle" 100%_`})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatal("search lost during read-model rebuild", err)
	}
	got := page.Sessions[0].Metadata
	if got.Revision != want.Revision || got.Note != want.Note || !got.Archived {
		t.Fatal("search migration changed organization")
	}
	next := "replacement"
	if _, err = s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "replace-after-search-rebuild", Revision: want.Revision, Note: &next}); err != nil {
		t.Fatal(err)
	}
	old, err := s.CatalogTotals(ctx, SessionQuery{Text: "needle"})
	if err != nil || old.Sessions != 0 {
		t.Fatal("rebuilt search triggers retained old text", err)
	}
}

func TestCatalogSearchDoesNotRewriteForCountOnlyUpdates(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before, after int64
	if err = tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE query_sessions SET event_count=event_count+1 WHERE id='s-000000'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after-before != 1 {
		t.Fatal("count-only catalog update rewrote dependent search storage", after-before)
	}
}

func TestCatalogSearchTotalsPreserveEveryAggregateAndFilter(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 30)
	ctx := context.Background()
	yes, no := true, false
	if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "search-totals-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, q := range []SessionQuery{{}, {Provider: "claude"}, {Provider: "missing"}, {MachineID: "m1"}, {Project: "p001"}, {UnassignedProject: true}, {Archived: &yes}, {Archived: &no}, {From: &from, To: &to, MachineID: "m1", Archived: &yes}} {
		want, err := s.CatalogTotals(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range []string{"Transcript", "p"} {
			q.Text = term // Every fixture title contains both terms.
			got, err := s.CatalogTotals(ctx, q)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("search changed filtered accounting: query=%+v error=%v got=%+v want=%+v", q, err, got, want)
			}
		}
	}
}

func TestCatalogSearchDensePagesAndSparseTailPreserveOrdering(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2050)
	ctx := context.Background()
	yes := true
	note := "oldest unique needle"
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "dense-search-pin", Pinned: &yes, Note: &note}); err != nil {
		t.Fatal(err)
	}
	for _, sort := range []string{"lastActivity", "title", "tokens", "cost"} {
		for _, direction := range []string{"asc", "desc"} {
			for _, pinned := range []bool{false, true} {
				q := SessionQuery{Sort: sort, Direction: direction, PinnedFirst: pinned, Limit: 100, Provider: "claude"}
				for pageNumber := 0; pageNumber < 3; pageNumber++ {
					plain, err := s.ListSessions(ctx, q)
					if err != nil {
						t.Fatal(err)
					}
					search := q
					search.Text = "Transcript"
					filtered, err := s.ListSessions(ctx, search)
					if err != nil {
						t.Fatal(err)
					}
					if len(plain.Sessions) != len(filtered.Sessions) || plain.NextCursor != filtered.NextCursor {
						t.Fatal("dense search changed page boundary", sort, direction, pinned)
					}
					for i := range plain.Sessions {
						if plain.Sessions[i].ID != filtered.Sessions[i].ID {
							t.Fatal("dense search changed row order", sort, direction, pinned)
						}
					}
					q.Cursor = plain.NextCursor
				}
			}
		}
	}
	q := SessionQuery{Text: "Transcript", Limit: 100}
	seen := map[string]bool{}
	for {
		page, err := s.ListSessions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Sessions {
			if seen[row.ID] {
				t.Fatal("duplicate search row")
			}
			seen[row.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != 2050 {
		t.Fatal("dense search omitted history beyond probe", len(seen))
	}
	page, err := s.ListSessions(ctx, SessionQuery{Text: note, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "s-000000" {
		t.Fatal("sparse search missed match beyond sorted probe", err)
	}
}
