package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func groupSources(t *testing.T, raw []byte, count int) (*Indexer, []SourceWork, []protocol.Source) {
	return groupSourcesAt(t, t.TempDir(), raw, count)
}

func groupSourcesAt(t *testing.T, dir string, raw []byte, count int) (*Indexer, []SourceWork, []protocol.Source) {
	t.Helper()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.SetupIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := make([]SourceWork, count)
	sources := make([]protocol.Source, count)
	for i := range work {
		src := protocol.Source{MachineID: "group-fixture", SourceID: fmt.Sprintf("source-%d", i), Generation: "g", Provider: "claude", NativeID: fmt.Sprintf("native-%d", i), Size: int64(len(raw))}
		h := sha256.Sum256(raw)
		if _, err := s.IngestChunk(context.Background(), protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		work[i] = SourceWork{SourceID: src.SourceID, Generation: src.Generation}
		sources[i] = src
	}
	return &Indexer{Store: s}, work, sources
}

var groupRecord = []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"content\":\"hi\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n")

func TestParserGroupFlushesPreparedBudget(t *testing.T) {
	raw := []byte(strings.Replace(string(groupRecord), "hi", strings.Repeat("a", 200<<10), 1))
	x, work, _ := groupSources(t, raw, 8)
	calls := 0
	result, err := x.groupOnce(context.Background(), work, func(ctx context.Context, batches []store.IndexBatch) error {
		calls++
		var cost int64
		for _, batch := range batches {
			cost += store.IndexBatchBudget(batch)
		}
		if cost > 4<<20 {
			t.Fatalf("prepared group exceeds bound: %d", cost)
		}
		return x.Store.CommitIndexGroup(ctx, batches)
	})
	if err != nil || calls < 2 {
		t.Fatal("fixture did not exercise bounded flushing", calls, err)
	}
	for _, r := range result {
		if r.Records != 1 || r.Error != "" {
			t.Fatal(result)
		}
	}
}

func TestParserGroupUncertainCommitDoesNotDuplicateUsage(t *testing.T) {
	x, work, sources := groupSources(t, groupRecord, 3)
	ctx := context.Background()
	result, err := x.groupOnce(ctx, work, func(ctx context.Context, batches []store.IndexBatch) error {
		if err := x.Store.CommitIndexGroup(ctx, batches); err != nil {
			t.Fatal(err)
		}
		return errors.New("simulated lost commit response")
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, src := range sources {
		if result[i].Error != "" || result[i].Records != 0 {
			t.Fatal("fallback repeated durable work", result)
		}
		s, e := x.Store.GetSession(ctx, parser.SessionID(src))
		if e != nil || s.TokensIn != 10 || s.TokensOut != 2 {
			t.Fatal(s, e)
		}
	}
}

func TestParserGroupCommitsOnlyAfterPreparation(t *testing.T) {
	x, work, sources := groupSources(t, groupRecord, 8)
	ctx := context.Background()
	calls := 0
	result, err := x.groupOnce(ctx, work, func(ctx context.Context, batches []store.IndexBatch) error {
		calls++
		if len(batches) != 8 {
			t.Fatalf("group size %d", len(batches))
		}
		for _, src := range sources {
			state, e := x.Store.SourceState(ctx, src.SourceID, src.Generation)
			if e != nil || state.IndexedOffset != 0 {
				t.Fatal("preparation published a checkpoint", state, e)
			}
		}
		return x.Store.CommitIndexGroup(ctx, batches)
	})
	if err != nil || calls != 1 || len(result) != 8 {
		t.Fatal(result, calls, err)
	}
	for i, src := range sources {
		if result[i].Records != 1 || result[i].Error != "" {
			t.Fatal(result)
		}
		s, e := x.Store.GetSession(ctx, parser.SessionID(src))
		if e != nil || s.TokensIn != 10 || s.TokensOut != 2 {
			t.Fatal(s, e)
		}
	}
	result, err = x.GroupOnce(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range result {
		if r.Records != 0 || r.Error != "" {
			t.Fatal("retry duplicated progress", result)
		}
	}
}

func TestParserGroupConflictFallsBackFromDurableState(t *testing.T) {
	x, work, sources := groupSources(t, groupRecord, 3)
	ctx := context.Background()
	result, err := x.groupOnce(ctx, work, func(ctx context.Context, batches []store.IndexBatch) error {
		batches[2].FromOffset++
		e := x.Store.CommitIndexGroup(ctx, batches)
		if !errors.Is(e, store.ErrConflict) {
			t.Fatal("fixture failed to create late conflict", e)
		}
		for _, src := range sources {
			state, e := x.Store.SourceState(ctx, src.SourceID, src.Generation)
			if e != nil || state.IndexedOffset != 0 {
				t.Fatal("group did not roll back", state, e)
			}
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, src := range sources {
		if result[i].Records != 1 || result[i].Error != "" {
			t.Fatal(result)
		}
		s, e := x.Store.GetSession(ctx, parser.SessionID(src))
		if e != nil || s.TokensIn != 10 || s.TokensOut != 2 {
			t.Fatal("fallback accounting", s, e)
		}
	}
}

func TestParserGroupKeepsScanOnlyAndBadSourceIndependent(t *testing.T) {
	x, work, _ := groupSources(t, []byte("{\"type\":"), 2)
	work = append(work, SourceWork{SourceID: "missing", Generation: "g"})
	ctx := context.Background()
	result, err := x.GroupOnce(ctx, work)
	if err != nil || len(result) != 3 {
		t.Fatal(result, err)
	}
	for i := 0; i < 2; i++ {
		if result[i].Records != 0 || result[i].Error != "" {
			t.Fatal(result)
		}
		state, e := x.Store.SourceState(ctx, work[i].SourceID, work[i].Generation)
		if e != nil || state.IndexedOffset != 0 {
			t.Fatal(state, e)
		}
		pending, e := x.Store.PendingParserScan(ctx, work[i].SourceID, work[i].Generation)
		if e != nil || pending {
			t.Fatal("partial EOF should await new bytes", pending, e)
		}
		checkpoint, e := parser.DecodeState(state.ParserState)
		if e != nil || checkpoint.ScanOffset != state.DurableOffset {
			t.Fatal("scan progress lost", checkpoint, e)
		}
	}
	if result[2].Error == "" || result[2].Records != 0 {
		t.Fatal("missing source appeared successful", result)
	}
}

func TestParserGroupRejectsInvalidWorkBeforePreparation(t *testing.T) {
	x, work, _ := groupSources(t, groupRecord, 1)
	for _, invalid := range [][]SourceWork{nil, {work[0], work[0]}, make([]SourceWork, 9)} {
		if _, err := x.GroupOnce(context.Background(), invalid); !errors.Is(err, store.ErrInvalid) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := x.GroupOnce(ctx, work)
	if err != nil || result[0].Records != 0 || result[0].Error == "" {
		t.Fatal(result, err)
	}
	state, err := x.Store.SourceState(context.Background(), work[0].SourceID, work[0].Generation)
	if err != nil || state.IndexedOffset != 0 {
		t.Fatal(state, err)
	}
}
