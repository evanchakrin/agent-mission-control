//go:build windows

package platform

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ValidateDesktopIdentity prevents a manual command launched from an elevated
// shell from turning the owner-only broker into a privileged write service.
func ValidateDesktopIdentity(ownerSID string) error {
	if windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("desktop must run unelevated under the interactive owner account")
	}
	current, err := CurrentOwnerSID()
	if err != nil {
		return err
	}
	if current != ownerSID {
		return fmt.Errorf("desktop configuration belongs to another Windows account")
	}
	return nil
}

func runTaskCommand(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "schtasks.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("task registration: %w: %.2000s", err, out)
	}
	return nil
}
func InstallDesktopTask(ctx context.Context, spec DesktopSpec) error {
	data, err := DesktopTaskXML(spec)
	if err != nil {
		return err
	}
	sid, err := CurrentOwnerSID()
	if err != nil {
		return err
	}
	if sid != spec.OwnerSID {
		return fmt.Errorf("desktop task owner must be the current Windows user")
	}
	name, _ := DesktopTaskName(spec.OwnerSID)
	f, err := os.CreateTemp("", "amc-desktop-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(desktopTaskFileBytes(data)); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Existing registration belongs to the owner; install must not overwrite it.
	return runTaskCommand(ctx, "/Create", "/TN", name, "/XML", f.Name())
}
func UninstallDesktopTask(ctx context.Context, ownerSID string) error {
	name, err := DesktopTaskName(ownerSID)
	if err != nil {
		return err
	}
	return runTaskCommand(ctx, "/Delete", "/TN", name, "/F")
}
func StartDesktopTask(ctx context.Context, ownerSID string) error {
	name, err := DesktopTaskName(ownerSID)
	if err != nil {
		return err
	}
	return runTaskCommand(ctx, "/Run", "/TN", name)
}

func StopDesktopTask(ctx context.Context, ownerSID string) error {
	name, err := DesktopTaskName(ownerSID)
	if err != nil {
		return err
	}
	return runTaskCommand(ctx, "/End", "/TN", name)
}

// DesktopTaskStatus reads Scheduler state only. State is the OS display value;
// callers must not interpret localized display text as an authorization check.
func DesktopTaskStatus(ctx context.Context, ownerSID string) (Status, error) {
	name, err := DesktopTaskName(ownerSID)
	status := Status{Name: name, State: "unknown", Account: ownerSID}
	if err != nil {
		return status, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", name, "/FO", "CSV", "/NH", "/V")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return status, fmt.Errorf("read desktop task state: %w: %.2000s", err, out)
	}
	state, err := taskCSVState(out)
	if err != nil {
		return status, err
	}
	status.Installed = true
	status.State = state
	return status, nil
}
func taskCSVState(data []byte) (string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	row, err := r.Read()
	if err != nil {
		return "", err
	}
	if len(row) < 4 || strings.TrimSpace(row[3]) == "" {
		return "", fmt.Errorf("unexpected task status response")
	}
	return strings.TrimSpace(row[3]), nil
}
