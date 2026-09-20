package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestCodexIdentityRebuildRepairsCounterScopeAndPreservesArchive(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"type":"session_meta","payload":{"id":"child","parent_thread_id":"parent","forked_from_id":"parent"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":20}}}}
{"type":"session_meta","payload":{"id":"parent"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":20}}}}
`)
	src := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "g", Provider: "codex", Size: int64(len(data))}
	sum := sha256.Sum256(data)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	id := "preserved-legacy-session-key"
	// The previous parser emitted the same cumulative total under two scopes.
	old := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, ToOffset: src.Size, ParserState: json.RawMessage(`{"version":"3","nativeId":"parent"}`), Session: store.Session{ID: id, NativeID: "parent"}, Usage: []store.UsageObservation{
		{ID: "old-child", AgentID: "main", CounterScope: "thread:child", Kind: "incomplete-attribution", TokensIn: 1000, TokensOut: 20},
		{ID: "old-parent", AgentID: "main", CounterScope: "thread:parent", Kind: "incomplete-attribution", TokensIn: 1000, TokensOut: 20},
	}}
	if err = s.CommitIndex(ctx, old); err != nil {
		t.Fatal(err)
	}
	audit, err := (&Indexer{Store: s}).CheckVersions(ctx, "", 50)
	if err != nil || len(audit.Issues) != 1 || audit.Issues[0].SessionID != id {
		t.Fatalf("version audit guessed session key instead of preserving alias: %+v %v", audit, err)
	}
	yes, name := true, "Owner label"
	metadata, err := s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "archive", Archived: &yes, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.BeginRebuild(ctx, id, "identity-rebuild", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, id)
	if err != nil || before.TokensIn != 2000 || before.NativeID != "parent" {
		t.Fatalf("old projection replaced before publication: %+v %v", before, err)
	}
	x := Indexer{Store: s}
	for i := 0; i < 10; i++ {
		if _, err = x.RebuildOnce(ctx, job.Revision); err != nil {
			t.Fatal(err)
		}
		status, e := s.ProjectionRevision(ctx, job.Revision)
		if e != nil {
			t.Fatal(e)
		}
		if status.State == "active" {
			break
		}
		if i == 9 {
			t.Fatal("rebuild did not publish")
		}
	}
	after, err := s.GetSession(ctx, id)
	if err == nil && (after.ForkedFromID != "parent" || after.ParentThreadID != "parent") {
		t.Fatal("rebuild lost explicit fork relationship", after)
	}
	if err != nil || after.NativeID != "child" || after.TokensIn != 1000 || after.TokensOut != 20 || !after.Metadata.Archived || after.Metadata.Name != name || after.Metadata.Revision != metadata.Revision {
		t.Fatalf("incorrect rebuilt projection: %+v %v", after, err)
	}
	r, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(raw, data) {
		t.Fatal("raw evidence changed", err)
	}
}
