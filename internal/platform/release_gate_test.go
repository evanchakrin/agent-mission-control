package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func releaseGateFixture(t *testing.T) (ReleaseGateManifest, string, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "synthetic-binary")
	evidence := filepath.Join(dir, "synthetic-evidence.json")
	if err := os.WriteFile(exe, []byte("unit fixture only; not an executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence, []byte(`{"fixture":true,"notProductionAcceptance":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	exeHash, _ := gateFileHash(exe, 1024)
	evidenceHash, _ := gateFileHash(evidence, 1024)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	m := ReleaseGateManifest{Version: 1, ReleaseVersion: "unit-test-only", Platform: "windows/amd64", ExecutableSHA256: exeHash, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	for _, id := range RequiredReleaseGates() {
		m.Gates = append(m.Gates, ReleaseGate{ID: id, Status: "passed", CompletedAt: now.Add(-2 * time.Hour), Evidence: []ReleaseEvidence{{Path: filepath.Base(evidence), SHA256: evidenceHash}}})
	}
	return m, filepath.Join(dir, "synthetic-manifest.json"), exe, now
}
func writeGateFixture(t *testing.T, path string, m ReleaseGateManifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestReleaseGateBindsEveryGateAndEvidenceToExactBinary(t *testing.T) {
	m, path, exe, now := releaseGateFixture(t)
	writeGateFixture(t, path, m)
	if _, err := verifyReleaseGateAt(path, exe, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("different bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseGateAt(path, exe, now); err == nil {
		t.Fatal("different executable reused acceptance")
	}
}

func TestSingleOwnerReleaseCriteria(t *testing.T) {
	got := RequiredReleaseGates()
	want := []string{"fleet-table-machines-projects", "accounting-economics-analytics", "durable-organization", "backup-restore-migration", "durable-ack-crash-boundaries", "organization-ingestion-isolation"}
	if len(got) != len(want) {
		t.Fatalf("unexpected admission gates: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected admission gates: %v", got)
		}
	}
	got[0] = "changed"
	if RequiredReleaseGates()[0] != want[0] {
		t.Fatal("caller changed release criteria")
	}
}
func TestReleaseGateFailsClosedOnIncompleteStaleOrEscapingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ReleaseGateManifest)
	}{
		{"missing-gate", func(m *ReleaseGateManifest) { m.Gates = m.Gates[:len(m.Gates)-1] }},
		{"duplicate-gate", func(m *ReleaseGateManifest) { m.Gates[1].ID = m.Gates[0].ID }},
		{"pending-gate", func(m *ReleaseGateManifest) { m.Gates[0].Status = "pending" }},
		{"no-evidence", func(m *ReleaseGateManifest) { m.Gates[0].Evidence = nil }},
		{"wrong-evidence-hash", func(m *ReleaseGateManifest) { m.Gates[0].Evidence[0].SHA256 = string(make([]byte, 64)) }},
		{"evidence-escape", func(m *ReleaseGateManifest) { m.Gates[0].Evidence[0].Path = "../outside.json" }},
		{"expired", func(m *ReleaseGateManifest) { m.ExpiresAt = m.CreatedAt }},
		{"unknown-platform", func(m *ReleaseGateManifest) { m.Platform = "windows/arm64" }},
		{"missing-timestamp", func(m *ReleaseGateManifest) { m.Gates[0].CompletedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, path, exe, now := releaseGateFixture(t)
			tc.change(&m)
			writeGateFixture(t, path, m)
			if _, err := verifyReleaseGateAt(path, exe, now); err == nil {
				t.Fatal("invalid release was accepted")
			}
		})
	}
}
func TestReleaseGateRejectsModifiedEvidenceAndUnknownFields(t *testing.T) {
	m, path, exe, now := releaseGateFixture(t)
	writeGateFixture(t, path, m)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), m.Gates[0].Evidence[0].Path), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseGateAt(path, exe, now); err == nil {
		t.Fatal("modified evidence was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseGateAt(path, exe, now); err == nil {
		t.Fatal("unknown manifest fields were accepted")
	}
}
