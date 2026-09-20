package store

import (
	"context"
	"errors"
	"testing"
)

func TestAnalyticsReadinessObservesBothDurableVersions(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.ensureAnalytics(ctx); err != nil {
		t.Fatal("fresh readiness", err)
	}
	if err := s.ensureAnalytics(ctx); err != nil {
		t.Fatal("ready read models", err)
	}
	for _, tc := range []struct{ key, current string }{
		{"analytics_schema", "10"},
		{"catalog_search_schema", "4"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if _, err := s.db.Exec(`UPDATE properties SET value='unsupported-fixture' WHERE key=?`, tc.key); err != nil {
				t.Fatal(err)
			}
			if err := s.ensureAnalytics(ctx); err == nil {
				t.Fatal("previous successful readiness hid an unsupported durable version")
			}
			if _, err := s.db.Exec(`UPDATE properties SET value=? WHERE key=?`, tc.current, tc.key); err != nil {
				t.Fatal(err)
			}
			if err := s.ensureAnalytics(ctx); err != nil {
				t.Fatal("restored fixture readiness", err)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ensureAnalytics(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("readiness ignored cancellation", err)
	}
}
