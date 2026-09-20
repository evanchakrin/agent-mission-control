//go:build windows

package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type scriptedService struct {
	states            []svc.State
	queries, controls int
	controlErr        error
}

func (s *scriptedService) Query() (svc.Status, error) {
	i := s.queries
	s.queries++
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	return svc.Status{State: s.states[i]}, nil
}
func (s *scriptedService) Control(cmd svc.Cmd) (svc.Status, error) {
	s.controls++
	if cmd != svc.Stop {
		panic("unexpected control")
	}
	return svc.Status{}, s.controlErr
}

func TestStopServiceWaitsForConcurrentStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		states     []svc.State
		controlErr error
		controls   int
	}{
		{"already stopped", []svc.State{svc.Stopped}, nil, 0},
		{"stop in progress", []svc.State{svc.StopPending, svc.Stopped}, nil, 0},
		{"ordinary stop", []svc.State{svc.Running, svc.Stopped}, nil, 1},
		{"concurrent stop", []svc.State{svc.Running, svc.StopPending, svc.Stopped}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL, 1},
		{"concurrent completed", []svc.State{svc.Running, svc.Stopped}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedService{states: tc.states, controlErr: tc.controlErr}
			if err := stopService(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if s.controls != tc.controls {
				t.Fatalf("controls=%d want %d", s.controls, tc.controls)
			}
		})
	}
}

func TestStopServiceDoesNotHideFailureOrCancellation(t *testing.T) {
	s := &scriptedService{states: []svc.State{svc.Running}, controlErr: windows.ERROR_ACCESS_DENIED}
	if err := stopService(context.Background(), s); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = &scriptedService{states: []svc.State{svc.Running}}
	if err := stopService(ctx, s); !errors.Is(err, context.Canceled) || s.queries != 0 || s.controls != 0 {
		t.Fatal(err, s)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s = &scriptedService{states: []svc.State{svc.StopPending}}
	if err := stopService(ctx, s); !errors.Is(err, context.DeadlineExceeded) || s.controls != 0 {
		t.Fatal(err, s)
	}
}
