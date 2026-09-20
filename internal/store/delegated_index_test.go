package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestDelegatedIndexFailurePreservesHistoryAndCanRetry(t *testing.T) {
	for _, stage := range []string{"before-build", "before-commit", "cancel-before-commit", "space-read-error"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			ingest(t, s, src, 0, "source bytes\n")
			b := batch(src, 0, 13)
			b.Events = []Event{{ID: "task", Kind: "tool-call", Text: "needle", Data: json.RawMessage(`{"tool":"Task"}`), SourceLength: 13}}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			yes := true
			name := "Preserved name"
			if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: "archive", Archived: &yes, Name: &name}); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetSession(ctx, b.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSource, err := s.SourceState(ctx, src.SourceID, src.Generation)
			if err != nil {
				t.Fatal(err)
			}
			// Disposable ledger only: simulate the pre-upgrade schema.
			if _, err = s.db.Exec("DROP INDEX IF EXISTS events_delegated_page"); err != nil {
				t.Fatal(err)
			}
			buildCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			checks := 0
			probeError := errors.New("storage probe unavailable")
			s.options.ReserveBytes = 1
			s.options.AvailableBytes = func(string) (int64, error) {
				checks++
				if stage == "space-read-error" {
					return 0, probeError
				}
				if (stage == "before-build" && checks == 1) || (stage == "before-commit" && checks == 2) {
					return 1, nil
				}
				if stage == "cancel-before-commit" && checks == 2 {
					cancel()
				}
				return 1 << 30, nil
			}
			err = s.ensureDelegatedIndex(buildCtx)
			want := ErrCapacity
			if stage == "cancel-before-commit" {
				want = context.Canceled
			}
			if stage == "space-read-error" {
				want = probeError
			}
			if !errors.Is(err, want) {
				t.Fatalf("unexpected failure: %v want %v", err, want)
			}
			var count int
			if err = s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='events_delegated_page'").Scan(&count); err != nil || count != 0 {
				t.Fatal("failed build published index", count, err)
			}
			s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
			after, err := s.GetSession(ctx, b.Session.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("session or organization changed", err)
			}
			afterSource, err := s.SourceState(ctx, src.SourceID, src.Generation)
			if err != nil || !reflect.DeepEqual(beforeSource, afterSource) {
				t.Fatal("capture or parser checkpoint changed", err)
			}
			r, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(raw, []byte("source bytes\n")) {
				t.Fatal("raw evidence changed", err)
			}
			if err = s.ensureDelegatedIndex(ctx); err != nil {
				t.Fatal("retry failed", err)
			}
			page, err := s.SearchPinned(ctx, SearchQuery{Text: "needle", DelegatedOnly: true}, "")
			if err != nil || len(page.Events) != 1 || page.Events[0].ID != "task" {
				t.Fatal("retry lost searchable history", page, err)
			}
			// An existing index remains readable even when no build space remains.
			s.options.AvailableBytes = func(string) (int64, error) { return 1, nil }
			if err = s.ensureDelegatedIndex(ctx); err != nil {
				t.Fatal("existing index requires build space", err)
			}
		})
	}
}
