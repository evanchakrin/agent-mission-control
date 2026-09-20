package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestStageEvidenceRequiresExplicitArtifacts(t *testing.T) {
	called := false
	stage := func(string, string) (string, error) { called = true; return "", nil }
	if err := runStageEvidence(nil, stage); err == nil || called {
		t.Fatal(called, err)
	}
	if err := runStageEvidence([]string{"--gate-manifest", "relative", "--executable", filepath.Join(t.TempDir(), "amc.exe")}, stage); err == nil || called {
		t.Fatal(called, err)
	}
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	exe := filepath.Join(t.TempDir(), "amc.exe")
	failure := errors.New("fixture staging failure")
	stage = func(m, e string) (string, error) {
		called = true
		if m != manifest || e != exe {
			t.Fatal(m, e)
		}
		return "", failure
	}
	if err := runStageEvidence([]string{"--gate-manifest", manifest, "--executable", exe, "--json"}, stage); !errors.Is(err, failure) || !called {
		t.Fatal(called, err)
	}
}
