package collector

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestMissingSourceStateRequiresDurableGap(t *testing.T) {
	for _, mode := range []string{"capture", "discovery"} {
		t.Run(mode, func(t *testing.T) {
			c, root := testCollector(t, nil)
			path := writeSource(t, root, "gone.jsonl", []byte("uncaptured history\n"))
			reconcile(t, c)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := c.db.Exec(`CREATE TRIGGER fail_gap BEFORE INSERT ON gaps BEGIN SELECT RAISE(ABORT,'injected gap failure'); END`); err != nil {
				t.Fatal(err)
			}
			run := func() error {
				if mode == "capture" {
					return c.CaptureOnce(context.Background())
				}
				return c.Reconcile(context.Background())
			}
			if err := run(); err == nil || !strings.Contains(err.Error(), "injected gap failure") {
				t.Fatalf("missing durable gap failure: %v", err)
			}
			var missing int
			if err := c.db.QueryRow("SELECT missing FROM sources").Scan(&missing); err != nil {
				t.Fatal(err)
			}
			if missing != 0 || stats(t, c).Gaps != 0 {
				t.Fatal("source retired without durable gap")
			}
			if _, err := c.db.Exec("DROP TRIGGER fail_gap"); err != nil {
				t.Fatal(err)
			}
			_ = run()
			if err := c.db.QueryRow("SELECT missing FROM sources").Scan(&missing); err != nil {
				t.Fatal(err)
			}
			if missing != 1 || stats(t, c).Gaps != 1 {
				t.Fatal("retry did not preserve gap")
			}
		})
	}
}

func TestMissingSourceUpdateFailureRollsBackGap(t *testing.T) {
	c, root := testCollector(t, nil)
	path := writeSource(t, root, "gone.jsonl", []byte("uncaptured history\n"))
	reconcile(t, c)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER fail_missing BEFORE UPDATE OF missing ON sources WHEN NEW.missing=1 BEGIN SELECT RAISE(ABORT,'injected missing failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.CaptureOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "injected missing failure") {
		t.Fatalf("missing state failure hidden: %v", err)
	}
	if stats(t, c).Gaps != 0 {
		t.Fatal("gap survived rolled back source update")
	}
	if _, err := c.db.Exec("DROP TRIGGER fail_missing"); err != nil {
		t.Fatal(err)
	}
	_ = c.CaptureOnce(context.Background())
	if stats(t, c).Gaps != 1 {
		t.Fatal("retry did not record gap")
	}
}

func TestRecoveryGapPersistenceFailureIsReported(t *testing.T) {
	for _, mode := range []string{"missing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			c, root := testCollector(t, nil)
			path := writeSource(t, root, "history.jsonl", []byte("history\n"))
			reconcile(t, c)
			capture(t, c, 1)
			r, err := c.leaseChunk(context.Background())
			if err != nil || r == nil {
				t.Fatalf("lease: %v", err)
			}
			if mode == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeSource(t, root, "history.jsonl", []byte("changed\n"))
			}
			before := stats(t, c)
			if _, err := c.db.Exec(`CREATE TRIGGER fail_gap BEFORE INSERT ON gaps BEGIN SELECT RAISE(ABORT,'injected gap failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := c.restorePayload(context.Background(), r); err == nil || !strings.Contains(err.Error(), "injected gap failure") {
				t.Fatalf("gap persistence failure hidden: %v", err)
			}
			after := stats(t, c)
			if after.Gaps != 0 || after.BacklogBytes != before.BacklogBytes || after.UploadedBytes != before.UploadedBytes || after.SpoolBytes != before.SpoolBytes {
				t.Fatal("failed gap write changed durable progress")
			}
			if _, err := c.db.Exec("DROP TRIGGER fail_gap"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := c.restorePayload(context.Background(), r); err == nil {
					t.Fatal("lost evidence recovered unexpectedly")
				}
				if stats(t, c).Gaps != 1 {
					t.Fatal("gap missing or duplicated")
				}
			}
		})
	}
}
