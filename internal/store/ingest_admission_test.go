package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// Done is first selected at writer admission in these paths. Observing it lets
// the test cancel an actual waiter rather than relying on an arbitrary sleep.
type admissionContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *admissionContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestIngestAndHeartbeatAbandonCanceledWriterAdmission(t *testing.T) {
	for _, role := range []string{"chunk", "heartbeat"} {
		t.Run(role, func(t *testing.T) {
			s := openTestStore(t, Options{ExternalCheckpointOwner: true})
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &admissionContext{Context: base, entered: make(chan struct{})}
			data := []byte("retained collector evidence\n")
			chunk := makeChunk(testSource(), 0, data)
			call := func(ctx context.Context) error {
				if role == "heartbeat" {
					return s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "machine-1", Name: "fixture"})
				}
				_, err := s.IngestChunk(ctx, chunk, bytes.NewReader(data))
				return err
			}
			s.writeMu.Lock()
			var once sync.Once
			unlock := func() { once.Do(s.writeMu.Unlock) }
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			defer unlock()
			select {
			case <-ctx.entered:
			case <-time.After(2 * time.Second):
				cancel()
				unlock()
				<-done
				t.Fatal("request did not reach writer admission")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				unlock()
				<-done
				t.Fatal("canceled request waited for the active writer")
			}
			unlock()
			for _, table := range []string{"machines", "sources", "chunks", "changes"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal(table, count, err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(s.dir, "blobs"))
			expectedEntries := 0
			if role == "chunk" {
				expectedEntries = 1
			}
			if err != nil || len(entries) != expectedEntries || s.pendingBytes != 0 {
				t.Fatal("admission leaked staging or capacity", entries, s.pendingBytes, err)
			}
			if role == "chunk" {
				// Raw publication now precedes writer admission. Cancellation must
				// leave only the durable reusable blob, never an incoming temp file
				// or a ledger receipt (the empty-table checks above still apply).
				if !entries[0].IsDir() || entries[0].Name() != chunk.SHA256[:2] {
					t.Fatal("unexpected raw publication entry", entries)
				}
				files, err := os.ReadDir(filepath.Join(s.dir, "blobs", entries[0].Name()))
				if err != nil || len(files) != 1 || files[0].Name() != chunk.SHA256 || files[0].IsDir() {
					t.Fatal("unexpected orphan evidence", files, err)
				}
				if err := verifyFile(s.blobPath(chunk.SHA256), chunk.SHA256, chunk.Length); err != nil {
					t.Fatal("orphan evidence is not reusable", err)
				}
			}
			if err := call(context.Background()); err != nil {
				t.Fatal("retry failed", err)
			}
			if role == "chunk" {
				state, err := s.SourceState(context.Background(), chunk.Source.SourceID, chunk.Source.Generation)
				if err != nil || state.DurableOffset != int64(len(data)) {
					t.Fatal(state, err)
				}
			} else {
				machines, err := s.Machines(context.Background())
				if err != nil || len(machines) != 1 {
					t.Fatal(machines, err)
				}
			}
		})
	}
}
