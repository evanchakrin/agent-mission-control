//go:build windows

package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestSCMParentCancellationDoesNotRaceIntoRecovery(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		requests := make(chan svc.ChangeRequest)
		close(requests)
		h := &serviceHandler{ctx: ctx, run: func(ctx context.Context) error { return ctx.Err() }}
		specific, code := h.Execute(nil, requests, make(chan svc.Status, 10))
		if specific || code != 0 || h.err != nil {
			t.Fatalf("deliberate parent cancellation requested recovery on iteration %d: %v/%d %v", i, specific, code, h.err)
		}
	}
}

func TestSCMUnexpectedWorkerCancellationStillRequestsRecovery(t *testing.T) {
	h := &serviceHandler{ctx: context.Background(), run: func(context.Context) error { return context.Canceled }}
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 10))
	if !specific || code == 0 || !errors.Is(h.err, context.Canceled) {
		t.Fatal("unsolicited worker cancellation suppressed recovery", specific, code, h.err)
	}
}

func TestSCMStopRetainsShutdownErrorWithoutRecovery(t *testing.T) {
	requests := make(chan svc.ChangeRequest, 1)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	want := errors.New("shutdown diagnostic")
	h := &serviceHandler{ctx: context.Background(), run: func(ctx context.Context) error { <-ctx.Done(); return want }}
	specific, code := h.Execute(nil, requests, make(chan svc.Status, 10))
	if specific || code != 0 || !errors.Is(h.err, want) {
		t.Fatal("manual stop lost its diagnostic or requested recovery", specific, code, h.err)
	}
}

func TestSCMUnexpectedCleanExitRequestsRecovery(t *testing.T) {
	h := &serviceHandler{ctx: context.Background(), run: func(context.Context) error { return nil }}
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 10))
	if !specific || code == 0 || h.err == nil {
		t.Fatal("unsolicited clean worker exit suppressed recovery")
	}
}

func TestSCMShutdownDeadlineRemainsStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises actual 30-second shutdown deadline")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &serviceHandler{ctx: ctx}
	started := time.Now()
	specific, code := h.stop(cancel, make(chan error), make(chan svc.Status, 1))
	elapsed := time.Since(started)
	if specific || code != 0 || h.err == nil || ctx.Err() == nil {
		t.Fatal("bounded intentional stop changed recovery semantics", specific, code, h.err)
	}
	if elapsed < 30*time.Second || elapsed > 35*time.Second {
		t.Fatal("shutdown deadline outside expected scheduling tolerance", elapsed)
	}
}
