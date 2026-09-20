package runtimeinfo

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanRestartAndMonotonicDurableProgress(t *testing.T) {
	dir := t.TempDir()
	j, err := Begin(dir, "hub", "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Finish("shutdown")
	first := j.Snapshot()
	if first.Starts != 1 || first.Restarts != 0 || !first.CountsKnown || first.PreviousDisposition != "none" || !first.FinishedAt.IsZero() {
		t.Fatal(first)
	}
	if _, err := Begin(dir, "hub", "test-v1"); !errors.Is(err, ErrLocked) {
		t.Fatal("concurrent writer admitted", err)
	}
	other, err := Begin(dir, "satellite", "test-v1")
	if err != nil {
		t.Fatal("different role blocked", err)
	}
	if err := other.Finish("completed"); err != nil {
		t.Fatal(err)
	}
	progress := time.Now().UTC()
	if err := j.NoteProgress(progress); err != nil {
		t.Fatal(err)
	}
	if got := j.Snapshot().LastDurableProgress; !got.Equal(progress) {
		t.Fatal("first progress not durable", got)
	}
	if err := j.NoteProgress(progress.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	pending := progress.Add(time.Second)
	if err := j.NoteProgress(pending); err != nil {
		t.Fatal(err)
	}
	if got := j.Snapshot().LastDurableProgress; !got.Equal(progress) {
		t.Fatal("reported unflushed progress", got)
	}
	if err := j.Finish("secret token must not be persisted"); !errors.Is(err, ErrInvalid) {
		t.Fatal("arbitrary log persisted", err)
	}
	if err := j.Finish("shutdown"); err != nil {
		t.Fatal(err)
	}
	if !j.Snapshot().LastDurableProgress.Equal(pending) {
		t.Fatal("finish dropped pending durable progress")
	}
	if err := j.NoteProgress(time.Now()); !errors.Is(err, ErrClosed) {
		t.Fatal("finished journal writable", err)
	}
	next, err := Begin(dir, "hub", "test-v2")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Finish("shutdown")
	s := next.Snapshot()
	if s.Starts != 2 || s.Restarts != 1 || s.PreviousDisposition != "clean" || s.PreviousFinishReason != "shutdown" || s.PreviousRunID != first.RunID || s.RunID == first.RunID || s.Version != "test-v2" {
		t.Fatal(s)
	}
	if !s.LastDurableProgress.Equal(pending) {
		t.Fatal("restart lost last durable work timestamp", s)
	}
	b, err := os.ReadFile(filepath.Join(dir, "runtime", "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxJournalBytes || bytes.Contains(b, []byte("secret token")) {
		t.Fatal("journal not bounded or leaked arbitrary text")
	}
}

func TestInterruptedRunProcess(t *testing.T) {
	if os.Getenv("AMC_RUNTIME_JOURNAL_HELPER") == "1" {
		j, err := Begin(os.Getenv("AMC_RUNTIME_JOURNAL_DIR"), "hub", "test-child")
		if err != nil {
			os.Exit(3)
		}
		if err = j.NoteProgress(time.Now().UTC()); err != nil {
			os.Exit(4)
		}
		// No Finish: a real OS process exit releases the advisory file lock but
		// leaves an unfinished durable run, as a crash or kill would.
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptedRunProcess$")
	cmd.Env = append(os.Environ(), "AMC_RUNTIME_JOURNAL_HELPER=1", "AMC_RUNTIME_JOURNAL_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v %s", err, output)
	}
	j, err := Begin(dir, "hub", "test-parent")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Finish("shutdown")
	s := j.Snapshot()
	if s.Starts != 2 || s.PreviousDisposition != "unexpected" || !s.PreviousFinishedAt.IsZero() || s.PreviousFinishReason != "" {
		t.Fatal("invented crash cause or lost unfinished run", s)
	}
	if s.LastDurableProgress.IsZero() {
		t.Fatal("unexpected restart lost durable progress")
	}
}

func TestCorruptJournalCountsAreUnknownAndOriginalPreserved(t *testing.T) {
	dir := t.TempDir()
	j, err := Begin(dir, "hub", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Finish("clean"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runtime", "hub.json")
	bad := []byte(`{"schema":1,"starts":999999`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := Begin(dir, "hub", "v2")
	if err != nil {
		t.Fatal(err)
	}
	s := next.Snapshot()
	if s.CountsKnown || s.PreviousDisposition != "unknown-corrupt" || s.Warning == "" || s.Starts != 1 {
		t.Fatal("corruption invented lifetime count", s)
	}
	actual, err := os.ReadFile(path + ".corrupt")
	if err != nil || !bytes.Equal(actual, bad) {
		t.Fatal("original corruption erased", err)
	}
	if err := next.Finish("clean"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(dir, "hub", "v3"); !errors.Is(err, ErrCorrupt) {
		t.Fatal("overwrote prior corruption or created unbounded archives", err)
	}
}

func TestBoundedReadsAndFutureSchemaFailWithoutOverwrite(t *testing.T) {
	for _, fixture := range []struct {
		name, body  string
		wantCorrupt bool
	}{
		{"oversize", strings.Repeat("x", maxJournalBytes+1), true},
		{"future", `{"schema":2}`, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "runtime", "hub.json")
			if err := os.WriteFile(path, []byte(fixture.body), 0600); err != nil {
				t.Fatal(err)
			}
			j, err := Begin(dir, "hub", "v1")
			if fixture.wantCorrupt {
				if err != nil {
					t.Fatal(err)
				}
				defer j.Finish("clean")
				if j.Snapshot().CountsKnown {
					t.Fatal("oversize history trusted")
				}
			} else {
				if err == nil {
					j.Finish("clean")
					t.Fatal("future schema overwritten")
				}
				actual, _ := os.ReadFile(path)
				if string(actual) != fixture.body {
					t.Fatal("future journal changed")
				}
			}
		})
	}
	for _, role := range []string{"../hub", "", "hub/name", strings.Repeat("a", 33)} {
		if _, err := Begin(t.TempDir(), role, "v1"); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid role", role, err)
		}
	}
}

func TestFailedProgressRetryAndInterruptedCorruptionRecovery(t *testing.T) {
	dir := t.TempDir()
	j, err := Begin(dir, "hub", "v1")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := os.Mkdir(j.path+".pending", 0700); err != nil {
		t.Fatal(err)
	}
	if err := j.NoteProgress(at); err == nil {
		t.Fatal("failed write claimed success")
	}
	if !j.Snapshot().LastDurableProgress.IsZero() {
		t.Fatal("failed progress reported durable")
	}
	if err := os.Remove(j.path + ".pending"); err != nil {
		t.Fatal(err)
	}
	if err := j.NoteProgress(at); err != nil {
		t.Fatal("same timestamp retry failed", err)
	}
	if !j.Snapshot().LastDurableProgress.Equal(at) {
		t.Fatal("same timestamp never retried")
	}
	if err := j.Finish("clean"); err != nil {
		t.Fatal(err)
	}
	// Simulate the narrow crash boundary after quarantining corruption and
	// before the replacement journal is durable.
	if err := os.Rename(j.path, j.path+".corrupt"); err != nil {
		t.Fatal(err)
	}
	next, err := Begin(dir, "hub", "v2")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Finish("clean")
	if next.Snapshot().CountsKnown || next.Snapshot().PreviousDisposition != "unknown-corrupt" {
		t.Fatal("lost uncertainty after recovery crash")
	}
}
