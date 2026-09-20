package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestNativeWorkerLargeInlineImage(t *testing.T) {
	nativeWorkerLargeInlineImage(t, false)
}

func TestNativeWorkerPreparesLargeInlineImage(t *testing.T) {
	nativeWorkerLargeInlineImage(t, true)
}

func nativeWorkerLargeInlineImage(t *testing.T, prepare bool) {
	t.Helper()
	binary := os.Getenv("AMC_IMAGE_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, (6<<20)-(64<<10))
	_, _ = rand.New(rand.NewSource(1)).Read(noise)
	output := []any{map[string]any{"type": "input_text", "text": "beforeimageneedle"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(noise)}, map[string]any{"type": "input_text", "text": "afterimageneedle"}}
	raw, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "image-call", "output": output}})
	raw = append(raw, '\n')
	if len(raw) > parser.MaxRecordBytes {
		t.Fatal("fixture exceeds supported record size")
	}
	src := protocol.Source{MachineID: "m", SourceID: "s", Generation: "g", Provider: "codex", Size: int64(len(raw))}
	for off := 0; off < len(raw); {
		end := min(off+(1<<20), len(raw))
		chunk := raw[off:end]
		hash := sha256.Sum256(chunk)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: int64(off), Length: int64(len(chunk)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(chunk)); err != nil {
			t.Fatal(err)
		}
		off = end
	}
	worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
	defer worker.Close()
	for i := 0; i < 4; i++ {
		if prepare {
			var result Preparation
			result, err = worker.Prepare(ctx, src.SourceID, src.Generation)
			if err == nil && result.Batch != nil {
				err = s.CommitIndex(ctx, *result.Batch)
			} else if err == nil && result.Scan != nil {
				err = s.SaveParserScan(ctx, result.Scan.Source, result.Scan.Checkpoint)
			}
		} else {
			_, err = worker.Process(ctx, src.SourceID, src.Generation)
		}
		if err != nil {
			t.Fatal(err)
		}
		state, e := s.SourceState(ctx, src.SourceID, src.Generation)
		if e != nil {
			t.Fatal(e)
		}
		if state.IndexedOffset == int64(len(raw)) {
			for _, needle := range []string{"beforeimageneedle", "afterimageneedle"} {
				page, e := s.SearchPinned(ctx, store.SearchQuery{Text: needle, Limit: 10}, "")
				if e != nil || len(page.Events) != 1 {
					t.Fatalf("text not searchable: %s %v", needle, e)
				}
			}
			reader, e := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer reader.Close()
			actual := make([]byte, len(raw))
			if _, e = io.ReadFull(reader, actual); e != nil || !bytes.Equal(raw, actual) {
				t.Fatal("raw evidence changed", e)
			}
			t.Logf("indexed %d source bytes through 192 MiB worker; surrounding text searchable", len(raw))
			return
		}
	}
	t.Fatal("large image record did not reach durable checkpoint")
}
