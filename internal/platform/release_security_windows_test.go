//go:build windows

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestInstallationDescriptorRequiresTrustedOwnerAndNonWritableParents(t *testing.T) {
	for _, tc := range []struct {
		name, sddl string
		anchor, ok bool
	}{
		{"protected", "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)", false, true},
		{"owner-can-rewrite-dacl", "O:" + testOwnerSID + "D:P(A;;FR;;;" + testOwnerSID + ")(A;;FA;;;BA)", false, false},
		{"user-can-write", "O:BAD:P(A;;FA;;;BA)(A;;FW;;;BU)", false, false},
		{"parent-delete-child", "O:BAD:P(A;;FA;;;BA)(A;;0x40;;;BU)", true, false},
		{"parent-change-dacl", "O:BAD:P(A;;FA;;;BA)(A;;WD;;;BU)", true, false},
		{"system-anchor-create-only", "O:BAD:P(A;;FA;;;BA)(A;;0x4;;;BU)", true, true},
		{"application-create-not-allowed", "O:BAD:P(A;;FA;;;BA)(A;;0x4;;;BU)", false, false},
		{"inherit-only-not-current-object", "O:BAD:P(A;;FA;;;BA)(A;OIIO;FA;;;CO)(A;;FRFX;;;BU)", true, true},
		{"missing-dacl", "O:BA", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = verifyInstallationDescriptor(sd, tc.anchor)
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v expected=%v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestProductionAnchorsIgnoreCallerEnvironment(t *testing.T) {
	fake := t.TempDir()
	t.Setenv("ProgramFiles", fake)
	t.Setenv("ProgramFiles(x86)", fake)
	t.Setenv("ProgramData", fake)
	if _, err := releaseAnchor(filepath.Join(fake, "AgentMissionControl", "releases")); err == nil {
		t.Fatal("environment value became a trusted release anchor")
	}
	if _, err := configAnchor(filepath.Join(fake, "AgentMissionControl", "config", "hub.json")); err == nil {
		t.Fatal("environment value became a trusted configuration anchor")
	}
}

func TestProductionStagingRefusesUnprotectedExistingParentWithoutChanges(t *testing.T) {
	parent := t.TempDir()
	keep := filepath.Join(parent, "keep.txt")
	if err := os.WriteFile(keep, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "candidate")
	if err = ensureProtectedDirectoryTree(parent, target); err == nil {
		t.Fatal("untrusted parent was accepted")
	}
	after, err := windows.GetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("failed staging altered existing parent permissions")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("failed staging created a directory")
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "preserve" {
		t.Fatal("failed staging altered unrelated content")
	}
}

func TestProductionConfigNeverStagesIntoTemporaryUserFolders(t *testing.T) {
	c := fixtureConfig(t)
	c.Role = Hub
	c.Sources = nil
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := StageServiceConfig(path, c); err == nil {
		t.Fatal("production config accepted a user-owned temporary path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refused production config left a file")
	}
}

func TestCanceledServiceCommandsNeverReachSCM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, fn := range []func(context.Context, Role) error{StartService, StopService, UninstallService} {
		if err := fn(ctx, Hub); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled operation reached platform boundary: %v", err)
		}
	}
}
