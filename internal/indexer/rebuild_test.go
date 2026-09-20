package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestParserVersionMismatchPreservesProjectionRawAndOrganization(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := []byte("{\"type\":\"assistant\",\"uuid\":\"a\",\"message\":{\"id\":\"reply\",\"model\":\"known\",\"content\":\"sanitized fixture\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":2}}}\n")
	source := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "g", Provider: "claude", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: source, Offset: 0, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	state, _ := parser.DecodeState(nil)
	record := parser.Record(source, &state, 0, raw)
	// Simulate an on-disk projection from the previous candidate parser version.
	state.Version = "1"
	checkpoint, _ := json.Marshal(state)
	id := parser.SessionID(source)
	if err := s.CommitIndex(ctx, store.IndexBatch{SourceID: source.SourceID, Generation: source.Generation, FromOffset: 0, ToOffset: int64(len(raw)), Session: store.Session{ID: id, Title: "keep title", Completeness: "indexed-source"}, Events: record.Events, Usage: record.Usage, ParserState: checkpoint}); err != nil {
		t.Fatal(err)
	}
	yes := true
	name := "keep custom name"
	notes := "keep notes"
	project := "keep project"
	if _, err := s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "archive", Revision: 0, Archived: &yes, Name: &name, Note: &notes, Project: &project}); err != nil {
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
	if n, err := x.Once(ctx, source.SourceID, source.Generation); n != 0 || !errors.Is(err, parser.ErrRebuildRequired) {
		t.Fatal("mismatch silently continued", n, err)
	}
	page, err := x.CheckVersions(ctx, "", 1)
	if err != nil || page.Checked != 1 || len(page.Issues) != 1 || page.Issues[0].Code != "rebuild-required" || page.Issues[0].CheckpointVersion != "1" || page.Issues[0].RequiredVersion != parser.Version {
		t.Fatal("caught-up source audit missed old parser", page, err)
	}
	if page.Next == "" {
		t.Fatal("pagination cursor missing")
	}
	if page.Issues[0].SessionID != id {
		t.Fatal("version audit omitted stable session identity", page.Issues)
	}
	end, err := x.CheckVersions(ctx, page.Next, 1)
	if err != nil || end.Checked != 0 || end.Next != "" {
		t.Fatal("bad cursor", end, err)
	}
	after, err := s.GetSession(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("version guard changed user projection", before, after, err)
	}
	afterUsage, err := s.GetUsage(ctx, id, "", 100)
	if err != nil || !reflect.DeepEqual(usage, afterUsage) {
		t.Fatal("version guard changed usage", err)
	}
	afterState, err := s.SourceState(ctx, source.SourceID, source.Generation)
	if err != nil || afterState.IndexedOffset != int64(len(raw)) || !bytes.Equal(checkpoint, afterState.ParserState) {
		t.Fatal("version guard reset checkpoint", err)
	}
	reader, err := s.OpenSource(ctx, source.SourceID, source.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(raw, actual) {
		t.Fatal("version guard changed original raw", err)
	}
}
