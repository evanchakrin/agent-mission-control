package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestEconomicsHistoryCursorRejectsRestoredDatabase(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, id := range []string{"first", "second"} {
		if _, err := s.CaptureEconomics(ctx, id, "manual"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.EconomicsHistory(ctx, "", 1)
	if err != nil || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	if _, err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	lost, err := s.CaptureEconomicsAtEpoch(ctx, "after-backup", s.RecoveryEpoch())
	if err != nil || lost == nil {
		t.Fatal(lost, err)
	}
	restored, err := RestoreBackup(ctx, backup, filepath.Join(root, "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.RecoveryEpoch() == s.RecoveryEpoch() {
		t.Fatal("restore did not rotate identity")
	}
	for _, epoch := range []string{s.RecoveryEpoch(), ""} {
		if _, err = restored.CaptureEconomicsAtEpoch(ctx, "after-backup", epoch); !errors.Is(err, ErrHistoryChanged) {
			t.Fatal("lost historical measurement silently recreated", err)
		}
		original, e := s.CaptureEconomicsAtEpoch(ctx, "first", epoch)
		retained, e2 := restored.CaptureEconomicsAtEpoch(ctx, "first", epoch)
		if e != nil || e2 != nil || !reflect.DeepEqual(original, retained) {
			t.Fatal("retained receipt not recovered", e, e2)
		}
	}
	all, err := restored.EconomicsHistory(ctx, "", 100)
	if err != nil || len(all.Items) != 2 {
		t.Fatal("retry changed restored history", all, err)
	}
	if _, err = restored.EconomicsHistory(ctx, page.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("pre-restore cursor silently continued", err)
	}
	fresh, err := restored.EconomicsHistory(ctx, "", 1)
	if err != nil || len(fresh.Items) != 1 || fresh.Items[0].ID != "second" {
		t.Fatal(fresh, err)
	}
	older, err := restored.EconomicsHistory(ctx, fresh.NextCursor, 1)
	if err != nil || len(older.Items) != 1 || older.Items[0].ID != "first" {
		t.Fatal(older, err)
	}
	if freshCapture, e := restored.CaptureEconomicsAtEpoch(ctx, "new-after-restore", restored.RecoveryEpoch()); e != nil || freshCapture == nil {
		t.Fatal("current identity cannot capture", freshCapture, e)
	}
}

func TestEconomicsHistoryCursorSurvivesOrdinaryRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"first", "second"} {
		if _, err = s.CaptureEconomics(ctx, id, "manual"); err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	page, err := s.EconomicsHistory(ctx, "", 1)
	if err != nil || page.NextCursor == "" {
		s.Close()
		t.Fatal(page, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	older, err := s.EconomicsHistory(ctx, page.NextCursor, 1)
	if err != nil || len(older.Items) != 1 || older.Items[0].ID != "first" {
		t.Fatal(older, err)
	}
	if _, err = s.EconomicsHistory(ctx, encodeKey("2", "economics-history"), 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("unbound legacy cursor accepted", err)
	}
	if _, err = s.EconomicsHistory(ctx, encodeKey("-1|"+s.RecoveryEpoch(), "economics-history"), 1); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid boundary accepted", err)
	}
}

func TestEconomicsHistoryReopenPreservesSampleAndTimerSpacing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := s.CaptureEconomics(ctx, "first-timer", "timer")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	retry, err := s.CaptureEconomics(ctx, "first-timer", "timer")
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatal("reopen changed original sample", retry, err)
	}
	if next, e := s.CaptureEconomics(ctx, "after-restart", "timer"); e != nil || next != nil {
		t.Fatal("restart bypassed durable timer gap", next, e)
	}
	page, err := s.EconomicsHistory(ctx, "", 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
}

func TestConcurrentEconomicsTimersAppendOnlyOneSample(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.CaptureEconomics(ctx, fmt.Sprintf("timer-%d", i), "timer")
			if err != nil {
				failures <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	page, err := s.EconomicsHistory(ctx, "", 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("racing timers duplicated the interval", page, err)
	}
}

func TestEconomicsHistoryPreservesEvidenceRetriesAndPagination(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	first, err := s.CaptureEconomics(ctx, "first", "manual")
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	if _, err = s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.tokensIn',99)`); err != nil {
		t.Fatal(err)
	}
	retry, err := s.CaptureEconomics(ctx, "first", "manual")
	if err != nil || !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry changed evidence: %+v %v", retry, err)
	}
	if _, err = s.CaptureEconomics(ctx, "first", "timer"); !errors.Is(err, ErrConflict) {
		t.Fatal("operation reuse changed reason", err)
	}
	if skipped, e := s.CaptureEconomics(ctx, "daily", "timer"); e != nil || skipped != nil {
		t.Fatal("timer gap ignored", skipped, e)
	}
	second, err := s.CaptureEconomics(ctx, "second", "manual")
	if err != nil || second.Measurement.Costs.RecordedTokens == first.Measurement.Costs.RecordedTokens {
		t.Fatal("new snapshot not sampled", second, err)
	}
	page, err := s.EconomicsHistory(ctx, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "second" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	// New appends cannot move an already issued descending cursor forward.
	if _, err = s.CaptureEconomics(ctx, "third", "manual"); err != nil {
		t.Fatal(err)
	}
	older, err := s.EconomicsHistory(ctx, page.NextCursor, 1)
	if err != nil || len(older.Items) != 1 || !reflect.DeepEqual(older.Items[0], *first) || older.NextCursor != "" {
		t.Fatal(older, err)
	}
	if _, err = s.EconomicsHistory(ctx, "invalid", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestEconomicsTimerCaptureIsDurablyRateLimited(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	first, err := s.CaptureEconomics(ctx, "timer-1", "timer")
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	second, err := s.CaptureEconomics(ctx, "timer-2", "timer")
	if err != nil || second != nil {
		t.Fatal(second, err)
	}
	if _, err = s.db.Exec("UPDATE economics_history SET measured_at=measured_at-21*60*60*1000"); err != nil {
		t.Fatal(err)
	}
	second, err = s.CaptureEconomics(ctx, "timer-2", "timer")
	if err != nil || second == nil {
		t.Fatal(second, err)
	}
	if second.Measurement.Costs.KnownCost != nil {
		t.Fatal("empty history is not a free-price claim")
	}
}
