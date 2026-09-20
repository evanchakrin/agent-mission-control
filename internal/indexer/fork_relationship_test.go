package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestForkRelationshipSurvivesIndexAppendAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	src := protocol.Source{MachineID: "machine", SourceID: "fork-source", Generation: "g", Provider: "codex"}
	var offset int64
	appendRecords := func(raw string) {
		t.Helper()
		b := []byte(raw)
		h := sha256.Sum256(b)
		src.Size = offset + int64(len(b))
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: offset, Length: int64(len(b)), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		offset += int64(len(b))
		x := Indexer{Store: s}
		if _, err := x.Once(ctx, src.SourceID, src.Generation); err != nil {
			t.Fatal(err)
		}
	}
	check := func(archived bool) {
		t.Helper()
		session, err := s.GetSession(ctx, parser.SessionID(src))
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(session)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["forkedFromId"] != "history-origin" || session.ParentThreadID != "spawn-parent" || session.NativeID != "child" {
			t.Fatalf("fork and spawn relationships lost or conflated: %s", b)
		}
		if session.TokensIn != 100 || session.TokensOut != 5 || session.Metadata.Archived != archived {
			t.Fatal("usage or organization changed", session)
		}
		page, err := s.QuerySessions(ctx, store.SessionQuery{Limit: 10})
		if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ForkedFromID != "history-origin" {
			t.Fatal("catalog lost fork relationship", page, err)
		}
	}
	appendRecords(`{"type":"session_meta","payload":{"id":"child","parent_thread_id":"spawn-parent","forked_from_id":"history-origin"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":5}}}}` + "\n")
	check(false)
	yes := true
	if _, err := s.PatchMetadata(ctx, parser.SessionID(src), store.MetadataPatch{Archived: &yes, OperationID: "archive-fork"}); err != nil {
		t.Fatal(err)
	}
	appendRecords(`{"type":"session_meta","payload":{"id":"copied-owner","forked_from_id":"wrong-origin"}}` + "\n" +
		`{"type":"session_meta","payload":{"id":"child"}}` + "\n")
	check(true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	check(true)
}
