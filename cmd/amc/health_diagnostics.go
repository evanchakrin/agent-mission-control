package main

import (
	"database/sql"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

const stallStackLimit = 64 << 10

// Diagnostic stacks remain in the private rotated service log. Bound both
// capture size and frequency; never include request parameters or bodies. Three
// fixed lanes allow at most three 64 KiB captures per minute: organization and
// totals requests cannot lose all their evidence to routine health stalls.
func slowRequestReporter(logger *slog.Logger, stats func() sql.DBStats) func(string, time.Duration) {
	var mu sync.Mutex
	var lastStack [3]time.Time
	return func(stage string, elapsed time.Duration) {
		now := time.Now()
		lane := 0
		if strings.HasPrefix(stage, "organization-") {
			lane = 1
		} else if stage == "totals-query" {
			lane = 2
		}
		mu.Lock()
		capture := lastStack[lane].IsZero() || now.Sub(lastStack[lane]) >= time.Minute
		if capture {
			lastStack[lane] = now
		}
		mu.Unlock()
		attrs := []any{"stage", stage, "elapsedMilliseconds", elapsed.Milliseconds(), "databasePool", stats()}
		if capture {
			buf := make([]byte, stallStackLimit)
			n := runtime.Stack(buf, true)
			attrs = append(attrs, "goroutineStacks", string(buf[:n]), "stacksMayBeTruncated", n == len(buf))
		}
		logger.Warn("hub request is slow", attrs...)
	}
}
