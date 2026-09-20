package platform

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// WorkerSpec only supervises an explicitly constructed AMC worker command.
// The caller owns the protocol and must validate worker inputs separately.
type WorkerSpec struct {
	Command     *exec.Cmd
	MemoryBytes uint64
}
type Worker struct {
	Command       *exec.Cmd
	done          chan struct{}
	err           error
	terminate     func() error
	once          sync.Once
	stopErr       error
	stopRequested atomic.Bool
}

func (w *Worker) Wait() error           { <-w.done; return w.err }
func (w *Worker) Done() <-chan struct{} { return w.done }
func (w *Worker) Stop() error           { w.stopRequested.Store(true); return w.closeJob() }
func (w *Worker) closeJob() error       { w.once.Do(func() { w.stopErr = w.terminate() }); return w.stopErr }
func (w *Worker) Close() error          { return w.Stop() }
func validateWorker(spec WorkerSpec) error {
	if spec.Command == nil || !filepath.IsAbs(spec.Command.Path) {
		return errors.New("worker executable must be an explicit absolute path")
	}
	if spec.Command.Process != nil {
		return errors.New("worker command has already started")
	}
	return nil
}
func finishWorker(ctx context.Context, w *Worker) {
	go func() {
		w.err = w.Command.Wait()
		if w.err == nil {
			if ctx.Err() != nil {
				w.err = ctx.Err()
			} else if w.stopRequested.Load() {
				w.err = context.Canceled
			}
		}
		_ = w.closeJob()
		close(w.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = w.Stop()
		case <-w.done:
		}
	}()
}
