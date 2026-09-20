package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWALReclaimSurvivesProcessKill(t *testing.T) {
	src := testSource()
	src.ModifiedAt = time.Unix(0, 0).UTC()
	ctx := context.Background()
	if dir := os.Getenv("AMC_RECLAIM_CRASH_FIXTURE"); dir != "" {
		if !filepath.IsAbs(dir) {
			t.Fatal("crash fixture requires an absolute fresh directory")
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("crash fixture refuses an existing or inaccessible directory")
		}
		s, err := Open(dir, Options{ExternalCheckpointOwner: true})
		if err != nil {
			t.Fatal(err)
		}
		// Intentionally no Close: the parent kills this process after readiness.
		receipt := ingest(t, s, src, 0, "fixture\n")
		if err := s.CommitIndex(ctx, batch(src, 0, 8)); err != nil {
			t.Fatal(err)
		}
		yes := true
		if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "reclaim-archive", Archived: &yes}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TABLE reclaim_padding(data BLOB); INSERT INTO reclaim_padding VALUES(zeroblob(68157440))`); err != nil {
			t.Fatal(err)
		}
		s.checkpointOnce(ctx)
		if status := s.CheckpointStatus(); status.ReclaimPolicy != "size-triggered-restart" || status.LastReclaim != "restart-ready" {
			t.Fatal("crash fixture did not exercise size-triggered restart", status)
		}
		// The ordinary committing writer, not a forced checkpoint, resets the WAL.
		if _, err := s.db.Exec(`INSERT INTO reclaim_padding VALUES(x'01')`); err != nil {
			t.Fatal("writer reset failed", err)
		}
		info, err := os.Stat(filepath.Join(dir, "ledger.sqlite-wal"))
		if err != nil || info.Size() == 0 || info.Size() > 64<<20 {
			t.Fatal("WAL reset did not reclaim retained allocation", info, err)
		}
		fmt.Println("reclaimed-ready", receipt.ReceiptID)
		select {}
	}
	dir := filepath.Join(t.TempDir(), "crash-store")
	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, os.Args[0], "-test.run=^TestWALReclaimSurvivesProcessKill$", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), "AMC_RECLAIM_CRASH_FIXTURE="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if id, ok := strings.CutPrefix(scanner.Text(), "reclaimed-ready "); ok {
				ready <- id
				return
			}
		}
		ready <- ""
	}()
	var receiptID string
	select {
	case receiptID = <-ready:
	case <-run.Done():
		t.Fatal("child did not reclaim before deadline")
	}
	if receiptID == "" {
		t.Fatal("child exited before reclamation readiness")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited cleanly; crash was not exercised")
	}
	s, err := Open(dir, Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	var resetRows, resetBytes int
	if err := s.db.QueryRow(`SELECT count(*),sum(length(data)) FROM reclaim_padding`).Scan(&resetRows, &resetBytes); err != nil || resetRows != 2 || resetBytes != (65<<20)+1 {
		t.Fatal("reset transaction lost after crash", resetRows, resetBytes, err)
	}
	raw, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(raw)
	_ = raw.Close()
	if err != nil || string(body) != "fixture\n" {
		t.Fatal("acknowledged raw bytes lost", string(body), err)
	}
	retry := ingest(t, s, src, 0, "fixture\n")
	if retry.ReceiptID != receiptID || retry.DurableOffset != 8 {
		t.Fatal("receipt changed after reclaimed-WAL crash", retry)
	}
	state, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || state.DurableOffset != 8 || state.IndexedOffset != 8 {
		t.Fatal("durable parser checkpoint lost", state, err)
	}
	metadata, err := s.GetMetadata(ctx, "session-1")
	if err != nil || !metadata.Archived || metadata.Revision != 1 {
		t.Fatal("archive state lost", metadata, err)
	}
	var audits int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM organization_audit WHERE operation_id='reclaim-archive'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("archive audit lost or duplicated", audits, err)
	}
}

func TestWALReclaimCrashChildRejectsUnsafeDirectory(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "relative"}[relative], func(t *testing.T) {
			dir := t.TempDir()
			target := dir
			if relative {
				target = "relative-store"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWALReclaimSurvivesProcessKill$", "-test.timeout=10s")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "AMC_RECLAIM_CRASH_FIXTURE="+target)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "crash fixture") {
				t.Fatal("unsafe child target was not rejected", err, string(output))
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("rejected child wrote fixture state", entries, err)
			}
		})
	}
}
