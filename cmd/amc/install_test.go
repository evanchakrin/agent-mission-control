package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

// Synthetic evidence is consumed only by mock registrars. It is not a release
// certificate and no SCM or Task Scheduler operation is called by these tests.
func installFixture(t *testing.T) (string, string, platform.ReleaseGateManifest) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "not-an-executable")
	manifest := filepath.Join(dir, "fixture.json")
	bytes := []byte("isolated unit fixture, not production acceptance")
	if err := os.WriteFile(exe, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(bytes)
	digest := hex.EncodeToString(hash[:])
	now := time.Now().UTC()
	m := platform.ReleaseGateManifest{Version: 1, ReleaseVersion: "unit-test-only", Platform: "windows/amd64", ExecutableSHA256: digest, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, id := range platform.RequiredReleaseGates() {
		m.Gates = append(m.Gates, platform.ReleaseGate{ID: id, Status: "passed", CompletedAt: now.Add(-time.Minute), Evidence: []platform.ReleaseEvidence{{Path: filepath.Base(exe), SHA256: digest}}})
	}
	writeInstallFixture(t, manifest, m)
	return exe, manifest, m
}
func writeInstallFixture(t *testing.T, path string, m platform.ReleaseGateManifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestInstallRoutesOnlyVerifiedReleaseToRequestedRegistrar(t *testing.T) {
	exe, manifest, _ := installFixture(t)
	for _, role := range []platform.Role{platform.Hub, platform.Collector, platform.Desktop} {
		called := 0
		config := platform.Config{Role: role, OwnerSID: "fixture-owner"}
		configPath := filepath.Join(t.TempDir(), "config.json")
		actions := installActions{service: func(s platform.ServiceSpec) error {
			called++
			wantManifest := manifest
			if role == platform.Collector {
				wantManifest = ""
			}
			if role == platform.Desktop || s.Config.Role != role || s.ConfigPath != configPath || s.Executable != exe || s.GateManifest != wantManifest {
				t.Fatal(s)
			}
			return nil
		}, desktop: func(_ context.Context, s platform.DesktopSpec) error {
			called++
			if role != platform.Desktop || s.Executable != exe || s.ConfigPath != configPath || s.OwnerSID != config.OwnerSID {
				t.Fatal(s)
			}
			return nil
		}}
		if err := installVerifiedRelease(context.Background(), config, configPath, exe, manifest, actions); err != nil || called != 1 {
			t.Fatal(role, called, err)
		}
	}
}

func TestCollectorInstallDoesNotRequireHubAcceptanceEvidence(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "amc.exe")
	if err := os.WriteFile(exe, []byte("collector fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	called := 0
	actions := installActions{
		service: func(s platform.ServiceSpec) error {
			called++
			if s.Config.Role != platform.Collector || s.Executable != exe || s.GateManifest != "" {
				t.Fatal(s)
			}
			return nil
		},
		desktop: func(context.Context, platform.DesktopSpec) error {
			t.Fatal("collector routed to desktop registrar")
			return nil
		},
	}
	if err := installVerifiedRelease(context.Background(), platform.Config{Role: platform.Collector}, "collector.json", exe, "", actions); err != nil || called != 1 {
		t.Fatal(called, err)
	}
}

func TestHubAndDesktopStillRequireAcceptanceEvidence(t *testing.T) {
	for _, role := range []platform.Role{platform.Hub, platform.Desktop} {
		called := 0
		actions := installActions{service: func(platform.ServiceSpec) error { called++; return nil }, desktop: func(context.Context, platform.DesktopSpec) error { called++; return nil }}
		err := installVerifiedRelease(context.Background(), platform.Config{Role: role}, "config.json", "amc.exe", "", actions)
		if err == nil || called != 0 {
			t.Fatal(role, called, err)
		}
	}
}
func TestInstallRejectsMissingPendingOrTamperedEvidenceBeforeRegistration(t *testing.T) {
	for _, damage := range []string{"missing-paths", "missing-file", "pending", "tampered", "wrong-role"} {
		t.Run(damage, func(t *testing.T) {
			exe, manifest, m := installFixture(t)
			role := platform.Hub
			switch damage {
			case "missing-paths":
				manifest = ""
			case "missing-file":
				manifest = filepath.Join(t.TempDir(), "absent.json")
			case "pending":
				m.Gates[0].Status = "pending"
				writeInstallFixture(t, manifest, m)
			case "tampered":
				if err := os.WriteFile(exe, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-role":
				role = "other"
			}
			called := 0
			actions := installActions{service: func(platform.ServiceSpec) error { called++; return nil }, desktop: func(context.Context, platform.DesktopSpec) error { called++; return nil }}
			err := installVerifiedRelease(context.Background(), platform.Config{Role: role}, "unused", exe, manifest, actions)
			if err == nil || called != 0 {
				t.Fatal(damage, called, err)
			}
		})
	}
}
