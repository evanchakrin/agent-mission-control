package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestStorageObservationReportsReadinessAndAge(t *testing.T) {
	s := storageStatusSampler{probe: func() (uint64, uint64, error) { return 20 << 30, 100 << 30, nil }}
	if v := s.snapshot(time.Now()); v.State != "checking" || v.AvailableBytes != nil || v.CheckedAt != nil {
		t.Fatal(v)
	}
	s.measure(context.Background())
	v := s.snapshot(time.Now())
	if v.State != "ready" || *v.AvailableBytes != 20<<30 || *v.ReserveBytes != 5<<30 || v.CheckedAt == nil || v.ProbeInProgress {
		t.Fatal(v)
	}
	if stale := s.snapshot(v.CheckedAt.Add(16 * time.Second)); stale.State != "stale" || stale.AvailableBytes == nil || stale.Problem == "" {
		t.Fatal(stale)
	}
	// A hung refresh must retain the last observation without presenting it as
	// current readiness or silently replacing it with zero available bytes.
	s.started, s.inFlight = time.Now().Add(-2*time.Second), true
	if delayed := s.snapshot(time.Now()); delayed.State != "probe-delayed" || delayed.CheckedAt != v.CheckedAt || *delayed.AvailableBytes != 20<<30 || delayed.Problem == "" {
		t.Fatal(delayed)
	}
	s.inFlight = false
	s.probe = func() (uint64, uint64, error) { return 6 << 30, 200 << 30, nil }
	s.measure(context.Background())
	if v = s.snapshot(time.Now()); v.State != "blocked" || *v.ReserveBytes != 10<<30 || v.Problem == "" {
		t.Fatal(v)
	}
	s.probe = func() (uint64, uint64, error) { return 0, 0, errors.New("fixture access denied") }
	s.measure(context.Background())
	v = s.snapshot(time.Now())
	if v.State != "blocked" || v.AvailableBytes != nil || v.ReserveBytes != nil || v.Problem != "fixture access denied" {
		t.Fatal(v)
	}
	b, err := json.Marshal(v)
	if err != nil || strings.Contains(string(b), "availableBytes") {
		t.Fatal("failed probe invented a byte count", string(b), err)
	}
	s.probe = func() (uint64, uint64, error) { return 20 << 30, 100 << 30, nil }
	s.measure(context.Background())
	if v = s.snapshot(time.Now()); v.State != "ready" || v.Problem != "" {
		t.Fatal("probe did not recover", v)
	}
}

func TestBlockedStorageProbeDoesNotBlockSnapshotsOrSpawnMoreWork(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := storageStatusSampler{probe: func() (uint64, uint64, error) {
		calls.Add(1)
		close(entered)
		<-release
		return 20 << 30, 100 << 30, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { defer close(done); s.run(ctx) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never started")
	}
	for i := 0; i < 1000; i++ {
		v := s.snapshot(time.Now().Add(2 * time.Second))
		if v.State != "probe-delayed" || !v.ProbeInProgress || v.ProbeStartedAt == nil || v.AvailableBytes != nil {
			t.Fatal(v)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("snapshot launched more probes", calls.Load())
	}
	cancel()
	// OS calls cannot be forcibly canceled: shutdown must not depend on waiting
	// for this reporting goroutine, and a late result must not be published.
}

func TestHealthRespondsWhileStorageProbeIsBlocked(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	sampler := storageStatusSampler{probe: func() (uint64, uint64, error) { close(entered); <-release; return 20 << 30, 100 << 30, nil }}
	go func() { defer close(done); sampler.run(ctx) }()
	defer func() { cancel(); close(release); <-done }()
	<-entered
	sampler.mu.Lock()
	sampler.started = time.Now().Add(-2 * time.Second)
	sampler.mu.Unlock()
	const token = "only-a-disposable-health-test-token"
	h := hub.New(s, token, "test")
	h.StorageStatus = func() any { return sampler.snapshot(time.Now()) }
	r := httptest.NewRequest("GET", "/api/v2/health", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	response := make(chan struct{})
	go func() { defer close(response); h.OwnerHandler().ServeHTTP(w, r) }()
	select {
	case <-response:
	case <-time.After(time.Second):
		t.Fatal("health waited on blocked free-space probe")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"probe-delayed"`) || strings.Contains(w.Body.String(), `"availableBytes":0`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCanceledStorageObservationDoesNotPublishLateReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := storageStatusSampler{probe: func() (uint64, uint64, error) { cancel(); return 20 << 30, 100 << 30, nil }}
	s.measure(ctx)
	v := s.snapshot(time.Now())
	if v.State != "checking" || v.CheckedAt != nil || v.ProbeInProgress {
		t.Fatal("canceled observation published readiness", v)
	}
}
