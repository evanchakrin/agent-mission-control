package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"testing"
)

func TestPartialRecordCheckpointAndUsageRevision(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "generation", Provider: "claude", NativeID: "native"}
	first := []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"content\":\"hi\",\"usage\":{\"input_tokens\":10}}}\n{\"type\":")
	send := func(off int64, b []byte) {
		h := sha256.Sum256(b)
		src.Size = off + int64(len(b))
		_, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: off, Length: int64(len(b)), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
	}
	send(0, first)
	x := Indexer{Store: s}
	n, err := x.Once(ctx, src.SourceID, src.Generation)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	checkpoint, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || checkpoint.IndexedOffset != int64(bytes.IndexByte(first, '\n')+1) {
		t.Fatal("partial line checkpoint", checkpoint, err)
	}
	if n, err = x.Once(ctx, src.SourceID, src.Generation); err != nil || n != 0 {
		t.Fatal("partial repeated", n, err)
	}
	send(int64(len(first)), []byte("\"assistant\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"content\":\"bye\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}}\n"))
	if n, err = x.Once(ctx, src.SourceID, src.Generation); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	session, err := s.GetSession(ctx, parser.SessionID(src))
	if err != nil || session.TokensIn != 10 || session.TokensOut != 20 {
		t.Fatal("duplicate usage", session, err)
	}
	yes := true
	_, err = s.PatchMetadata(ctx, session.ID, store.MetadataPatch{Archived: &yes, OperationID: "archive-1", Revision: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	session, _ = s.GetSession(ctx, session.ID)
	if !session.Metadata.Archived {
		t.Fatal("indexer unarchived session")
	}
}
