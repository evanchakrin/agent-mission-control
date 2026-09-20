package store

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// This is a focused concurrency check, not the 100k catalog or fleet soak gate.
// It uses the production worker cadence and indexes with FULL durability.
func TestArchiveLatencyDuringProjectDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent writer latency fixture")
	}
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3001)
	ctx := context.Background()
	p, err := s.MutateProject(ctx, ProjectMutation{ID: "latency-project", Name: "Latency", Color: "#123456", OperationID: "latency-create"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO session_metadata SELECT id,1,json_object('revision',1,'project','latency-project','projectOverride',json('true')) FROM sessions WHERE id<>'s-003000'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueProjectDeletion(ctx, ProjectMutation{ID: p.ID, Revision: 1, Delete: true, OperationID: "latency-delete"}); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.RunProjectDeletions(lifetime, nil) }()
	defer func() { stop(); <-done }()
	var times []time.Duration
	var revision int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		job, err := s.ProjectDeletion(ctx, "latency-delete")
		if err != nil {
			t.Fatal(err)
		}
		if job.State == "complete" {
			break
		}
		yes := len(times)%2 == 0
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := time.Now()
		m, err := s.PatchMetadata(request, "s-003000", MetadataPatch{Revision: revision, Archived: &yes, OperationID: fmt.Sprintf("latency-archive-%d", len(times))})
		times = append(times, time.Since(start))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		revision = m.Revision
		time.Sleep(20 * time.Millisecond)
	}
	job, err := s.ProjectDeletion(ctx, "latency-delete")
	if err != nil || job.State != "complete" || job.Processed != 3000 {
		t.Fatal("deletion did not finish", job, err)
	}
	if len(times) < 20 {
		t.Fatal("insufficient overlapping archive samples", len(times))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p95 := times[(len(times)*95+99)/100-1]
	t.Logf("archive samples=%d p95=%s max=%s during 3000-member deletion", len(times), p95, times[len(times)-1])
	if p95 >= 250*time.Millisecond {
		t.Errorf("archive commit p95 exceeded 250 ms: %s", p95)
	}
}
