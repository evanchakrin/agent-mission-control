package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

func TestStageReleaseRequiresExplicitPathsBeforeWrites(t *testing.T) {
	called := 0
	stage := func(string, string, string) (platform.Release, error) { called++; return platform.Release{}, nil }
	for _, args := range [][]string{nil, {"--release-version", "v1"}, {"--executable", "relative.exe", "--release-root", t.TempDir(), "--release-version", "v1"}} {
		if err := runStageRelease(args, stage); err == nil {
			t.Fatal(args)
		}
	}
	if called != 0 {
		t.Fatal("invalid staging invoked writer")
	}
}

func TestStageReleaseForwardsOnlyExplicitPackagingInputs(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "candidate.exe")
	root := t.TempDir()
	called := 0
	sentinel := errors.New("fixture staging failure")
	stage := func(e, r, v string) (platform.Release, error) {
		called++
		if e != exe || r != root || v != "v1" {
			t.Fatal(e, r, v)
		}
		return platform.Release{}, sentinel
	}
	if err := runStageRelease([]string{"--executable", exe, "--release-root", root, "--release-version", "v1", "--json"}, stage); !errors.Is(err, sentinel) || called != 1 {
		t.Fatal(called, err)
	}
}
