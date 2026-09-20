package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestMissingHookCatalogFiltersBeforePagingAndTotals(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 8)
	ctx := context.Background()
	// Fixture-only: current evidence includes warned sessions. Other revisions
	// and generations must not hide missing evidence in the active projection.
	if _, err := s.db.Exec(`INSERT INTO hook_javascript_evidence
	 SELECT source_id,generation,projection_revision,0,0,0,1 FROM query_sessions WHERE id<'s-000005';
	 INSERT INTO hook_javascript_evidence VALUES('source-000005','g1','staged',0,0,0,0);
	 INSERT INTO hook_javascript_evidence VALUES('source-000006','old-generation','',0,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "s-000007", MetadataPatch{OperationID: "archive-missing", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q := SessionQuery{MissingJavaScriptEvidence: true, Limit: 2}
	var ids []string
	for {
		page, err := s.QuerySessions(ctx, q)
		if err != nil || len(page.Sessions) > 2 {
			t.Fatal(page, err)
		}
		for _, row := range page.Sessions {
			ids = append(ids, row.ID)
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(ids, []string{"s-000007", "s-000006", "s-000005"}) {
		t.Fatal(ids)
	}
	totals, err := s.CatalogTotals(ctx, q)
	if err != nil || totals.Sessions != 3 {
		t.Fatal(totals, err)
	}
	no := false
	q.Cursor, q.Archived = "", &no
	totals, err = s.CatalogTotals(ctx, q)
	if err != nil || totals.Sessions != 2 {
		t.Fatal(totals, err)
	}
	all, err := s.CatalogTotals(ctx, SessionQuery{})
	if err != nil || all.Sessions != 8 {
		t.Fatal(all, err)
	}
}

func TestMissingHookCatalog100001Sessions(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001-session recovery catalog fixture")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 100001)
	// Put the only missing entries at the end of the default activity order,
	// exercising a nearly completed corpus, not only a cheap first batch.
	if _, err := s.db.Exec(`INSERT INTO hook_javascript_evidence SELECT source_id,generation,projection_revision,0,0,0,0 FROM query_sessions WHERE id>='s-000003'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now()
	page, err := s.QuerySessions(ctx, SessionQuery{MissingJavaScriptEvidence: true, Limit: 2})
	elapsed := time.Since(start)
	if err != nil || len(page.Sessions) != 2 || page.Sessions[0].ID != "s-000002" || page.Sessions[1].ID != "s-000001" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	last, err := s.QuerySessions(ctx, SessionQuery{MissingJavaScriptEvidence: true, Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(last.Sessions) != 1 || last.Sessions[0].ID != "s-000000" || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	t.Logf("100001-session recovery page=%s; correctness fixture, not workload p95 certification", elapsed)
	if _, err := s.db.Exec(`INSERT INTO git_undo_checkpoints SELECT source_id,generation,projection_revision,0,0,0,0 FROM query_sessions WHERE id>='s-000003'`); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	undo, err := s.QuerySessions(ctx, SessionQuery{MissingUndoEvidence: true, Limit: 2})
	if err != nil || len(undo.Sessions) != 2 || undo.Sessions[0].ID != "s-000002" || undo.Sessions[1].ID != "s-000001" || undo.NextCursor == "" {
		t.Fatal(undo, err)
	}
	t.Logf("100001-session undo recovery page=%s; correctness fixture, not workload p95 certification", time.Since(start))
	undoLast, err := s.QuerySessions(ctx, SessionQuery{MissingUndoEvidence: true, Limit: 2, Cursor: undo.NextCursor})
	if err != nil || len(undoLast.Sessions) != 1 || undoLast.Sessions[0].ID != "s-000000" || undoLast.NextCursor != "" {
		t.Fatal(undoLast, err)
	}
	totals, err := s.CatalogTotals(ctx, SessionQuery{MissingUndoEvidence: true})
	if err != nil || totals.Sessions != 3 {
		t.Fatal(totals, err)
	}
}
