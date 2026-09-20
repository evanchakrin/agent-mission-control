package main

import (
	"context"
	"sync"
	"time"
)

type storageObservation struct {
	State           string     `json:"state"`
	AvailableBytes  *uint64    `json:"availableBytes,omitempty"`
	ReserveBytes    *uint64    `json:"reserveBytes,omitempty"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	ProbeStartedAt  *time.Time `json:"probeStartedAt,omitempty"`
	ProbeInProgress bool       `json:"probeInProgress"`
	Problem         string     `json:"problem,omitempty"`
}

// Only the reporting worker calls probe. An uninterruptible OS call cannot
// block HTTP requests or create an accumulating population of probe goroutines.
// This snapshot is never used to authorize ingestion or bypass reserve checks.
type storageStatusSampler struct {
	probe    func() (uint64, uint64, error)
	mu       sync.RWMutex
	last     storageObservation
	started  time.Time
	inFlight bool
}

func (s *storageStatusSampler) snapshot(now time.Time) storageObservation {
	s.mu.RLock()
	v, started, inFlight := s.last, s.started, s.inFlight
	s.mu.RUnlock()
	v.ProbeInProgress = inFlight
	if inFlight {
		v.ProbeStartedAt = &started
	}
	switch {
	case inFlight && now.Sub(started) >= time.Second:
		v.State = "probe-delayed"
		v.Problem = "Free-space query has not completed; any byte values are from the last completed check"
	case v.CheckedAt == nil:
		v.State = "checking"
		v.Problem = "No completed free-space check is available"
	case now.Sub(*v.CheckedAt) >= 15*time.Second:
		v.State = "stale"
		v.Problem = "Free-space observation is stale; current availability is unknown"
	}
	return v
}

func (s *storageStatusSampler) measure(ctx context.Context) {
	s.mu.Lock()
	s.started = time.Now().UTC()
	s.inFlight = true
	s.mu.Unlock()
	available, total, err := s.probe()
	checked := time.Now().UTC()
	v := storageObservation{State: "ready", CheckedAt: &checked}
	if err != nil {
		v.State, v.Problem = "blocked", err.Error()
	} else {
		reserve := max(uint64(5<<30), total/20)
		v.AvailableBytes, v.ReserveBytes = &available, &reserve
		if available < reserve {
			v.State, v.Problem = "blocked", "Available storage is below the required reserve"
		}
	}
	s.mu.Lock()
	s.inFlight = false
	if ctx.Err() == nil {
		s.last = v
	}
	s.mu.Unlock()
}

func (s *storageStatusSampler) run(ctx context.Context) {
	for ctx.Err() == nil {
		s.measure(ctx)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
