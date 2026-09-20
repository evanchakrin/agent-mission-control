package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestRawHookEvidenceRebuildResumesWithoutChangingHistoryOrOrganization(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	output, _ := json.Marshal(strings.Repeat("ordinary output ", 2000) + "app.js SyntaxError")
	raw := []byte(`{"type":"assistant","uuid":"edit","message":{"id":"edit","model":"known","usage":{"input_tokens":10,"output_tokens":2},"content":[{"type":"tool_use","id":"edit-call","name":"Edit","input":{"file_path":"app.js"}}]}}` + "\n" + `{"type":"user","uuid":"result","message":{"content":[{"type":"tool_result","tool_use_id":"edit-call","content":` + string(output) + `}]}}` + "\n")
	// More than one verification page makes the interruption deterministic.
	blocks := make([]map[string]string, 300)
	for i := range blocks {
		blocks[i] = map[string]string{"type": "thinking", "thinking": fmt.Sprintf("synthetic evidence %d", i)}
	}
	page, err := json.Marshal(map[string]any{"type": "assistant", "uuid": "verification-pages", "message": map[string]any{"id": "verification-pages", "content": blocks}})
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, append(page, '\n')...)
	src := protocol.Source{MachineID: "fixture", SourceID: "fixture", Generation: "g", Provider: "claude", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	if _, err := x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	id := parser.SessionID(src)
	yes, name, note := true, "Owner archived name", "Owner note"
	if _, err := s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "owner", Archived: &yes, Name: &name, Note: &note}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.JavaScriptHookEvidence(ctx, id, "")
	if err != nil || evidence.Errors != 1 {
		t.Fatal(evidence, err)
	}
	job, err := s.BeginRebuild(ctx, id, "recover-hook-evidence", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.RebuildOnce(ctx, job.Revision); err != nil {
		t.Fatal(err)
	}
	stage, err := s.ProjectionRevision(ctx, job.Revision)
	if err != nil || stage.State != "verifying" {
		t.Fatal("fixture must interrupt before publication", stage.State, err)
	}
	still, err := s.JavaScriptHookEvidence(ctx, id, evidence.Snapshot)
	if err != nil || still != evidence {
		t.Fatal("staged evidence leaked", still, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	x.Store = s
	for i := 0; i < 12; i++ {
		if _, err := x.RebuildOnce(ctx, job.Revision); err != nil {
			t.Fatal(err)
		}
		stage, err = s.ProjectionRevision(ctx, job.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if stage.State == "active" {
			break
		}
	}
	if stage.State != "active" {
		t.Fatal(stage.State)
	}
	after, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Metadata, after.Metadata) || before.TokensIn != after.TokensIn || before.TokensOut != after.TokensOut || before.EventCount != after.EventCount {
		t.Fatal("rebuild changed accounting or organization", before, after)
	}
	recovered, err := s.JavaScriptHookEvidence(ctx, id, "")
	if err != nil || recovered.State != "indexed-history" || !recovered.Edited || recovered.Errors != 1 || recovered.IndexedOffset != int64(len(raw)) || recovered.Snapshot == evidence.Snapshot {
		t.Fatal(recovered, err)
	}
	reader, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(actual, raw) {
		t.Fatal("raw source changed", err)
	}
	retry, err := s.BeginRebuild(ctx, id, "recover-hook-evidence", parser.Version)
	if err != nil || retry.Revision != job.Revision {
		t.Fatal("retry duplicated rebuild", retry, err)
	}
}
