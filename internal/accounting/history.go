package accounting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type HistorySamplerStatus struct {
	State       string    `json:"state"`
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	LastCapture time.Time `json:"lastCapture,omitempty"`
	NextAttempt time.Time `json:"nextAttempt,omitempty"`
	Problem     string    `json:"problem,omitempty"`
}

// These timestamps describe this sampler process, not the latest durable entry
// (which may have been captured manually or by a previous process).
func (s HistorySamplerStatus) MarshalJSON() ([]byte, error) {
	optional := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		t = t.UTC()
		return &t
	}
	return json.Marshal(struct {
		State            string     `json:"state"`
		ObservationScope string     `json:"observationScope"`
		LastAttempt      *time.Time `json:"lastAttempt"`
		LastCapture      *time.Time `json:"lastCapture"`
		NextAttempt      *time.Time `json:"nextAttempt"`
		Problem          string     `json:"problem,omitempty"`
	}{s.State, "current-process", optional(s.LastAttempt), optional(s.LastCapture), optional(s.NextAttempt), s.Problem})
}

// HistorySampler is independent of ingestion and collector heartbeats. The
// durable capture method enforces spacing across hub restarts; this loop checks
// infrequently and never backfills invented samples for time the hub was off.
type HistorySampler struct {
	Capture func(context.Context, string, string) (*store.EconomicsHistoryEntry, error)
	OnError func(error)
	mu      sync.Mutex
	status  HistorySamplerStatus
}

func (h *HistorySampler) Status() HistorySamplerStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.status
	if s.State == "" {
		s.State = "waiting"
	}
	return s
}

func (h *HistorySampler) Run(ctx context.Context) error {
	return h.run(ctx, 5*time.Minute, 6*time.Hour)
}

func (h *HistorySampler) run(ctx context.Context, initial, interval time.Duration) error {
	if h.Capture == nil || initial < 0 || interval <= 0 {
		return errors.New("invalid economics history sampler configuration")
	}
	h.mu.Lock()
	h.status = HistorySamplerStatus{State: "waiting", NextAttempt: time.Now().UTC().Add(initial)}
	h.mu.Unlock()
	timer := time.NewTimer(initial)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.status.State = "stopped"
			h.status.NextAttempt = time.Time{}
			h.mu.Unlock()
			return ctx.Err()
		case <-timer.C:
			h.sample(ctx)
			if ctx.Err() != nil {
				continue
			}
			h.mu.Lock()
			h.status.NextAttempt = time.Now().UTC().Add(interval)
			h.mu.Unlock()
			timer.Reset(interval)
		}
	}
}

func (h *HistorySampler) sample(ctx context.Context) {
	h.mu.Lock()
	h.status.State = "sampling"
	h.status.LastAttempt = time.Now().UTC()
	h.status.NextAttempt = time.Time{}
	h.mu.Unlock()
	var random [16]byte
	_, err := rand.Read(random[:])
	var entry *store.EconomicsHistoryEntry
	if err == nil {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		entry, err = h.Capture(bounded, "economics-"+hex.EncodeToString(random[:]), "timer")
		cancel()
	}
	h.mu.Lock()
	if err != nil {
		h.status.State = "blocked"
		h.status.Problem = err.Error()
	} else {
		h.status.State = "waiting"
		h.status.Problem = ""
		if entry != nil {
			h.status.LastCapture = entry.Measurement.MeasuredAt
		}
	}
	h.mu.Unlock()
	if err != nil && ctx.Err() == nil && h.OnError != nil {
		h.OnError(err)
	}
}
