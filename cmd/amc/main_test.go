package main

import (
	"strings"
	"testing"
)

func TestCandidateCommandsRequireExplicitConfiguration(t *testing.T) {
	for _, command := range []string{"hub", "collector", "desktop", "doctor", "status", "install", "stage-config", "backup", "restore", "reindex-outdated", "migration-preflight", "migrate"} {
		err := run([]string{command, "--json"})
		if err == nil || !strings.Contains(err.Error(), "--config is required") {
			t.Fatalf("%s discovered implicit owner state: %v", command, err)
		}
	}
}
