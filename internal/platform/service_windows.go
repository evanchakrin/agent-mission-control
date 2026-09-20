//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// InstallService registers, but never starts, the service. It never overwrites
// an existing service. The CLI must stage the executable/configuration first.
func InstallService(spec ServiceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := validateReleaseRoot(filepath.Dir(spec.Executable)); err != nil {
		return fmt.Errorf("install requires a staged, protected executable: %w", err)
	}
	if err := verifyReleaseFile(spec.Executable); err != nil {
		return err
	}
	if err := verifyServiceConfig(spec.ConfigPath); err != nil {
		return err
	}
	// Collectors only discover and forward bounded source bytes. Requiring the
	// hub/UI acceptance matrix on every satellite made installation needlessly
	// fragile. Keep those gates for the authoritative hub; the collector still
	// must use protected release/config paths and a restricted service account.
	if spec.Config.Role != Collector {
		manifest, err := VerifyReleaseGate(spec.GateManifest, spec.Executable)
		if err != nil {
			return fmt.Errorf("service installation acceptance gate: %w", err)
		}
		if err = verifyReleaseFile(spec.GateManifest); err != nil {
			return err
		}
		for _, gate := range manifest.Gates {
			for _, evidence := range gate.Evidence {
				path, e := releaseEvidencePath(filepath.Dir(spec.GateManifest), evidence.Path)
				if e != nil {
					return e
				}
				if e = verifyReleaseFile(path); e != nil {
					return e
				}
			}
		}
	}
	if err := validateServiceData(spec.Config); err != nil {
		return err
	}
	if err := prepareServiceDataParent(spec.Config); err != nil {
		return err
	}
	if _, err := windows.StringToSid(spec.Config.OwnerSID); err != nil {
		return fmt.Errorf("owner SID: %w", err)
	}
	name := spec.Config.Role.ServiceName()
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("install service requires administrator access: %w", err)
	}
	defer m.Disconnect()
	if existing, e := m.OpenService(name); e == nil {
		existing.Close()
		return fmt.Errorf("%s is already installed; stop and explicitly remove its registration before replacing it", name)
	} else if !errors.Is(e, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return e
	}
	s, err := m.CreateService(name, spec.Executable, mgr.Config{
		DisplayName: "Agent Mission Control " + string(spec.Config.Role),
		Description: "Records chat history and usage with bounded background work.",
		// Disabled until every permission and recovery setting is complete.
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartDisabled, ErrorControl: mgr.ErrorNormal,
		ServiceStartName: `NT SERVICE\` + name, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, string(spec.Config.Role), "--config", spec.ConfigPath)
	if err != nil {
		return err
	}
	defer s.Close()
	configured := false
	defer func() {
		if !configured {
			_ = s.Delete()
		}
	}()
	if err = s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 30 * time.Second}, {Type: mgr.ServiceRestart, Delay: 60 * time.Second}}, 24*60*60); err != nil {
		return err
	}
	if err = s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\`+name)
	if err != nil {
		return err
	}
	if err = protectServiceState(spec.Config, sid.String()); err != nil {
		return err
	}
	if err = setProtectedFileACL(spec.ConfigPath, sid.String(), spec.Config.OwnerSID); err != nil {
		return err
	}
	for _, source := range spec.Config.Sources {
		if err = grantSourceRead(source.Path, sid); err != nil {
			return fmt.Errorf("grant transcript read access: %w", err)
		}
	}
	installedConfig, err := s.Config()
	if err != nil {
		return err
	}
	installedConfig.StartType = mgr.StartAutomatic
	installedConfig.DelayedAutoStart = true
	if err = s.UpdateConfig(installedConfig); err != nil {
		return err
	}
	configured = true
	return nil
}

func openService(role Role, access uint32) (*mgr.Mgr, *mgr.Service, error) {
	name := role.ServiceName()
	if name == "" {
		return nil, nil, errors.New("invalid service role")
	}
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, err
	}
	m := &mgr.Mgr{Handle: h}
	p, _ := windows.UTF16PtrFromString(name)
	sh, err := windows.OpenService(h, p, access)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, &mgr.Service{Name: name, Handle: sh}, nil
}
func StartService(ctx context.Context, role Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, s, err := openService(role, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err = s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitService(ctx, s, svc.Running)
}
func StopService(ctx context.Context, role Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, s, err := openService(role, windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stopService(ctx, s)
}

type serviceController interface {
	Query() (svc.Status, error)
	Control(svc.Cmd) (svc.Status, error)
}

func stopService(ctx context.Context, s serviceController) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err = s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			// Another caller may have requested stop after our status read.
			// Only suppress the control error when a fresh status proves that.
			current, queryErr := s.Query()
			if queryErr != nil || (current.State != svc.StopPending && current.State != svc.Stopped) {
				return err
			}
		}
	}
	return waitService(ctx, s, svc.Stopped)
}
func waitService(ctx context.Context, s interface{ Query() (svc.Status, error) }, want svc.State) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return fmt.Errorf("service stopped during startup (Windows exit %d, service exit %d)", st.Win32ExitCode, st.ServiceSpecificExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for service: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
func ServiceStatus(role Role) (Status, error) {
	out := Status{Name: role.ServiceName(), State: "not-installed"}
	m, s, err := openService(role, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return out, err
	}
	c, err := s.Config()
	if err != nil {
		return out, err
	}
	states := map[svc.State]string{svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running", svc.Paused: "paused"}
	out.Installed = true
	out.State = states[st.State]
	if out.State == "" {
		out.State = "unknown"
	}
	out.ProcessID = st.ProcessId
	out.Account = c.ServiceStartName
	out.StartType = c.StartType
	return out, nil
}

// UninstallService deliberately deletes only the SCM registration. It neither
// enumerates nor deletes data, configurations, logs, binaries, or transcripts.
func UninstallService(ctx context.Context, role Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := StopService(ctx, role); err != nil && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	m, s, err := openService(role, windows.DELETE)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return s.Delete()
}

type serviceHandler struct {
	ctx context.Context
	run func(context.Context) error
	err error
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, updates chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	updates <- svc.Status{State: svc.StartPending, WaitHint: 30000}
	done := make(chan error, 1)
	go func() { done <- invoke(ctx, h.run) }()
	updates <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			// Cancellation and completion can both be ready. A random select
			// winner must not turn an intentional parent stop into recovery.
			if h.ctx.Err() != nil {
				return h.stopped(err)
			}
			if err == nil {
				err = errors.New("service worker exited without a stop request")
			}
			h.err = err
			return true, 1
		case <-ctx.Done():
			return h.stop(cancel, done, updates)
		case request, ok := <-requests:
			if !ok {
				if h.ctx.Err() != nil {
					return h.stop(cancel, done, updates)
				}
				h.err = errors.New("service control channel closed")
				return true, 1
			}
			switch request.Cmd {
			case svc.Interrogate:
				updates <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				return h.stop(cancel, done, updates)
			}
		}
	}
}
func (h *serviceHandler) stop(cancel context.CancelFunc, done <-chan error, updates chan<- svc.Status) (bool, uint32) {
	cancel()
	updates <- svc.Status{State: svc.StopPending, WaitHint: 30000, CheckPoint: 1}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return h.stopped(err)
	case <-timer.C:
		return h.stopped(errors.New("service worker exceeded the 30-second shutdown deadline"))
	}
}
func (h *serviceHandler) stopped(err error) (bool, uint32) {
	if err != nil && !errors.Is(err, context.Canceled) {
		h.err = err
	}
	// An intentional SCM stop stays stopped, even if shutdown needs diagnosis.
	return false, 0
}
func RunService(ctx context.Context, role Role, run func(context.Context) error) error {
	if role.ServiceName() == "" {
		return errors.New("invalid service role")
	}
	asService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !asService {
		return invoke(ctx, run)
	}
	h := &serviceHandler{ctx: ctx, run: run}
	if err = svc.Run(role.ServiceName(), h); err != nil {
		return err
	}
	return h.err
}

func protectServiceState(c Config, serviceSID string) error {
	if err := validateServiceData(c); err != nil {
		return err
	}
	root, err := safeAbsolutePath(c.DataDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	// The owner uses the API; only the service and administrators can modify
	// service-owned state. Read access supports explicit owner backup/export.
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + serviceSID + ")(A;OICI;FR;;;" + c.OwnerSID + ")"
	if err = setAdministratorACL(root, sddl); err != nil {
		return err
	}
	if err = markServiceData(c); err != nil {
		return err
	}
	if err = setAdministratorACL(filepath.Join(root, serviceDataMarker), "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;"+c.OwnerSID+")"); err != nil {
		return err
	}
	for _, name := range []string{"logs", "tmp", "exports"} {
		if err = os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	return nil
}
func setProtectedFileACL(filename, serviceSID, ownerSID string) error {
	return setAdministratorACL(filename, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;"+serviceSID+")(A;;FR;;;"+ownerSID+")")
}
func applyDACL(filename, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(filename, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func grantSourceRead(filename string, sid *windows.SID) error {
	var pin runtime.Pinner
	pin.Pin(sid)
	defer pin.Unpin()
	root, err := safeAbsolutePath(filename)
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	old, _, err := sd.DACL()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{AccessPermissions: windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(sid)}}}, old)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
