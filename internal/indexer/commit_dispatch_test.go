package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestCommittedDispatchDoesNotAcquireSecondWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"committed\"}}\n")
	src := protocol.Source{MachineID: "fixture", SourceID: "committed", Generation: "g", Provider: "codex", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ready, err := s.PendingSources(ctx, "", 1)
	if err != nil || len(ready) != 1 {
		t.Fatal(ready, err)
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var held *sql.Tx
	defer func() {
		if held != nil {
			held.Rollback()
		}
	}()
	x := Indexer{Store: s}
	x.Process = func(work context.Context, id, generation string) (int64, error) {
		n, e := x.Once(work, id, generation)
		if e != nil {
			return n, e
		}
		// A different fixture writer starts after the durable parser commit.
		// Dispatch must not wait on a redundant parent-side UPDATE afterward.
		held, e = db.BeginTx(ctx, nil)
		return n, e
	}
	work, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if progress, e := x.processSource(work, ready[0]); e != nil || !progress {
		t.Fatal("committed batch required another writer", progress, e)
	}
	if held == nil {
		t.Fatal("fixture did not hold competing writer")
	}
	if err = held.Rollback(); err != nil {
		t.Fatal(err)
	}
	p, err := s.IndexingProgress(ctx)
	if err != nil || p.Ready != 0 || p.Blocked != 0 {
		t.Fatal("committed work remained queued", p, err)
	}
}
