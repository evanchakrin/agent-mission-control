package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/migration"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestImportedSourceAdoptionKeepsReceiptsAndAppends(t *testing.T) {
	ctx := context.Background()
	var receiver http.Handler
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) { receiver.ServeHTTP(w, r) })
	c.cfg.Token = "isolated-migration-adoption-test-token"
	prefix := bytes.Repeat([]byte("x"), protocol.MaxChunkBytes+7)
	path := writeSource(t, root, "history.jsonl", prefix)
	dest := filepath.Join(t.TempDir(), "import")
	plan, err := migration.Preflight(ctx, migration.Options{LegacyStateDir: t.TempDir(), Destination: dest, LocalRoots: []migration.Root{{Path: root, Provider: "codex", MachineID: c.cfg.MachineID}}, AvailableBytes: func(string) (int64, error) { return 1 << 40, nil }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := migration.Run(ctx, plan, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 {
		t.Fatal("missing imported source")
	}
	source := result.Sources[0]
	s, err := store.Open(dest, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	receiver = hub.New(s, c.cfg.Token, "fixture").IngestionHandler()
	yes := true
	id := parser.SessionID(source)
	if _, err = s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "archive-before-adoption", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	// Appends before adoption must not turn the imported seven-byte tail into
	// a larger conflicting chunk at that same accepted offset.
	all := append(append([]byte{}, prefix...), []byte("new data\n")...)
	writeSource(t, root, "history.jsonl", all)
	hash := sha256.Sum256(prefix)
	if err = c.AdoptImportedSource(ctx, source, path, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	if st := stats(t, c); st.UploadedBytes != 0 || st.BacklogBytes != int64(len(prefix)) || st.SpoolBytes != 0 {
		t.Fatal("adoption invented acknowledgement or payload", st)
	}
	cfg, freeSpace := c.cfg, c.freeSpace
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.freeSpace = freeSpace
	reconcile(t, c)
	capture(t, c, 1)
	for range 3 {
		if did, err := c.UploadOnce(ctx); err != nil || !did {
			t.Fatal("adopted receipt or append failed", did, err)
		}
	}
	if st := stats(t, c); st.Sources != 1 || st.BacklogBytes != 0 || st.UploadedBytes != int64(len(all)) {
		t.Fatal("duplicate identity or incomplete transfer", st)
	}
	raw, err := s.OpenSource(ctx, source.SourceID, source.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(raw)
	raw.Close()
	if err != nil || !bytes.Equal(got, all) {
		t.Fatal("imported history changed", err)
	}
	session, err := s.GetSession(ctx, id)
	if err != nil || !session.Metadata.Archived {
		t.Fatal("organization lost", err)
	}
	if err = c.AdoptImportedSource(ctx, source, path, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal("acknowledged adoption could not be replayed", err)
	}
	conflicting := source
	conflicting.Size++
	if err = c.AdoptImportedSource(ctx, conflicting, path, hex.EncodeToString(hash[:])); err == nil {
		t.Fatal("adoption receipt was replaceable")
	}
	if st := stats(t, c); st.Sources != 1 || st.UploadedBytes != int64(len(all)) {
		t.Fatal("rejected adoption changed state")
	}
	renamed := filepath.Join(root, "renamed.jsonl")
	if err = os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c)
	if st := stats(t, c); st.Sources != 1 {
		t.Fatal("rename duplicated adopted source")
	}
	writeSource(t, root, "renamed.jsonl", []byte("rewritten\n"))
	reconcile(t, c)
	var generation string
	var sequence int64
	if err = c.db.QueryRow(`SELECT g.generation,g.generation_sequence FROM generations g JOIN sources s ON s.id=g.source_id AND s.current_generation=g.generation WHERE s.id=?`, source.SourceID).Scan(&generation, &sequence); err != nil {
		t.Fatal(err)
	}
	if generation == source.Generation || sequence != source.GenerationSequence+1 {
		t.Fatal("rewrite did not advance imported generation")
	}
	capture(t, c, 1)
	if did, err := c.UploadOnce(ctx); err != nil || !did {
		t.Fatal("rewritten adopted source failed", did, err)
	}
	raw, err = s.OpenSource(ctx, source.SourceID, source.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(raw)
	raw.Close()
	if err != nil || !bytes.Equal(got, all) {
		t.Fatal("rewrite lost original adopted generation", err)
	}
}

func TestSourceAdoptionRejectsMismatchWithoutCatalogWrites(t *testing.T) {
	for _, failure := range []string{"checksum", "machine", "outside-root", "short-source", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			c, root := testCollector(t, nil)
			data := []byte("verified prefix")
			path := writeSource(t, root, "history.jsonl", data)
			hash := sha256.Sum256(data)
			source := protocol.Source{MachineID: c.cfg.MachineID, SourceID: "import", Generation: "generation", Provider: "codex", Size: int64(len(data))}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "checksum":
				hash = sha256.Sum256([]byte("different"))
			case "machine":
				source.MachineID = "other-machine"
			case "outside-root":
				path = writeSource(t, t.TempDir(), "outside.jsonl", data)
			case "short-source":
				source.Size++
			case "cancelled":
				cancel()
			}
			if err := c.AdoptImportedSource(ctx, source, path, hex.EncodeToString(hash[:])); err == nil {
				t.Fatal("invalid adoption accepted")
			}
			if st := stats(t, c); st.Sources != 0 || st.CapturedBytes != 0 || st.BacklogBytes != 0 {
				t.Fatal("failed adoption changed catalog", st)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("adoption changed source", err)
			}
		})
	}
}
