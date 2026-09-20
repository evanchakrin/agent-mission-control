package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEconomicsResolutionPreservesUnavailableAttemptWithoutInventingMeasurement(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := s.ResolveEconomicsCapture(ctx, "missing", "old", "stale"); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	if _, err := s.ResolveEconomicsCapture(ctx, "missing", s.RecoveryEpoch(), s.RecoveryEpoch()); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	r, err := s.ResolveEconomicsCapture(ctx, "missing", "old", s.RecoveryEpoch())
	if err != nil || r.Outcome != "capture-unavailable" || r.ResolvedAt == "" {
		t.Fatal(r, err)
	}
	retry, err := s.ResolveEconomicsCapture(ctx, "missing", "old", "stale")
	if err != nil || !reflect.DeepEqual(r, retry) {
		t.Fatal(retry, err)
	}
	if _, err = s.ResolveEconomicsCapture(ctx, "missing", "different", s.RecoveryEpoch()); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.CaptureEconomicsAtEpoch(ctx, "missing", s.RecoveryEpoch()); !errors.Is(err, ErrConflict) {
		t.Fatal("resolved identity reused as new measurement", err)
	}
	page, err := s.EconomicsHistory(ctx, "", 100)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("resolution invented a measurement", page, err)
	}
	if _, err = s.CaptureEconomicsAtEpoch(ctx, "new-measurement", s.RecoveryEpoch()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveEconomicsCapture(ctx, "new-measurement", "old", s.RecoveryEpoch()); !errors.Is(err, ErrConflict) {
		t.Fatal("surviving receipt marked unavailable", err)
	}
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	if _, err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(ctx, backup, filepath.Join(root, "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	saved, err := restored.EconomicsCaptureResolution(ctx, "missing")
	if err != nil || !reflect.DeepEqual(r, saved) {
		t.Fatal("backup lost audit record", saved, err)
	}
	if _, err = restored.CaptureEconomicsAtEpoch(ctx, "missing", restored.RecoveryEpoch()); !errors.Is(err, ErrConflict) {
		t.Fatal("restore removed reuse guard", err)
	}
}

func TestEconomicsResolutionPaginationTiesAppendAndRestore(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.ResolveEconomicsCapture(ctx, id, "old", s.RecoveryEpoch()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE economics_capture_resolutions SET resolved_at='2020-01-01T00:00:00.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	page, err := s.EconomicsCaptureResolutions(ctx, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "c" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	if _, err = s.ResolveEconomicsCapture(ctx, "later", "old", s.RecoveryEpoch()); err != nil {
		t.Fatal(err)
	}
	next, err := s.EconomicsCaptureResolutions(ctx, page.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != "b" || next.NextCursor == "" {
		t.Fatal(next, err)
	}
	last, err := s.EconomicsCaptureResolutions(ctx, next.NextCursor, 1)
	if err != nil || len(last.Items) != 1 || last.Items[0].ID != "a" || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	if _, err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(ctx, backup, filepath.Join(root, "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err = restored.EconomicsCaptureResolutions(ctx, page.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("old restore cursor accepted", err)
	}
	fresh, err := restored.EconomicsCaptureResolutions(ctx, "", 100)
	if err != nil || len(fresh.Items) != 4 {
		t.Fatal(fresh, err)
	}
}
