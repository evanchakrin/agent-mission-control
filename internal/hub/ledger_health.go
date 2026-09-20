package hub

import (
	"context"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// LedgerObservation describes reporting freshness, not ingestion authority.
// Diagnostics always represents a completed successful check; unknown values
// are absent, and failed refreshes do not replace evidence with zero counts.
type LedgerObservation struct {
	State           string             `json:"state"`
	CheckedAt       *time.Time         `json:"checkedAt,omitempty"`
	CompletedAt     *time.Time         `json:"completedAt,omitempty"`
	ProbeStartedAt  *time.Time         `json:"probeStartedAt,omitempty"`
	ProbeInProgress bool               `json:"probeInProgress"`
	Problem         string             `json:"problem,omitempty"`
	Diagnostics     *store.Diagnostics `json:"-"`
}

type LedgerMonitor struct {
	Probe    func(context.Context) (store.Diagnostics, error)
	mu       sync.RWMutex
	last     LedgerObservation
	started  time.Time
	inFlight bool
	running  sync.Mutex
}

func (m *LedgerMonitor) Snapshot(now time.Time) LedgerObservation {
	m.mu.RLock()
	v, started, active := m.last, m.started, m.inFlight
	m.mu.RUnlock()
	v.ProbeInProgress = active
	if active {
		v.ProbeStartedAt = &started
	}
	switch {
	case active && now.Sub(started) >= time.Second:
		v.State, v.Problem = "probe-delayed", "Ledger check is delayed; any counts are from the last successful check"
	case v.CheckedAt != nil && now.Sub(*v.CheckedAt) >= 15*time.Second:
		v.State, v.Problem = "stale", "Ledger observation is stale; current progress is unknown"
	case v.State == "":
		v.State, v.Problem = "checking", "No completed ledger check is available"
	}
	// Do not expose pointers into the monitor's stored observation to callers.
	if v.Diagnostics != nil {
		d := *v.Diagnostics
		v.Diagnostics = &d
	}
	if v.CheckedAt != nil {
		at := *v.CheckedAt
		v.CheckedAt = &at
	}
	if v.CompletedAt != nil {
		at := *v.CompletedAt
		v.CompletedAt = &at
	}
	return v
}

func (m *LedgerMonitor) measure(ctx context.Context) {
	started := time.Now().UTC()
	m.mu.Lock()
	m.started, m.inFlight = started, true
	m.mu.Unlock()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	d, err := m.Probe(probeCtx)
	probeErr := probeCtx.Err()
	cancel()
	completed := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = false
	if ctx.Err() != nil {
		return
	}
	m.last.CompletedAt = &completed
	if err != nil || probeErr != nil {
		m.last.State, m.last.Problem = "blocked", "Database diagnostics unavailable; any counts are from the last successful check"
		return
	}
	m.last = LedgerObservation{State: "ready", CheckedAt: &started, CompletedAt: &completed, Diagnostics: &d}
}

// Run performs at most one probe at a time. A slow native call cannot create
// extra probe goroutines or block Snapshot. Cancellation abandons late results;
// it does not pretend that an uninterruptible storage call has been terminated.
func (m *LedgerMonitor) Run(ctx context.Context) {
	if !m.running.TryLock() {
		return
	}
	defer m.running.Unlock()
	for ctx.Err() == nil {
		m.measure(ctx)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// CheckLedger is used by the single background monitor in the service. The
// fixed-stage watchdog remains available in private diagnostic logs.
func (h *Hub) CheckLedger(ctx context.Context) (store.Diagnostics, error) {
	stage, stop := watchHealth(h.HealthStall, time.Second)
	defer stop()
	return h.Store.DiagnosticsWithStage(ctx, stage)
}
