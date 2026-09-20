package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestHistorySamplerJSONDistinguishesUnobservedFromDurableHistory(t *testing.T) {
	h := &HistorySampler{}
	encode := func() map[string]any {
		t.Helper()
		raw, err := json.Marshal(h.Status())
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err = json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := encode()
	if initial["state"] != "waiting" || initial["observationScope"] != "current-process" {
		t.Fatal(initial)
	}
	for _, key := range []string{"lastAttempt", "lastCapture", "nextAttempt"} {
		if value, exists := initial[key]; !exists || value != nil {
			t.Fatal("unobserved timestamp was not explicit null", key, initial)
		}
	}
	stamp := time.Date(2026, 9, 5, 11, 30, 0, 123, time.FixedZone("fixture", 3600))
	h.status = HistorySamplerStatus{State: "waiting", LastAttempt: stamp, LastCapture: stamp, NextAttempt: stamp.Add(time.Hour)}
	measured := encode()
	if measured["lastCapture"] != "2026-09-05T10:30:00.000000123Z" || measured["nextAttempt"] != "2026-09-05T11:30:00.000000123Z" {
		t.Fatal(measured)
	}
	h.status.State = "stopped"
	h.status.NextAttempt = time.Time{}
	stopped := encode()
	if stopped["nextAttempt"] != nil || stopped["lastCapture"] != measured["lastCapture"] {
		t.Fatal(stopped)
	}
}

func TestHistorySamplerFailureVisibleRecoveryAndSkippedCapture(t *testing.T) {
	failure := errors.New("storage unavailable")
	calls, logs := 0, 0
	stamp := time.Now().UTC()
	h := &HistorySampler{OnError: func(err error) {
		if !errors.Is(err, failure) {
			t.Error(err)
		}
		logs++
	}}
	h.Capture = func(ctx context.Context, id, reason string) (*store.EconomicsHistoryEntry, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second || reason != "timer" || len(id) != 42 {
			t.Fatalf("unbounded or invalid capture: %s %s", id, reason)
		}
		if calls == 1 {
			return nil, failure
		}
		if calls == 3 {
			return nil, nil
		}
		return &store.EconomicsHistoryEntry{Measurement: store.EconomicsMeasurement{MeasuredAt: stamp}}, nil
	}
	h.sample(context.Background())
	if s := h.Status(); s.State != "blocked" || s.Problem != failure.Error() || logs != 1 {
		t.Fatal(s, logs)
	}
	h.sample(context.Background())
	if s := h.Status(); s.State != "waiting" || s.Problem != "" || !s.LastCapture.Equal(stamp) {
		t.Fatal(s)
	}
	h.sample(context.Background())
	if s := h.Status(); !s.LastCapture.Equal(stamp) {
		t.Fatal("skipped timer lost last capture", s)
	}
}

func TestHistorySamplerCancelsInFlightAndWaitingWithoutBlockedLog(t *testing.T) {
	for _, initial := range []time.Duration{0, time.Hour} {
		t.Run(initial.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			done := make(chan error, 1)
			h := &HistorySampler{Capture: func(ctx context.Context, _, _ string) (*store.EconomicsHistoryEntry, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}, OnError: func(error) { t.Error("normal shutdown logged as blocked") }}
			go func() { done <- h.run(ctx, initial, time.Hour) }()
			if initial == 0 {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("sampler did not start")
				}
				if h.Status().State != "sampling" {
					t.Fatal(h.Status())
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown did not cancel sampler")
			}
			if s := h.Status(); s.State != "stopped" || !s.NextAttempt.IsZero() {
				t.Fatal(s)
			}
		})
	}
}
