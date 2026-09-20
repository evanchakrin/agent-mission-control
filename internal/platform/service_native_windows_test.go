//go:build windows

package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// This helper exists only in the test binary. Production amc has no fixture
// registration command and its installation acceptance gates remain mandatory.
func TestNativeServiceWorker(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != "amc-native-worker" {
		t.Skip("SCM-only fixture worker")
	}
	if !strings.HasPrefix(args[1], "AMCFixture-") || (args[2] != "wait" && args[2] != "fail" && args[2] != "crash") {
		t.Fatal("invalid fixture worker arguments")
	}
	asService, err := svc.IsWindowsService()
	if err != nil || !asService {
		t.Fatal("fixture worker must be launched by SCM")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	wantSID, _, _, lookupErr := windows.LookupSID("", `NT SERVICE\`+args[1])
	if err != nil || lookupErr != nil || token.IsElevated() || !user.User.Sid.Equals(wantSID) {
		t.Fatal("fixture worker is not running under its unelevated virtual service account")
	}
	h := &serviceHandler{ctx: context.Background(), run: func(ctx context.Context) error {
		if args[2] == "wait" {
			<-ctx.Done()
			return ctx.Err()
		}
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if args[2] == "crash" {
				os.Exit(23) // Test-only abrupt process death, without service cleanup.
			}
			return errors.New("intentional native fixture worker failure")
		}
	}}
	if err := svc.Run(args[1], h); err != nil {
		t.Fatal(err)
	}
	if h.err != nil {
		t.Fatal(h.err)
	}
}

// Explicit opt-in plus an elevated Windows token are both required. This tests
// the real service handler under SCM, not hub/collector data or boot behavior.
func TestNativeServiceLifecycle(t *testing.T) {
	if testing.Short() || os.Getenv("AMC_NATIVE_SERVICE_TEST") != "1" {
		t.Skip("requires explicit AMC_NATIVE_SERVICE_TEST=1 and an elevated disposable Windows host")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("native service fixture requires administrator elevation; no registration attempted")
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(nonce[:])
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	release, err := StageRelease(executable, filepath.Join(programFiles, "AgentMissionControl", "native-fixtures"), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained fixture executable=%s sha256=%s; no production manifest generated", release.Executable, release.SHA256)
	for _, mode := range []string{"wait", "fail", "crash"} {
		t.Run(mode, func(t *testing.T) {
			name := "AMCFixture-" + id + "-" + mode
			s, err := m.CreateService(name, release.Executable, mgr.Config{
				DisplayName: name, Description: "Disposable AMC handler acceptance fixture; no history collection",
				ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartManual,
				ErrorControl: mgr.ErrorNormal, ServiceStartName: `NT SERVICE\` + name,
				SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
			}, "-test.run=^TestNativeServiceWorker$", "--", "amc-native-worker", name, mode)
			if err != nil {
				t.Fatal(err) // Never open/adopt an existing registration.
			}
			defer s.Close()
			installed, err := s.Config()
			if err != nil {
				t.Fatalf("read created fixture %s: %v; registration retained", name, err)
			}
			defer func() {
				current, err := s.Config()
				if err != nil || current.BinaryPathName != installed.BinaryPathName || current.ServiceStartName != installed.ServiceStartName {
					t.Errorf("fixture %s ownership changed or unreadable; refusing cleanup", name)
					return
				}
				// Disable starts as well as recovery: an already queued SCM restart
				// must not race cleanup after a failed assertion.
				current.StartType = mgr.StartDisabled
				if err := s.UpdateConfig(current); err != nil {
					t.Errorf("disable fixture %s: %v; registration retained", name, err)
					return
				}
				if err := s.ResetRecoveryActions(); err != nil {
					t.Errorf("disable fixture recovery %s: %v; registration retained", name, err)
					return
				}
				if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
					t.Errorf("stop fixture %s: %v; registration retained", name, err)
					return
				}
				if _, err := waitNativeService(s, 35*time.Second, func(st svc.Status) bool { return st.State == svc.Stopped }); err != nil {
					t.Errorf("fixture %s did not stop: %v; registration retained", name, err)
					return
				}
				if err := s.Delete(); err != nil {
					t.Errorf("delete fixture registration %s: %v", name, err)
				}
			}()
			if err := s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 30 * time.Second}, {Type: mgr.ServiceRestart, Delay: 60 * time.Second}}, 86400); err != nil {
				t.Fatal(err)
			}
			if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
				t.Fatal(err)
			}
			actions, err := s.RecoveryActions()
			if err != nil || len(actions) != 3 {
				t.Fatalf("read recovery configuration: actions=%v error=%v", actions, err)
			}
			for i, delay := range []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second} {
				if actions[i].Type != mgr.ServiceRestart || actions[i].Delay != delay {
					t.Fatalf("unexpected recovery action %d: %v", i, actions[i])
				}
			}
			if enabled, err := s.RecoveryActionsOnNonCrashFailures(); err != nil || !enabled {
				t.Fatalf("non-crash recovery is not enabled: %v", err)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			first, err := waitNativeService(s, 15*time.Second, func(st svc.Status) bool { return st.State == svc.Running && st.ProcessId != 0 })
			if err != nil {
				t.Fatal(err)
			}
			if mode != "wait" {
				// Observe all configured delays and one additional failure: SCM must
				// repeat the final action instead of exhausting the recovery list.
				for attempt, delay := range []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second} {
					if _, err := waitNativeService(s, 15*time.Second, func(st svc.Status) bool { return st.State == svc.Stopped }); err != nil {
						t.Fatalf("attempt %d did not stop after injected %s: %v", attempt+1, mode, err)
					}
					stoppedAt := time.Now()
					next, err := waitNativeService(s, delay+15*time.Second, func(st svc.Status) bool {
						return st.State == svc.Running && st.ProcessId != 0
					})
					elapsed := time.Since(stoppedAt)
					if err != nil {
						t.Fatalf("SCM recovery attempt %d (%s) failed: %v", attempt+1, delay, err)
					}
					// State polling is 100 ms; one second allows observation jitter.
					// The upper bound is enforced by waitNativeService above.
					if elapsed < delay-time.Second {
						t.Fatalf("SCM recovery attempt %d restarted too early: %s, expected %s", attempt+1, elapsed, delay)
					}
					t.Logf("%s recovery attempt=%d expected=%s observed=%s oldPID=%d newPID=%d", mode, attempt+1, delay, elapsed, first.ProcessId, next.ProcessId)
					first = next
				}
			}
			if _, err := s.Control(svc.Stop); err != nil {
				t.Fatal(err)
			}
			if _, err := waitNativeService(s, 35*time.Second, func(st svc.Status) bool { return st.State == svc.Stopped }); err != nil {
				t.Fatal(err)
			}
			observe := 7 * time.Second
			if mode != "wait" {
				observe = 65 * time.Second // Beyond the repeated final recovery delay.
			}
			deadline := time.Now().Add(observe)
			for time.Now().Before(deadline) {
				st, err := s.Query()
				if err != nil || st.State != svc.Stopped {
					t.Fatalf("deliberate stop did not stay stopped: state=%v error=%v", st.State, err)
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
	}
}

func waitNativeService(s *mgr.Service, timeout time.Duration, accept func(svc.Status) bool) (svc.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil || accept(st) {
			return st, err
		}
		if time.Now().After(deadline) {
			return st, errors.New("timed out waiting for fixture service state")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
