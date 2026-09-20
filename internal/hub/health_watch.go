package hub

import (
	"sync/atomic"
	"time"
)

// One warning per watched request, with only fixed stage names and elapsed
// time. Healthy requests stop the timer. This observes a stall; it cannot cancel
// an uninterruptible filesystem call or extend the request's deadline.
func watchHealth(report func(string, time.Duration), threshold time.Duration) (func(string), func()) {
	if report == nil {
		return func(string) {}, func() {}
	}
	started := time.Now()
	var stage atomic.Value
	stage.Store("ledger")
	timer := time.AfterFunc(threshold, func() { report(stage.Load().(string), time.Since(started)) })
	return func(value string) { stage.Store(value) }, func() { timer.Stop() }
}
