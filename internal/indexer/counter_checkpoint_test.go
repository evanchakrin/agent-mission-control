package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestDamagedCounterCannotAdvanceOrReplaceProjection(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	line := []byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":100,\"output_tokens\":10}}}}\n")
	raw := append(append([]byte(nil), line...), line...)
	source := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: source, Offset: 0, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	state, _ := parser.DecodeState(nil)
	record := parser.Record(source, &state, 0, line)
	state.Counters["s"] = parser.Counter{Set: true, Input: -100}
	checkpoint, _ := json.Marshal(state)
	id := parser.SessionID(source)
	if err = s.CommitIndex(ctx, store.IndexBatch{SourceID: "s", Generation: "g", ToOffset: int64(len(line)), Session: store.Session{ID: id, Title: "retained"}, Events: record.Events, Usage: record.Usage, ParserState: checkpoint}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err = s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := s.GetUsage(ctx, id, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	if n, err := x.Once(ctx, "s", "g"); n != 0 || err == nil || !strings.Contains(err.Error(), "inconsistent usage counter") {
		t.Fatalf("invalid checkpoint continued: %d %v", n, err)
	}
	after, err := s.GetSession(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("session or organization changed", err)
	}
	afterUsage, err := s.GetUsage(ctx, id, "", 100)
	if err != nil || !reflect.DeepEqual(usage, afterUsage) {
		t.Fatal("usage changed", err)
	}
	stored, err := s.SourceState(ctx, "s", "g")
	if err != nil || stored.IndexedOffset != int64(len(line)) || !bytes.Equal(stored.ParserState, checkpoint) {
		t.Fatal("checkpoint replaced", err)
	}
}
