package main

import (
	"errors"
	"flag"
	"path/filepath"
)

func runStageEvidence(args []string, stage func(string, string) (string, error)) error {
	flags := flag.NewFlagSet("stage-evidence", flag.ContinueOnError)
	manifest := flags.String("gate-manifest", "", "absolute verified source acceptance manifest")
	executable := flags.String("executable", "", "absolute protected release executable")
	jsonMode := flags.Bool("json", false, "machine readable output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*manifest) || !filepath.IsAbs(*executable) {
		return errors.New("stage-evidence requires absolute --gate-manifest and --executable")
	}
	path, err := stage(*manifest, *executable)
	if err != nil {
		return err
	}
	return output(*jsonMode, map[string]any{"gateManifest": path, "registered": false, "started": false, "authoritative": false})
}
