//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopTaskStateCSV(t *testing.T) {
	state, err := taskCSVState([]byte("\"HOST\",\"\\AMC Desktop SID\",\"N/A\",\"Running\",\"Interactive only\"\r\n"))
	if err != nil || state != "Running" {
		t.Fatalf("unexpected task status: %q %v", state, err)
	}
	if _, err = taskCSVState([]byte("bad")); err == nil {
		t.Fatal("accepted malformed task state")
	}
}

func TestProtectedHubConfigPublishesAsUnelevatedOwner(t *testing.T) {
	c := fixtureConfig(t)
	c.Role = Hub
	c.Sources = nil
	var err error
	c.OwnerSID, err = CurrentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = WriteProtectedConfig(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Token != c.Token {
		t.Fatalf("config did not round-trip: %v", err)
	}
	if err = WriteProtectedConfig(path, c); err == nil {
		t.Fatal("existing protected config was overwritten")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("staging file leaked: %v %v", files, err)
	}
}

func TestDesktopIdentityRejectsDifferentOwnerOrElevation(t *testing.T) {
	sid, err := CurrentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateDesktopIdentity(sid)
	if windows.GetCurrentProcessToken().IsElevated() {
		if err == nil {
			t.Fatal("elevated desktop identity accepted")
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if ValidateDesktopIdentity("S-1-5-21-9-8-7-9999") == nil {
		t.Fatal("another owner identity accepted")
	}
}

func TestOwnerPipeRoundTripAndPathRestrictions(t *testing.T) {
	sid, err := CurrentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test-%d-%d", os.Getpid(), time.Now().UnixNano())
	l, err := ListenOwnerPipe(name, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	serverDone := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 4)
		_, err = io.ReadFull(c, b)
		if err == nil {
			_, err = c.Write(b)
		}
		serverDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := DialOwnerPipe(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err = io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("pipe response: %q %v", b, err)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`\\server\pipe\abc`, `../abc`, `a/b`, ""} {
		if _, err := DialOwnerPipe(ctx, bad); err == nil {
			t.Fatalf("accepted nonlocal pipe %q", bad)
		}
	}
	if l, err := ListenOwnerPipe(name+"-public", "S-1-1-0"); err == nil {
		l.Close()
		t.Fatal("accepted Everyone as pipe owner")
	}
}
func TestWorkerMemoryIsLimitedByJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestWorkerMemoryHelper$")
	cmd.Env = append(os.Environ(), "AMC_PLATFORM_MEMORY_TEST=1")
	w, err := StartWorker(ctx, WorkerSpec{Command: cmd, MemoryBytes: 80 * 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	err = w.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("expected allocation refusal exit 23, got %v", err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("worker timed out instead of hitting its memory limit")
	}
}
func TestWorkerMemoryHelper(t *testing.T) {
	if os.Getenv("AMC_PLATFORM_MEMORY_TEST") != "1" {
		return
	}
	for i := 0; i < 12; i++ {
		if _, err := windows.VirtualAlloc(0, 16*1024*1024, windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
			os.Exit(23)
		}
	}
	os.Exit(0)
}
func TestSCMManualStopAndFatalExitHaveDifferentStatus(t *testing.T) {
	t.Run("manual-stop", func(t *testing.T) {
		requests := make(chan svc.ChangeRequest, 1)
		updates := make(chan svc.Status, 10)
		requests <- svc.ChangeRequest{Cmd: svc.Stop}
		h := &serviceHandler{ctx: context.Background(), run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		specific, code := h.Execute(nil, requests, updates)
		if specific || code != 0 {
			t.Fatalf("manual stop would trigger recovery: %v/%d", specific, code)
		}
	})
	t.Run("fatal-exit", func(t *testing.T) {
		h := &serviceHandler{ctx: context.Background(), run: func(context.Context) error { return errors.New("fatal") }}
		specific, code := h.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 10))
		if !specific || code == 0 || h.err == nil {
			t.Fatalf("fatal exit did not request recovery: %v/%d %v", specific, code, h.err)
		}
	})
}
