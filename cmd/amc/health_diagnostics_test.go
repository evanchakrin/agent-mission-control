package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlowRequestReporterBoundsAndRateLimitsStacks(t *testing.T) {
	var output bytes.Buffer
	report := slowRequestReporter(slog.New(slog.NewJSONHandler(&output, nil)), func() sql.DBStats {
		return sql.DBStats{OpenConnections: 4, InUse: 4}
	})
	report("sessions-query", time.Second)
	report("ledger-source-offsets", 2*time.Second)
	report("organization-writer-admission", 250*time.Millisecond)
	report("organization-commit", 250*time.Millisecond)
	report("sessions-query", time.Second)
	decoder := json.NewDecoder(&output)
	for i := 0; i < 5; i++ {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		stack, present := record["goroutineStacks"].(string)
		if i == 0 || i == 2 {
			if !present || len(stack) > stallStackLimit || !strings.Contains(stack, "slowRequestReporter") {
				t.Fatal("first warning per lane must include a bounded diagnostic stack")
			}
		} else if present {
			t.Fatal("repeated warnings in the same lane must not recapture stacks within one minute")
		}
		if record["databasePool"].(map[string]any)["InUse"] != float64(4) {
			t.Fatal("missing database pool evidence")
		}
	}
}

func TestConcurrentSlowReportsKeepThreeFixedCaptureBudgets(t *testing.T) {
	var output bytes.Buffer
	report := slowRequestReporter(slog.New(slog.NewJSONHandler(&output, nil)), func() sql.DBStats { return sql.DBStats{} })
	var work sync.WaitGroup
	for i := 0; i < 100; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			stage := "ledger-source-offsets"
			if i%3 == 0 {
				stage = "organization-commit"
			} else if i%3 == 1 {
				stage = "totals-query"
			}
			report(stage, time.Second)
		}(i)
	}
	work.Wait()
	decoder := json.NewDecoder(&output)
	captures := 0
	for i := 0; i < 100; i++ {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if stack, present := record["goroutineStacks"].(string); present {
			captures++
			if len(stack) > stallStackLimit {
				t.Fatal("stack exceeded bound")
			}
		}
	}
	if captures != 3 {
		t.Fatal("expected exactly one capture per fixed lane", captures)
	}
}
