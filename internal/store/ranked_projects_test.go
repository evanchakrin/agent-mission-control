package store

import (
	"context"
	"errors"
	"testing"
)

func TestRankedProjectsCountsBeforePagingAndRejectsMovedOrder(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 6)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE sessions SET project=CASE WHEN id<'s-000003' THEN 'busy' WHEN id<'s-000005' THEN 'middle' ELSE 'quiet' END`); err != nil {
		t.Fatal(err)
	}
	no := false
	q := SessionQuery{Limit: 1, Archived: &no}
	first, err := s.RankedProjects(ctx, q)
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "busy" || first.Items[0].Sessions != 3 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	q.Cursor = first.NextCursor
	second, err := s.RankedProjects(ctx, q)
	if err != nil || second.Items[0].ID != "middle" || second.Items[0].Sessions != 2 {
		t.Fatal(second, err)
	}
	q.Cursor = second.NextCursor
	third, err := s.RankedProjects(ctx, q)
	if err != nil || third.Items[0].ID != "quiet" || third.NextCursor != "" {
		t.Fatal(third, err)
	}
	yes := true
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "rank-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	if _, err = s.RankedProjects(ctx, q); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("stale ranking accepted", err)
	}
	q.Cursor = ""
	fresh, err := s.RankedProjects(ctx, q)
	if err != nil || fresh.Items[0].ID != "middle" {
		t.Fatal("latest activity tie break", fresh, err)
	}
}
