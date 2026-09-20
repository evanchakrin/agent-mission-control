package store

import (
	"context"
	"reflect"
	"testing"
)

func TestMissingUndoCatalogFiltersBeforePagingAndTotals(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 8)
	ctx := context.Background()
	// Fixture-only: current evidence includes warned sessions. Other revisions
	// and generations must not hide missing evidence in the active projection.
	if _, err := s.db.Exec(`INSERT INTO git_undo_checkpoints
	 SELECT source_id,generation,projection_revision,0,0,0,1 FROM query_sessions WHERE id<'s-000005';
	 INSERT INTO git_undo_checkpoints VALUES('source-000005','g1','staged',0,0,0,0);
	 INSERT INTO git_undo_checkpoints VALUES('source-000006','old-generation','',0,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "s-000007", MetadataPatch{OperationID: "archive-missing", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q := SessionQuery{MissingUndoEvidence: true, Limit: 2}
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
