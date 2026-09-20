package store

import (
	"context"
	"errors"
	"sync"
)

var ErrWriterQueueFull = errors.New("store: writer admission queue full")

const normalWriterLimit = 64
const priorityWriterLimit = 16
const priorityWriterBurst = 4

type writerWaiter struct {
	ready   chan struct{}
	granted bool
}

// writerMutex bounds admitted work and favors interactive organization writes.
// Four priority grants at most precede a queued normal grant. No goroutine is
// created by admission, and cancellation never preempts an active transaction.
type writerMutex struct {
	mu       sync.Mutex
	held     bool
	normal   []*writerWaiter
	priority []*writerWaiter
	burst    int
	space    chan struct{}
}

func (m *writerMutex) LockContext(ctx context.Context) error {
	return m.lock(ctx, false, false)
}

func (m *writerMutex) LockPriorityContext(ctx context.Context) error {
	return m.lock(ctx, true, false)
}

// Legacy synchronous callers wait outside the admitted queue when it is full.
// Their calling goroutines already exist; admission allocates no waiter until
// there is room. Service request concurrency is bounded separately by the hub.
func (m *writerMutex) Lock() { _ = m.lock(context.Background(), false, true) }

func (m *writerMutex) lock(ctx context.Context, priority, waitForSpace bool) error {
	m.mu.Lock()
	for {
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return err
		}
		if !m.held {
			m.held = true
			m.burst = 0
			m.mu.Unlock()
			return nil
		}
		queue, limit := &m.normal, normalWriterLimit
		if priority {
			queue, limit = &m.priority, priorityWriterLimit
		}
		if len(*queue) < limit {
			break
		}
		if !waitForSpace {
			m.mu.Unlock()
			return ErrWriterQueueFull
		}
		if m.space == nil {
			m.space = make(chan struct{})
		}
		space := m.space
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-space:
		}
		m.mu.Lock()
	}
	w := &writerWaiter{ready: make(chan struct{})}
	if priority {
		m.priority = append(m.priority, w)
	} else {
		m.normal = append(m.normal, w)
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-w.ready:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if w.granted {
			m.advance()
		} else {
			queue := &m.normal
			if priority {
				queue = &m.priority
			}
			for i, candidate := range *queue {
				if candidate == w {
					copy((*queue)[i:], (*queue)[i+1:])
					(*queue)[len(*queue)-1] = nil
					*queue = (*queue)[:len(*queue)-1]
					m.notifySpace()
					break
				}
			}
		}
		return err
	}
	return nil
}

func (m *writerMutex) Unlock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held {
		panic("store: unlock of unlocked writer mutex")
	}
	m.advance()
}

func (m *writerMutex) notifySpace() {
	if m.space != nil {
		close(m.space)
		m.space = nil
	}
}

// Called with mu held; ownership transfers before the next caller wakes.
func (m *writerMutex) advance() {
	queue := &m.normal
	if len(m.priority) > 0 && (len(m.normal) == 0 || m.burst < priorityWriterBurst) {
		queue = &m.priority
		m.burst++
	} else {
		m.burst = 0
	}
	if len(*queue) == 0 {
		m.held = false
		m.notifySpace()
		return
	}
	w := (*queue)[0]
	copy(*queue, (*queue)[1:])
	(*queue)[len(*queue)-1] = nil
	*queue = (*queue)[:len(*queue)-1]
	w.granted = true
	close(w.ready)
	m.notifySpace()
	if m.burst > priorityWriterBurst {
		m.burst = priorityWriterBurst
	}
}
