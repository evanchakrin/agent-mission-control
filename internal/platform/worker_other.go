//go:build !windows

package platform

import (
	"context"
	"syscall"
)

func StartWorker(ctx context.Context, spec WorkerSpec) (*Worker, error) {
	if err := validateWorker(spec); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The portable test runner supports process-group cleanup, but must not
	// claim a Windows-equivalent memory boundary that it has not installed.
	if spec.MemoryBytes != 0 {
		return nil, ErrUnsupported
	}
	cmd := spec.Command
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &Worker{Command: cmd, done: make(chan struct{}), terminate: func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }}
	finishWorker(ctx, w)
	return w, nil
}
