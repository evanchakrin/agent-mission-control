package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestResumedLargeRecordDoesNotAccumulateFollowingRecords(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := `{"type":"response_item","payload":{"type":"custom_tool_call_output","output":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("a", 5<<20) + `"}]}}` + "\n"
	tail := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"native\"}}\n{}\n"
	raw := []byte(first + tail)
	src := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", Size: int64(len(raw))}
	for off := 0; off < len(raw); {
		end := min(off+(1<<20), len(raw))
		part := raw[off:end]
		hash := sha256.Sum256(part)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: int64(off), Length: int64(len(part)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(part)); err != nil {
			t.Fatal(err)
		}
		off = end
	}
	x := Indexer{Store: s}
	if n, e := x.Once(ctx, "s", "g"); e != nil || n != 0 {
		t.Fatal("initial bounded scan", n, e)
	}
	if n, e := x.Once(ctx, "s", "g"); e != nil || n != 1 {
		t.Fatal("large record must commit alone", n, e)
	}
	state, e := s.SourceState(ctx, "s", "g")
	if e != nil || state.IndexedOffset != int64(len(first)) {
		t.Fatal("wrong boundary", state.IndexedOffset, e)
	}
	if n, e := x.Once(ctx, "s", "g"); e != nil || n != 2 {
		t.Fatal("following records not resumed", n, e)
	}
}
