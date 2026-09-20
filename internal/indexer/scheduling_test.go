package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestGroupedDispatchSeparatesSourceGenerations(t *testing.T) {
	var calls [][]string
	x := Indexer{ProcessGroup: func(_ context.Context, work []SourceWork) ([]SourceResult, error) {
		var ids []string
		result := make([]SourceResult, len(work))
		seen := map[string]bool{}
		for i, w := range work {
			if seen[w.SourceID] {
				t.Fatal("duplicate source dispatched together", work)
			}
			seen[w.SourceID] = true
			ids = append(ids, w.SourceID+"/"+w.Generation)
			result[i].Records = 1
		}
		calls = append(calls, ids)
		return result, nil
	}}
	sources := []store.SourceState{
		{Source: protocol.Source{SourceID: "rewrite", Generation: "a"}},
		{Source: protocol.Source{SourceID: "rewrite", Generation: "b"}},
		{Source: protocol.Source{SourceID: "other", Generation: "a"}},
	}
	progress, err := x.processSources(context.Background(), sources)
	if err != nil || !progress {
		t.Fatal(progress, err)
	}
	want := [][]string{{"rewrite/a"}, {"rewrite/b", "other/a"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
}

func TestGroupedIngestionYieldsToRebuildBetweenBoundedPages(t *testing.T) {
	s, source, raw := rebuildFixture(t, 1, store.Options{})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.BeginRebuild(ctx, "preserved-session-key", "group-fair-rebuild", parser.Version); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	for i := 0; i < 17; i++ {
		source.SourceID = fmt.Sprintf("pending-%02d", i)
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: source, Length: source.Size, SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	var visited []string
	x := Indexer{Store: s,
		RebuildProcess: func(context.Context, string) (int64, error) { calls = append(calls, "rebuild"); return 1, nil },
		ProcessGroup: func(_ context.Context, work []SourceWork) ([]SourceResult, error) {
			calls = append(calls, fmt.Sprintf("group-%d", len(work)))
			result := make([]SourceResult, len(work))
			for i, src := range work {
				visited = append(visited, src.SourceID)
				result[i].Records = 1
			}
			if len(visited) >= 17 {
				cancel()
			}
			return result, nil
		},
	}
	if err := x.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if want := []string{"rebuild", "group-8", "rebuild", "group-8", "rebuild", "group-1"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("group dispatcher starved rebuilds or lost its cursor: %v", calls)
	}
	for i, id := range visited {
		if id != fmt.Sprintf("pending-%02d", i) {
			t.Fatalf("source skipped or repeated: %v", visited)
		}
	}
}

func TestRebuildAlternatesWithIngestionWithoutCatalogStarvation(t *testing.T) {
	s, source, raw := rebuildFixture(t, 1, store.Options{})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.BeginRebuild(ctx, "preserved-session-key", "fair-rebuild", parser.Version); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	for _, id := range []string{"pending-a", "pending-b", "pending-c"} {
		source.SourceID = id
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: source, Length: source.Size, SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	var normal int
	x := Indexer{Store: s,
		RebuildProcess: func(context.Context, string) (int64, error) { calls = append(calls, "rebuild"); return 1, nil },
		Process: func(_ context.Context, id, generation string) (int64, error) {
			calls = append(calls, id)
			normal++
			if normal == 4 {
				cancel()
			}
			return 1, nil
		},
	}
	if err := x.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("run termination: %v", err)
	}
	want := []string{"rebuild", "pending-a", "rebuild", "pending-b", "rebuild", "pending-c", "rebuild", "rebuild", "pending-a"}
	// Wrapping the source cursor may dispatch an extra rebuild, but must not
	// repeat the first source before servicing the other sources.
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unfair dispatch sequence: %v; want %v", calls, want)
	}
}
