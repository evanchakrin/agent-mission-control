package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestRetryDispatchBypassesCatalogWithoutBusyPollingOrDrainingQueue(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "z"} {
		src := protocol.Source{MachineID: "fixture", SourceID: id, Generation: "g", Provider: "codex"}
		raw := []byte(`{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n")
		src.Size = int64(len(raw))
		hash := sha256.Sum256(raw)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if id != "z" {
			if err = s.RecordIndexAttempt(ctx, id, "g", src.Size, false, errors.New("fixture contention")); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Only the private fixture's due timestamps are advanced, not production time.
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "ledger.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE index_work SET ready_at='' WHERE state='blocked'`); err != nil {
		t.Fatal(err)
	}
	ready, err := s.PendingSources(ctx, "", 100)
	if err != nil || len(ready) != 3 {
		t.Fatal(ready, err)
	}
	cursor := store.SourceCursor(ready[2])
	if after, e := s.PendingSources(ctx, cursor, 100); e != nil || len(after) != 0 {
		t.Fatal(after, e)
	}
	x := Indexer{Store: s}
	start := time.Now()
	last := start
	if progress, e := x.retryIfDue(ctx, &last, start.Add(29*time.Second)); e != nil || progress {
		t.Fatal("early retry", progress, e)
	}
	for turn := 1; turn <= 2; turn++ {
		if progress, e := x.retryIfDue(ctx, &last, start.Add(time.Duration(turn)*30*time.Second)); e != nil || !progress {
			t.Fatal("due retry did not progress", progress, e)
		}
		p, e := s.IndexingProgress(ctx)
		if e != nil || p.Blocked != int64(2-turn) || p.Ready != 1 {
			t.Fatal("retry drained queue or consumed ordinary work", p, e)
		}
		if progress, e := x.retryIfDue(ctx, &last, last.Add(time.Second)); e != nil || progress {
			t.Fatal("retry busy polled", progress, e)
		}
	}
	if ready, e := s.PendingSources(ctx, "", 100); e != nil || len(ready) != 1 || ready[0].Source.SourceID != "z" {
		t.Fatal("ordinary work changed", ready, e)
	}
}
