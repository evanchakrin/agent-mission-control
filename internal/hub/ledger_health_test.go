package hub

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestLedgerObservationFreshnessFailureAndRecovery(t *testing.T) {
	m := &LedgerMonitor{Probe: func(ctx context.Context) (store.Diagnostics, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("probe deadline missing")
		}
		return store.Diagnostics{Sources: 12, DurableBytes: 80, IndexedBytes: 60}, nil
	}}
	if v := m.Snapshot(time.Now()); v.State != "checking" || v.Diagnostics != nil || v.CheckedAt != nil {
		t.Fatal(v)
	}
	m.measure(context.Background())
	v := m.Snapshot(time.Now())
	if v.State != "ready" || v.Diagnostics.Sources != 12 || v.CheckedAt == nil || v.CompletedAt == nil {
		t.Fatal(v)
	}
	checked := *v.CheckedAt
	if v := m.Snapshot(checked.Add(15 * time.Second)); v.State != "stale" || v.Problem == "" {
		t.Fatal(v)
	}
	v.Diagnostics.Sources = 999
	*v.CheckedAt = time.Time{}
	if v := m.Snapshot(time.Now()); v.Diagnostics.Sources != 12 || !v.CheckedAt.Equal(checked) {
		t.Fatal("mutable snapshot", v)
	}
	m.Probe = func(context.Context) (store.Diagnostics, error) {
		return store.Diagnostics{}, errors.New("secret fixture path")
	}
	m.measure(context.Background())
	v = m.Snapshot(time.Now())
	if v.State != "blocked" || v.Diagnostics.Sources != 12 || !v.CheckedAt.Equal(checked) || strings.Contains(v.Problem, "secret") {
		t.Fatal(v)
	}
	m.Probe = func(context.Context) (store.Diagnostics, error) { return store.Diagnostics{Sources: 15}, nil }
	m.measure(context.Background())
	if v := m.Snapshot(time.Now()); v.State != "ready" || v.Problem != "" || v.Diagnostics.Sources != 15 {
		t.Fatal(v)
	}
}

func TestLedgerProbeDoesNotBlockHealthOrMultiplyWork(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	// No database operations can succeed: the monitored health path must not
	// fall back to synchronous SQL while its only probe is blocked.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := &LedgerMonitor{Probe: func(context.Context) (store.Diagnostics, error) {
		calls.Add(1)
		close(entered)
		<-release
		return store.Diagnostics{Sources: 99}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	m.mu.Lock()
	m.started = time.Now().Add(-2 * time.Second)
	m.mu.Unlock()
	h := New(s, testToken, "test")
	h.LedgerStatus = func() LedgerObservation { return m.Snapshot(time.Now()) }
	handler := h.OwnerHandler()
	response := make(chan string, 1)
	go func() {
		for i := 0; i < 100; i++ {
			r := httptest.NewRequest("GET", "/api/v2/health", nil)
			r.Header.Set("Authorization", "Bearer "+testToken)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, `"state":"probe-delayed"`) || !strings.Contains(body, `"ledgerError"`) || strings.Contains(body, `"ledger":`) {
				response <- body
				return
			}
		}
		response <- ""
	}()
	select {
	case problem := <-response:
		if problem != "" {
			t.Fatal(problem)
		}
	case <-time.After(time.Second):
		t.Fatal("health waited for database probe")
	}
	// Even accidental duplicate starts must not run another probe.
	duplicate := make(chan struct{})
	go func() { defer close(duplicate); m.Run(ctx) }()
	select {
	case <-duplicate:
	case <-time.After(time.Second):
		t.Fatal("duplicate monitor did not return")
	}
	if calls.Load() != 1 {
		t.Fatal("extra probes", calls.Load())
	}
	cancel()
}

func TestLedgerProbeDoesNotPublishAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &LedgerMonitor{Probe: func(context.Context) (store.Diagnostics, error) { cancel(); return store.Diagnostics{Sources: 20}, nil }}
	m.measure(ctx)
	if v := m.Snapshot(time.Now()); v.State != "checking" || v.Diagnostics != nil || v.ProbeInProgress {
		t.Fatal(v)
	}
}
