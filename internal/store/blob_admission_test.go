package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestBlobPublicationDoesNotHoldOrganizationWriter(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 1)
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	body := []byte("raw evidence\n")
	go func() {
		_, err := s.IngestChunk(ctx, makeChunk(testSource(), 0, body), bytes.NewReader(body))
		done <- err
	}()
	waitWriterQueue(t, &s.blobMu, 1, 0)
	archiveCtx, archiveCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer archiveCancel()
	yes := true
	m, err := s.PatchMetadata(archiveCtx, "s-000000", MetadataPatch{Archived: &yes, OperationID: "blob-wait-archive"})
	if err != nil || !m.Archived || m.Revision != 1 {
		t.Fatal("raw publication blocked organization", m, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("blob admission did not cancel", err)
	}
}

func TestBlobDurableBeforeWriterAdmissionAndRetry(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	s.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.writeMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := []byte("unacknowledged evidence\n")
	chunk := makeChunk(testSource(), 0, body)
	done := make(chan error, 1)
	go func() {
		_, err := s.IngestChunk(ctx, chunk, bytes.NewReader(body))
		done <- err
	}()
	waitWriterQueue(t, &s.writeMu, 1, 0)
	if err := verifyFile(s.blobPath(chunk.SHA256), chunk.SHA256, chunk.Length); err != nil {
		t.Fatal("raw publication was deferred until after writer admission", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var receipts int
	if err := s.db.QueryRow(`SELECT count(*) FROM chunks`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("cancelled admission acknowledged evidence", receipts, err)
	}
	s.writeMu.Unlock()
	locked = false
	first := ingest(t, s, chunk.Source, 0, string(body))
	replay := ingest(t, s, chunk.Source, 0, string(body))
	if first.ReceiptID != replay.ReceiptID || replay.DurableOffset != int64(len(body)) {
		t.Fatal("retry of orphaned evidence changed receipt", first, replay)
	}
}
