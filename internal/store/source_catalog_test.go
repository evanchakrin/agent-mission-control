package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSourceCatalogFilterCoversCatalogAndBindsCursor(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		src := testSource()
		src.SourceID = id
		src.Path = "ordinary.jsonl"
		if id != "a" {
			src.Path = "history/Target%_" + id + ".jsonl"
		}
		ingest(t, s, src, 0, "raw\n")
	}
	p, err := s.SearchSourceCatalog(ctx, "machine-1", "", 1, "target%_")
	if err != nil || len(p.Items) != 1 || p.Items[0].Source.SourceID != "b" || p.NextCursor == "" || p.Query != "target%_" {
		t.Fatal(p, err)
	}
	n, err := s.SearchSourceCatalog(ctx, "machine-1", p.NextCursor, 1, "target%_")
	if err != nil || len(n.Items) != 1 || n.Items[0].Source.SourceID != "c" || n.NextCursor != "" {
		t.Fatal(n, err)
	}
	if _, err = s.SearchSourceCatalog(ctx, "machine-1", p.NextCursor, 1, "ordinary"); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("filter cursor reused", err)
	}
	for _, q := range []string{strings.Repeat("x", 1025), "bad\x00path"} {
		if _, err = s.SearchSourceCatalog(ctx, "machine-1", "", 1, q); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestSourceCatalogSearchUsesIndexedTitleAndOwnerName(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "raw\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 4)); err != nil {
		t.Fatal(err)
	}
	page, err := s.SearchSourceCatalog(ctx, src.MachineID, "", 1, "HISTORICAL")
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "Historical session" {
		t.Fatal(page, err)
	}
	if _, err = s.db.Exec(`INSERT INTO session_metadata VALUES('session-1',1,'{"name":"Owner <title>","archived":true}')`); err != nil {
		t.Fatal(err)
	}
	page, err = s.SearchSourceCatalog(ctx, src.MachineID, "", 1, "owner <title>")
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "Owner <title>" {
		t.Fatal(page, err)
	}
	page, err = s.SearchSourceCatalog(ctx, src.MachineID, "", 1, "historical")
	if err != nil || len(page.Items) != 0 {
		t.Fatal("search disagrees with displayed title", page, err)
	}
	if _, err = s.db.Exec(`UPDATE session_metadata SET value='{"name":"","archived":true}'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.SearchSourceCatalog(ctx, src.MachineID, "", 1, "historical")
	if err != nil || len(page.Items) != 1 {
		t.Fatal("cleared label or archive hid source", page, err)
	}
}

func TestSourceCatalogPreservesUnindexedAndOlderGenerations(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "unsupported raw evidence\n")
	old := src
	src.Generation = "generation-2"
	src.GenerationSequence = 2
	ingest(t, s, src, 0, "replacement\n")
	other := testSource()
	other.MachineID = "other-machine"
	other.SourceID = "other-source"
	ingest(t, s, other, 0, "private other machine\n")
	page, err := s.SourceCatalog(ctx, src.MachineID, "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	first := page.Items[0]
	if first.Source.Generation != old.Generation || first.ActiveGeneration || first.SessionID != "" || first.DurableOffset != int64(len("unsupported raw evidence\n")) || first.IndexedOffset != 0 {
		t.Fatal(first)
	}
	next, err := s.SourceCatalog(ctx, src.MachineID, page.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.NextCursor != "" || !next.Items[0].ActiveGeneration || next.Items[0].Source.Generation != src.Generation {
		t.Fatal(next, err)
	}
	if _, err = s.SourceCatalog(ctx, other.MachineID, page.NextCursor, 1); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cursor crossed machines", err)
	}
	for _, item := range append(page.Items, next.Items...) {
		r, err := s.OpenSource(ctx, item.Source.SourceID, item.Source.Generation, 0)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		r.Close()
		if err != nil || int64(len(raw)) != item.DurableOffset {
			t.Fatal("raw evidence lost", err)
		}
	}
	if _, err = s.SourceCatalog(ctx, src.MachineID, "", 101); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = s.SourceCatalog(ctx, src.MachineID, "bad", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err = s.CommitIndex(ctx, batch(src, 0, int64(len("replacement\n")))); err != nil {
		t.Fatal(err)
	}
	indexed, err := s.SourceCatalog(ctx, src.MachineID, page.NextCursor, 1)
	if err != nil || len(indexed.Items) != 1 || indexed.Items[0].SessionID != "session-1" || indexed.Items[0].IndexedOffset != int64(len("replacement\n")) {
		t.Fatal("indexed session linkage missing", indexed, err)
	}
	originalEpoch := s.epoch
	s.epoch = "restored-test-epoch"
	_, err = s.SourceCatalog(ctx, src.MachineID, page.NextCursor, 1)
	s.epoch = originalEpoch
	if !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("cursor survived hub restore", err)
	}
}
