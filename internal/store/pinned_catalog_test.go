package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestPinnedCatalogPaginationAcrossEverySortAndNullCosts(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 14)
	ctx := context.Background()
	pins := map[string]bool{"s-000000": true, "s-000003": true, "s-000005": true, "s-000009": true}
	for id := range pins {
		value := true
		if _, err := s.PatchMetadata(ctx, id, MetadataPatch{Pinned: &value, OperationID: "pin-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"lastActivity", "title", "tokens", "cost"} {
		for _, direction := range []string{"asc", "desc"} {
			t.Run(key+"-"+direction, func(t *testing.T) {
				base, err := s.QuerySessions(ctx, SessionQuery{Sort: key, Direction: direction, Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				var want, got []string
				for _, pin := range []bool{true, false} {
					for _, row := range base.Sessions {
						if pins[row.ID] == pin {
							want = append(want, row.ID)
						}
					}
				}
				q := SessionQuery{Sort: key, Direction: direction, Limit: 2, PinnedFirst: true}
				for i := 0; i < 20; i++ {
					page, err := s.QuerySessions(ctx, q)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range page.Sessions {
						got = append(got, row.ID)
					}
					if page.NextCursor == "" {
						break
					}
					wrong := q
					wrong.Cursor = page.NextCursor
					wrong.PinnedFirst = false
					if _, err = s.QuerySessions(ctx, wrong); !errors.Is(err, ErrInvalid) {
						t.Fatalf("wrong pin mode accepted: %v", err)
					}
					q.Cursor = page.NextCursor
				}
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("want %v; got %v", want, got)
				}
			})
		}
	}
	// Trigger updates must immediately remove a pin without changing usage.
	value := false
	if _, err := s.PatchMetadata(ctx, "s-000009", MetadataPatch{Pinned: &value, Revision: 1, OperationID: "unpin"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.QuerySessions(ctx, SessionQuery{PinnedFirst: true, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Sessions {
		if !row.Metadata.Pinned {
			t.Fatalf("unpinned before remaining pins: %s", row.ID)
		}
	}
}

func TestAnalyticsV3PinUpgradePreservesHistoryReadModels(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 5)
	ctx := context.Background()
	value := true
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{Pinned: &value, OperationID: "pin"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetMetadata(ctx, "s-000000")
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the old catalog shape. History tables retain a sentinel index,
	// which would disappear if this upgrade needlessly rebuilt their contents.
	for _, name := range []string{"query_session_insert", "query_session_update", "query_session_delete", "query_metadata_insert", "query_metadata_update", "query_metadata_delete"} {
		if _, err = s.db.ExecContext(ctx, "DROP TRIGGER "+name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"activity", "activity_asc", "title", "title_desc", "tokens", "tokens_asc", "cost", "cost_asc"} {
		if _, err = s.db.ExecContext(ctx, "DROP INDEX query_pin_"+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `DROP INDEX query_machine_accounting;ALTER TABLE query_sessions DROP COLUMN pinned;
	ALTER TABLE query_sessions DROP COLUMN pricing_known; ALTER TABLE query_sessions DROP COLUMN priced_tokens; ALTER TABLE query_sessions DROP COLUMN unattributed_tokens;
	CREATE INDEX preserve_usage_index ON query_usage(id); UPDATE properties SET value='3' WHERE key='analytics_schema'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"session", "metadata"} {
		for _, event := range []string{"insert", "update", "delete"} {
			source := "sessions"
			if table == "metadata" {
				source = "session_metadata"
			}
			if _, err = s.db.ExecContext(ctx, fmt.Sprintf("CREATE TRIGGER query_%s_%s AFTER %s ON %s BEGIN SELECT 1; END", table, event, event, source)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='preserve_usage_index'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history index replaced: %d %v", count, err)
	}
	after, err := s.GetMetadata(ctx, "s-000000")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("metadata changed: %+v %v", after, err)
	}
	page, err := s.QuerySessions(ctx, SessionQuery{PinnedFirst: true, Limit: 1})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "s-000000" {
		t.Fatalf("pin not migrated: %+v %v", page, err)
	}
	value = false
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{Pinned: &value, Revision: 1, OperationID: "unpin"}); err != nil {
		t.Fatal(err)
	}
	page, err = s.QuerySessions(ctx, SessionQuery{PinnedFirst: true, Limit: 1})
	if err != nil || page.Sessions[0].ID != "s-000004" {
		t.Fatalf("upgraded trigger failed: %+v %v", page, err)
	}
}
