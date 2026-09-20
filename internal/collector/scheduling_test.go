package collector

import (
	"context"
	"testing"
	"time"
)

func TestUploadSignalsAreIndependentCoalescedAndInterruptIdleWait(t *testing.T) {
	c, _ := testCollector(t, nil)
	for range 100 {
		c.wakeUploads()
	}
	for i, signal := range c.uploadNotify {
		if len(signal) != 1 {
			t.Fatalf("worker %d signal count %d", i, len(signal))
		}
		if !waitForWork(context.Background(), signal, time.Hour) {
			t.Fatal("signal failed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- waitForWork(ctx, c.uploadNotify[0], time.Hour) }()
	select {
	case <-done:
		t.Fatal("idle worker woke without work")
	case <-time.After(100 * time.Millisecond):
	}
	c.wakeUploads()
	select {
	case worked := <-done:
		if !worked {
			t.Fatal("work signal was cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("work did not interrupt idle wait")
	}
	if len(c.uploadNotify[1]) != 1 {
		t.Fatal("first worker consumed second worker's signal")
	}
	cancel()
	// No signal competes with cancellation in this check.
	if waitForWork(ctx, make(chan struct{}), time.Hour) {
		t.Fatal("cancelled wait continued")
	}
	if !waitForWork(context.Background(), make(chan struct{}), time.Millisecond) {
		t.Fatal("retry fallback disabled")
	}
}

func TestCommittedCaptureWakesBothUploadWorkers(t *testing.T) {
	c, root := testCollector(t, nil)
	writeSource(t, root, "rollout.jsonl", []byte("{\"id\":\"fixture\"}\n"))
	reconcile(t, c)
	for _, signal := range c.uploadNotify {
		if len(signal) != 0 {
			t.Fatal("discovery masqueraded as committed capture")
		}
	}
	capture(t, c, 1)
	for _, signal := range c.uploadNotify {
		if len(signal) != 1 {
			t.Fatal("durable capture did not wake upload worker")
		}
	}
}
