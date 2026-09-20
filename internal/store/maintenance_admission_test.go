package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceAdmissionLeavesOwnerQueriesUnblocked(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 1)
	jobs := []func(context.Context) error{
		func(ctx context.Context) error { _, err := s.RebuildWork(ctx, 1); return err },
		func(ctx context.Context) error { _, err := s.RebuildProgress(ctx); return err },
		func(ctx context.Context) error { _, err := s.NextPricingJob(ctx); return err },
		func(ctx context.Context) error { _, err := s.NextProjectDeletion(ctx); return err },
		func(ctx context.Context) error { _, err := s.PendingSources(ctx, "", 1); return err },
		func(ctx context.Context) error { _, err := s.RetrySources(ctx, 1); return err },
		func(ctx context.Context) error { _, err := s.IndexingProgress(ctx); return err },
		func(ctx context.Context) error { _, err := s.Diagnostics(ctx); return err },
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, len(jobs))
	before := s.ConnectionStats()
	for _, job := range jobs {
		go func(job func(context.Context) error) { done <- job(ctx) }(job)
	}
	waitWriterQueue(t, &s.maintenanceMu, len(jobs), 0)
	after := s.ConnectionStats()
	if after.InUse != before.InUse || after.WaitCount != before.WaitCount || after.MaxOpenConnections != 4 {
		t.Fatal("background waiters consumed database admission", before, after)
	}
	owner, ownerCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ownerCancel()
	if _, err := s.CatalogTotals(owner, SessionQuery{}); err != nil {
		t.Fatal("owner totals blocked behind background admission", err)
	}
	yes := true
	if _, err := s.PatchMetadata(owner, "s-000000", MetadataPatch{Archived: &yes, OperationID: "background-admission-archive"}); err != nil {
		t.Fatal("owner organization blocked behind background admission", err)
	}
	cancel()
	for range jobs {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("background admission did not cancel", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("background admission waiter leaked")
		}
	}
	waitWriterQueue(t, &s.maintenanceMu, 0, 0)
}
