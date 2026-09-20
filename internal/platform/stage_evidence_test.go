package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func evidenceStagingFixture(t *testing.T) (string, string, ReleaseGateManifest) {
	t.Helper()
	m, path, exe, _ := releaseGateFixture(t)
	m.CreatedAt = time.Now().UTC()
	m.ExpiresAt = m.CreatedAt.Add(time.Hour)
	writeGateFixture(t, path, m)
	return path, exe, m
}
func testEvidenceFiles() evidenceStageFiles {
	return evidenceStageFiles{directory: func(p string) error { return os.Mkdir(p, 0700) }, file: func(p string) (*os.File, error) { return os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) }}
}

func TestEvidenceStagingPreservesVerifiedBytesAndRefusesOverwrite(t *testing.T) {
	source, exe, _ := evidenceStagingFixture(t)
	staged, err := stageReleaseEvidence(source, exe, testEvidenceFiles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyReleaseGate(staged, exe); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(staged))
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	before, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stageReleaseEvidence(source, exe, testEvidenceFiles()); err == nil {
		t.Fatal("overwrote staged evidence")
	}
	after, err := os.ReadFile(staged)
	if err != nil || string(before) != string(after) {
		t.Fatal("existing manifest changed", err)
	}
}

func TestEvidenceStagingRejectsPendingGateBeforeCreatingDirectory(t *testing.T) {
	source, exe, m := evidenceStagingFixture(t)
	m.Gates[0].Status = "pending"
	writeGateFixture(t, source, m)
	files := testEvidenceFiles()
	called := false
	files.directory = func(string) error { called = true; return nil }
	if _, err := stageReleaseEvidence(source, exe, files); err == nil || called {
		t.Fatal(called, err)
	}
}

func TestEvidenceStagingDoesNotPublishChangedOrFailedEvidence(t *testing.T) {
	for _, mode := range []string{"source-changed", "create-failed"} {
		t.Run(mode, func(t *testing.T) {
			source, exe, m := evidenceStagingFixture(t)
			files := testEvidenceFiles()
			if mode == "source-changed" {
				files.directory = func(p string) error {
					if err := os.WriteFile(filepath.Join(filepath.Dir(source), m.Gates[0].Evidence[0].Path), []byte("different fixture bytes"), 0600); err != nil {
						return err
					}
					return os.Mkdir(p, 0700)
				}
			} else {
				files.file = func(string) (*os.File, error) { return nil, errors.New("injected write failure") }
			}
			if result, err := stageReleaseEvidence(source, exe, files); err == nil || result != "" {
				t.Fatal(result, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "acceptance", "release-manifest.json")); !os.IsNotExist(err) {
				t.Fatal("failed evidence published manifest", err)
			}
		})
	}
}

func TestEvidenceStagingRetainsNestedRelativeReferences(t *testing.T) {
	source, exe, m := evidenceStagingFixture(t)
	old := filepath.Join(filepath.Dir(source), m.Gates[0].Evidence[0].Path)
	data, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(source), "nested")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for i := range m.Gates {
		m.Gates[i].Evidence[0].Path = "nested/result.json"
	}
	writeGateFixture(t, source, m)
	staged, err := stageReleaseEvidence(source, exe, testEvidenceFiles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyReleaseGate(staged, exe); err != nil {
		t.Fatal(err)
	}
}
