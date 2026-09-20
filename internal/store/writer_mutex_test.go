package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func waitWriterQueue(t *testing.T, m *writerMutex, normal, priority int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		n, p := len(m.normal), len(m.priority)
		m.mu.Unlock()
		if n == normal && p == priority {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue %d/%d, want %d/%d", n, p, normal, priority)
		}
		runtime.Gosched()
	}
}

func TestWriterPriorityPreservesFIFOAndNormalProgress(t *testing.T) {
	var m writerMutex
	m.Lock()
	order := make(chan string, 8)
	for i := 0; i < 2; i++ {
		go func(i int) { m.Lock(); order <- fmt.Sprintf("n%d", i); m.Unlock() }(i)
		waitWriterQueue(t, &m, i+1, 0)
	}
	for i := 0; i < 6; i++ {
		go func(i int) {
			if err := m.LockPriorityContext(context.Background()); err != nil {
				order <- err.Error()
				return
			}
			order <- fmt.Sprintf("p%d", i)
			m.Unlock()
		}(i)
		waitWriterQueue(t, &m, 2, i+1)
	}
	m.Unlock()
	for _, want := range []string{"p0", "p1", "p2", "p3", "n0", "p4", "p5", "n1"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("writer did not progress")
		}
	}
	waitWriterQueue(t, &m, 0, 0)
}

func TestWriterQueueLimitsCancellationAndLegacyOverflow(t *testing.T) {
	var m writerMutex
	m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, normalWriterLimit+priorityWriterLimit)
	for i := 0; i < normalWriterLimit; i++ {
		go func() { done <- m.LockContext(ctx) }()
	}
	for i := 0; i < priorityWriterLimit; i++ {
		go func() { done <- m.LockPriorityContext(ctx) }()
	}
	waitWriterQueue(t, &m, normalWriterLimit, priorityWriterLimit)
	if err := m.LockContext(context.Background()); !errors.Is(err, ErrWriterQueueFull) {
		t.Fatal(err)
	}
	if err := m.LockPriorityContext(context.Background()); !errors.Is(err, ErrWriterQueueFull) {
		t.Fatal(err)
	}
	if !IsContention(fmt.Errorf("wrapped: %w", ErrWriterQueueFull)) {
		t.Fatal("overflow is not retryable contention")
	}
	legacy := make(chan struct{})
	go func() { m.Lock(); m.Unlock(); close(legacy) }()
	cancel()
	for i := 0; i < normalWriterLimit+priorityWriterLimit; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queue cancellation blocked")
		}
	}
	// The synchronous caller may now be admitted, but cannot own the held lock.
	select {
	case <-legacy:
		t.Fatal("overflow bypassed active writer")
	default:
	}
	m.Unlock()
	select {
	case <-legacy:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy overflow never recovered")
	}
	if err := m.LockPriorityContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Unlock()
}

func TestWriterCanceledGrantReleasesOwnership(t *testing.T) {
	for i := 0; i < 100; i++ {
		var m writerMutex
		m.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			err := m.LockPriorityContext(ctx)
			if err == nil {
				m.Unlock()
			}
			done <- err
		}()
		waitWriterQueue(t, &m, 0, 1)
		m.mu.Lock()
		m.advance() // Grant while cancellation is racing with the wake-up.
		cancel()
		m.mu.Unlock()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("canceled grant stuck")
		}
		if err := m.LockContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.Unlock()
	}
}

func TestWriterAdmissionCancellationAndReuse(t *testing.T) {
	var m writerMutex
	m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.LockContext(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for active writer")
	}
	m.Unlock()
	if err := m.LockContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.LockContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Unlock()
}

func TestWriterAdmissionSerializesMixedCallers(t *testing.T) {
	var m writerMutex
	var wg sync.WaitGroup
	count := 0
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if i%2 == 0 {
					m.Lock()
				} else if err := m.LockContext(context.Background()); err != nil {
					t.Error(err)
					return
				}
				count++
				m.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1600 {
		t.Fatal(count)
	}
}
