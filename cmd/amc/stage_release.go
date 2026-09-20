package main

import (
	"errors"
	"flag"
	"path/filepath"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

// Preparing immutable files is not release acceptance or service registration.
// No configuration or credential is required to package an executable.
func runStageRelease(args []string, stage func(string, string, string) (platform.Release, error)) error {
	flags := flag.NewFlagSet("stage-release", flag.ContinueOnError)
	executable := flags.String("executable", "", "absolute executable to package")
	root := flags.String("release-root", "", "protected Program Files release root")
	version := flags.String("release-version", "", "new immutable version directory")
	jsonMode := flags.Bool("json", false, "machine readable output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*executable) || !filepath.IsAbs(*root) || *version == "" {
		return errors.New("stage-release requires absolute --executable and --release-root, plus a new --release-version")
	}
	release, err := stage(*executable, *root, *version)
	if err != nil {
		return err
	}
	return output(*jsonMode, map[string]any{"release": release, "registered": false, "started": false, "authoritative": false})
}
