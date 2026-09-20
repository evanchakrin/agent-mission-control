package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Exercise the real supervised child, not just an in-process canceled context.
// The held writer and all accepted bytes belong exclusively to a temporary store.
func TestNativeWorkerRecoversAfterBlockedWriteCancellation(t *testing.T) {
	binary := os.Getenv("AMC_RECOVERY_WORKER_BINARY")
	if binary == "" {
		t.Skip("native worker binary not selected")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := protocol.Source{MachineID: "recovery-machine", SourceID: "recovery-source", Generation: "generation", Provider: "codex"}
	var end int64
	appendRecord := func(raw string) {
		t.Helper()
		data := []byte(raw + "\n")
		sum := sha256.Sum256(data)
		src.Size = end + int64(len(data))
		_, e := s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: end, Length: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		end = src.Size
	}
	appendRecord(`{"type":"session_meta","payload":{"id":"worker-recovery"}}`)
	worker := ProcessWorker{Executable: binary, DataDir: dir, Lifetime: ctx}
	defer worker.Close()
	if _, err = worker.Process(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	before := end
	appendRecord(`{"type":"event_msg","payload":{"type":"user_message","message":"recoveryneedle"}}`)
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	blocked, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	_, err = worker.Process(blocked, src.SourceID, src.Generation)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected canceled blocked write, got %v", err)
	}
	if worker.child != nil {
		t.Fatal("canceled child was not released")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	state, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || state.IndexedOffset != before || state.DurableOffset != end {
		t.Fatalf("cancellation changed durable evidence/checkpoint: indexed=%d durable=%d error=%v", state.IndexedOffset, state.DurableOffset, err)
	}
	if _, err = worker.Process(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	state, err = s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || state.IndexedOffset != end {
		t.Fatalf("replacement worker did not resume: %+v %v", state, err)
	}
	page, err := s.SearchPinned(ctx, store.SearchQuery{Text: "recoveryneedle", Limit: 10}, "")
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("resumed history missing or duplicated: count=%d error=%v", len(page.Events), err)
	}
	t.Log("blocked writer cancellation retained accepted bytes and checkpoint; replacement worker indexed the record exactly once")
}
