package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestDurableHealthRefreshIsBoundedAndDoesNotWaitForCatalogEnd(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	x.status.Durable.Blocked = 99
	last := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	if err = x.refreshIfDue(ctx, &last, last.Add(29*time.Second)); err != nil || x.Status().Durable.Blocked != 99 {
		t.Fatal("refresh was not throttled", err)
	}
	due := last.Add(30 * time.Second)
	if err = x.refreshIfDue(ctx, &last, due); err != nil || x.Status().Durable.Blocked != 0 || !last.Equal(due) {
		t.Fatal("stale status was retained", x.Status(), err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	prior := last
	if err = x.refreshIfDue(cancelled, &last, last.Add(30*time.Second)); err == nil || !last.Equal(prior) {
		t.Fatal("failed refresh consumed interval", err, last)
	}
}
