package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type duplicateCrashFixture struct {
	Marker      string
	Left, Right string
}

// This subprocess entrypoint refuses any directory without the explicit tiny
// fixture marker. os.Exit deliberately bypasses SQLite Close and test cleanup.
func TestDuplicateReconciliationCrashHelper(t *testing.T) {
	dir := os.Getenv("AMC_RECONCILIATION_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess fixture only")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("absolute fixture directory required")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "disposable-crash-fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture duplicateCrashFixture
	if json.Unmarshal(raw, &fixture) != nil || fixture.Marker != "tiny-reconciliation-fixture-v1" || fixture.Left == "" || fixture.Right == "" {
		t.Fatal("fixture marker missing")
	}
	mode := os.Getenv("AMC_RECONCILIATION_CRASH_MODE")
	if mode != "before-selection-commit" && mode != "after-selection-commit" {
		t.Fatal("invalid crash mode")
	}
	commits := 0
	s, err := store.Open(dir, store.Options{BeforeCommit: func() error {
		commits++
		if mode == "before-selection-commit" && commits == 2 {
			os.Exit(87)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := ReconcileDuplicateUsagePage(ctx, s, fixture.Left, fixture.Right, "", "", 1)
	if err != nil || page.Matched != 1 {
		t.Fatal(page, err)
	}
	if mode == "after-selection-commit" {
		os.Exit(88)
	}
	t.Fatal("intended pre-commit boundary was not reached")
}

func checkDuplicateReconciliationCrashes(t *testing.T, backupDir, dest, left, right string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.RestoreBackup(ctx, backupDir, dest, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := s.ContributionTotals(ctx, store.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(duplicateCrashFixture{Marker: "tiny-reconciliation-fixture-v1", Left: left, Right: right})
	if err = os.WriteFile(filepath.Join(dest, "disposable-crash-fixture.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(mode string, want int) {
		t.Helper()
		childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		command := exec.CommandContext(childCtx, executable, "-test.run=^TestDuplicateReconciliationCrashHelper$", "-test.count=1")
		command.Env = append(os.Environ(), "AMC_RECONCILIATION_CRASH_DIR="+dest, "AMC_RECONCILIATION_CRASH_MODE="+mode)
		output, err := command.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != want {
			t.Fatalf("crash boundary %s: %v: %s", mode, err, output)
		}
	}
	read := func() (store.UsageContributionTotals, int64) {
		t.Helper()
		opened, err := store.Open(dest, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		value, err := opened.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil {
			t.Fatal(err)
		}
		head, err := opened.ChangeHead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return value, head
	}
	run("before-selection-commit", 87)
	afterFailure, _ := read()
	if !reflect.DeepEqual(afterFailure, baseline) {
		t.Fatal("uncommitted exclusion survived process exit", afterFailure, baseline)
	}
	run("after-selection-commit", 88)
	afterCommit, head := read()
	if afterCommit.ActiveExclusions != 1 || afterCommit.Recorded != baseline.Recorded || afterCommit.Excluded.Total <= 0 || afterCommit.Counted.Total != afterCommit.Recorded.Total-afterCommit.Excluded.Total {
		t.Fatal("committed exclusion lost or duplicated", afterCommit)
	}
	run("after-selection-commit", 88)
	afterRetry, retryHead := read()
	if !reflect.DeepEqual(afterCommit, afterRetry) || head != retryHead {
		t.Fatal("lost acknowledgement retry duplicated exclusion", afterRetry, afterCommit, head, retryHead)
	}
}
