package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestUndoRecoveryWithExistingJavaScriptEvidenceAdvancesAfterPublication(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	x := &indexer.Indexer{Store: s}
	before := map[string]store.Session{}
	for i := 0; i < 3; i++ {
		raw := []byte(`{"type":"assistant","uuid":"fixture","message":{"id":"fixture","model":"known","usage":{"input_tokens":11,"output_tokens":3},"content":[{"type":"tool_use","id":"edit","name":"Bash","input":{"command":"git restore -- app.js"}}]}}` + "\n")
		src := protocol.Source{MachineID: "fixture", SourceID: fmt.Sprint("source-", i), Generation: "g", Provider: "claude", Size: int64(len(raw))}
		hash := sha256.Sum256(raw)
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := x.Once(ctx, src.SourceID, src.Generation); err != nil {
			t.Fatal(err)
		}
		id := parser.SessionID(src)
		yes := true
		if _, err := s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: fmt.Sprint("archive-", i), Archived: &yes}); err != nil {
			t.Fatal(err)
		}
		row, err := s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = row
	}
	// Only this disposable database: simulate pre-evidence projections.
	db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DELETE FROM git_undo_checkpoints"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	h := hub.New(s, "fixture-owner-boundary", "test")
	server := httptest.NewServer(h.OwnerHandler())
	defer server.Close()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		report, err := queueEvidenceRebuilds(ctx, server.Client(), server.URL, "stable-recovery", 1, "undo")
		if err != nil || report.Complete || !report.LimitReached || len(report.Jobs) != 1 || report.Checked != 1 || report.Skipped != 0 {
			t.Fatal(report, err)
		}
		job := report.Jobs[0]
		if seen[job.SessionID] {
			t.Fatal("covered session queued again", job)
		}
		seen[job.SessionID] = true
		retry, err := queueEvidenceRebuilds(ctx, server.Client(), server.URL, "stable-recovery", 1, "undo")
		if err != nil || len(retry.Jobs) != 1 || retry.Jobs[0].Revision != job.Revision {
			t.Fatal("pending retry changed revision", retry, err)
		}
		for step := 0; step < 12; step++ {
			if _, err := x.RebuildOnce(ctx, job.Revision); err != nil {
				t.Fatal(err)
			}
			r, err := s.ProjectionRevision(ctx, job.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if r.State == "active" {
				break
			}
		}
		evidence, err := s.GitUndoHistory(ctx, job.SessionID, "", 0, 1)
		if err != nil || evidence.State != "indexed-history" || evidence.Attempts != 1 || len(evidence.Events) != 1 {
			t.Fatal(evidence, err)
		}
		after, err := s.GetSession(ctx, job.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		prior := before[job.SessionID]
		if !reflect.DeepEqual(prior.Metadata, after.Metadata) || prior.TokensIn != after.TokensIn || prior.TokensOut != after.TokensOut || prior.EventCount != after.EventCount {
			t.Fatal("recovery changed owner state or accounting")
		}
	}
	done, err := queueEvidenceRebuilds(ctx, server.Client(), server.URL, "stable-recovery", 1, "undo")
	if err != nil || !done.Complete || len(done.Jobs) != 0 || done.Checked != 0 || done.Skipped != 0 {
		t.Fatal(done, err)
	}
}
