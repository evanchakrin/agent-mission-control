package store

import (
	"context"
	"io"
	"testing"
)

// A slow parser or raw download must not pin a SQLite read snapshot while it
// holds an immutable source file open. Ingestion/checkpointing continue between
// reads without changing the reader's captured end offset.
func TestPausedSourceReaderDoesNotPinCheckpoint(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "fixture\n")
	r, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := make([]byte, 1)
	if _, err = io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "f" {
		t.Fatal("wrong source byte")
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TABLE source_checkpoint_fixture(data BLOB); INSERT INTO source_checkpoint_fixture VALUES(zeroblob(5242880));`); err != nil {
		t.Fatal(err)
	}
	s.checkpointOnce(ctx)
	status := s.CheckpointStatus()
	if status.State != "caught-up" || status.CopiedFrames != status.LogFrames {
		t.Fatal("paused source reader prevented checkpoint", status)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != "ixture\n" {
		t.Fatal("checkpoint changed open raw reader", string(rest), err)
	}
}
