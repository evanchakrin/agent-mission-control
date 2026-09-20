package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testSource() protocol.Source {
	return protocol.Source{MachineID: "machine-1", SourceID: "source-1", Generation: "generation-1", Provider: "claude", NativeID: "native-1", Path: "projects/session.jsonl", ModifiedAt: time.Now().UTC()}
}
func ingest(t *testing.T, s *Store, src protocol.Source, off int64, data string) protocol.Receipt {
	t.Helper()
	c := makeChunk(src, off, []byte(data))
	r, err := s.IngestChunk(context.Background(), c, bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func makeChunk(src protocol.Source, off int64, data []byte) protocol.Chunk {
	h := sha256.Sum256(data)
	src.Size = off + int64(len(data))
	return protocol.Chunk{Source: src, Offset: off, Length: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
}
func batch(src protocol.Source, from, to int64) IndexBatch {
	return IndexBatch{SourceID: src.SourceID, Generation: src.Generation, FromOffset: from, ToOffset: to, Session: Session{ID: "session-1", Title: "Historical session", Project: "project", LastActivity: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC), Completeness: "complete"}}
}

func TestChunkDurabilityLostACKAndStreaming(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ctx := context.Background()
	first := ingest(t, s, src, 0, "first\n")
	ingest(t, s, src, 6, "second\n")
	if first.DurableOffset != 6 || first.IndexedOffset != 0 {
		t.Fatalf("bad receipt: %+v", first)
	}
	epoch := s.RecoveryEpoch()
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.RecoveryEpoch() != epoch {
		t.Fatal("ordinary restart changed recovery epoch")
	}
	retry := ingest(t, reopened, src, 0, "first\n")
	if retry.ReceiptID != first.ReceiptID || retry.DurableOffset != first.DurableOffset {
		t.Fatalf("lost ACK replay changed receipt: %+v", retry)
	}
	r, err := reopened.OpenSource(ctx, src.SourceID, src.Generation, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "st\nsecond\n" {
		t.Fatalf("stream: %q %v", got, err)
	}
	st, err := reopened.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || st.Source.MachineID != src.MachineID || st.DurableOffset != 13 {
		t.Fatalf("state: %+v %v", st, err)
	}
}

func TestInvalidChunksNeverAdvanceLedger(t *testing.T) {
	for _, which := range []string{"short", "long", "checksum", "offset", "oversized", "identity"} {
		t.Run(which, func(t *testing.T) {
			s := openTestStore(t, Options{})
			src := testSource()
			data := []byte("body")
			c := makeChunk(src, 0, data)
			switch which {
			case "short":
				data = data[:3]
			case "long":
				data = append(data, 'x')
			case "checksum":
				c.SHA256 = fmt.Sprintf("%064d", 0)
			case "offset":
				c.Offset = 1
				c.Source.Size++
			case "oversized":
				c.Length = protocol.MaxChunkBytes + 1
				c.Source.Size = c.Length
			case "identity":
				c.Source.SourceID = ""
			}
			if _, err := s.IngestChunk(context.Background(), c, bytes.NewReader(data)); err == nil {
				t.Fatal("invalid chunk accepted")
			}
			if _, err := s.SourceState(context.Background(), src.SourceID, src.Generation); !errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid chunk created ledger: %v", err)
			}
		})
	}
}

func TestRawOrphanAfterFailedCommitIsRecoverable(t *testing.T) {
	failed := true
	s := openTestStore(t, Options{BeforeCommit: func() error {
		if failed {
			return errors.New("simulated process death before commit")
		}
		return nil
	}})
	src := testSource()
	data := []byte("raw evidence survives")
	c := makeChunk(src, 0, data)
	if _, err := s.IngestChunk(context.Background(), c, bytes.NewReader(data)); err == nil {
		t.Fatal("fault did not fire")
	}
	if _, err := s.SourceState(context.Background(), src.SourceID, src.Generation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted offset survived: %v", err)
	}
	if err := verifyFile(s.blobPath(c.SHA256), c.SHA256, int64(len(data))); err != nil {
		t.Fatal("durable raw orphan missing", err)
	}
	failed = false
	ingest(t, s, src, 0, string(data))
	st, err := s.SourceState(context.Background(), src.SourceID, src.Generation)
	if err != nil || st.DurableOffset != int64(len(data)) {
		t.Fatalf("retry failed %+v %v", st, err)
	}
}

func TestConcurrentDuplicateChunksAndNamespaceCollision(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	data := []byte("one immutable chunk")
	c := makeChunk(src, 0, data)
	var wg sync.WaitGroup
	results := make(chan protocol.Receipt, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.IngestChunk(context.Background(), c, bytes.NewReader(data))
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	receipt := ""
	for r := range results {
		if receipt != "" && receipt != r.ReceiptID {
			t.Fatal("duplicate created another receipt")
		}
		receipt = r.ReceiptID
	}
	bad := c
	bad.Source.MachineID = "other-machine"
	if _, err := s.IngestChunk(context.Background(), bad, bytes.NewReader(data)); !errors.Is(err, ErrConflict) {
		t.Fatalf("source identity collision accepted: %v", err)
	}
	bad = makeChunk(src, 0, []byte("different body"))
	if _, err := s.IngestChunk(context.Background(), bad, bytes.NewReader([]byte("different body"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("range conflict accepted: %v", err)
	}
}

func TestEmptySourceCanLaterGrow(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ingest(t, s, src, 0, "")
	r := ingest(t, s, src, 0, "growth")
	if r.DurableOffset != 6 {
		t.Fatal(r)
	}
}

func TestSQLiteWALFixedVersionGate(t *testing.T) {
	for _, version := range []string{"3.51.2", "3.50.0", "bad", "3.51", "3.-1.0"} {
		if supportedSQLite(version) {
			t.Fatalf("unsafe/unknown SQLite %s accepted", version)
		}
	}
	for _, version := range []string{"3.51.3", "3.53.4", "4.0.0"} {
		if !supportedSQLite(version) {
			t.Fatalf("supported SQLite %s refused", version)
		}
	}
}

func TestOutOfOrderGenerationsUseDurableOrdinal(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	newest := testSource()
	newest.Generation = "newest"
	newest.GenerationSequence = 2
	ingest(t, s, newest, 0, "new\n")
	b := batch(newest, 0, 4)
	b.Usage = []UsageObservation{{ID: "newest-usage", TokensOut: 10}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []int64{1, 0} {
		older := newest
		older.Generation = fmt.Sprintf("older-%d", sequence)
		older.GenerationSequence = sequence
		ingest(t, s, older, 0, "old\n")
		b = batch(older, 0, 4)
		b.Usage = []UsageObservation{{ID: fmt.Sprintf("old-%d", sequence), TokensOut: 99}}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetSession(ctx, "session-1")
	if err != nil || got.Generation != "newest" || got.TokensOut != 10 {
		t.Fatalf("late generation superseded: %+v %v", got, err)
	}
	conflict := newest
	conflict.Generation = "same-ordinal-different-generation"
	c := makeChunk(conflict, 0, []byte("bad\n"))
	if _, err = s.IngestChunk(ctx, c, bytes.NewReader([]byte("bad\n"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("ordinal reused: %v", err)
	}
	conflict = newest
	conflict.GenerationSequence = 3
	c = makeChunk(conflict, 0, []byte("new\n"))
	if _, err = s.IngestChunk(ctx, c, bytes.NewReader([]byte("new\n"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing generation ordinal changed: %v", err)
	}
}

func TestSlowUploadDoesNotHoldMetadataWriter(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	archiveSource := testSource()
	archiveSource.SourceID = "archive-source"
	archiveSource.NativeID = "archive-native"
	ingest(t, s, archiveSource, 0, "fixture\n")
	if err := s.CommitIndex(context.Background(), batch(archiveSource, 0, 8)); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	c := makeChunk(src, 0, []byte("slow"))
	go func() { _, err := s.IngestChunk(context.Background(), c, r); done <- err }()
	if _, err := w.Write([]byte("s")); err != nil {
		t.Fatal(err)
	}
	hb := make(chan error, 1)
	go func() { hb <- s.RecordHeartbeat(context.Background(), protocol.Heartbeat{MachineID: "other"}) }()
	select {
	case err := <-hb:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow upload blocked independent writer")
	}
	archived := true
	archiveDone := make(chan error, 1)
	go func() {
		m, err := s.PatchMetadata(context.Background(), "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "archive-during-upload"})
		if err == nil && (!m.Archived || m.Revision != 1) {
			err = errors.New("archive result lost intent or revision")
		}
		archiveDone <- err
	}()
	select {
	case err := <-archiveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow upload blocked archive commit")
	}
	var audits int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM organization_audit WHERE operation_id='archive-during-upload'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("archive audit missing while upload is stalled", audits, err)
	}
	if _, err := w.Write([]byte("low")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapacityRefusalPreservesUnacknowledgedState(t *testing.T) {
	s := openTestStore(t, Options{ReserveBytes: 100, AvailableBytes: func(string) (int64, error) { return 102, nil }})
	src := testSource()
	c := makeChunk(src, 0, []byte("123"))
	if _, err := s.IngestChunk(context.Background(), c, bytes.NewReader([]byte("123"))); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if _, err := s.SourceState(context.Background(), src.SourceID, src.Generation); !errors.Is(err, ErrNotFound) {
		t.Fatal("refused data was acknowledged")
	}
}

func TestIndexRevisionsDedupeFTSAndArchivePersistence(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ctx := context.Background()
	ingest(t, s, src, 0, "first\nsecond\n")
	b := batch(src, 0, 6)
	b.Session.TokensOut = 999999
	b.Session.NativeID = "actual-native"
	b.Events = []Event{{ID: "event-1", DedupeKey: "message-block", Kind: "assistant", Text: "preview", SearchText: "searchable deep historical needle", SourceOffset: 0, SourceLength: 6}}
	b.Usage = []UsageObservation{{ID: "usage-1", Model: "model-a", TokensIn: 100, TokensCache: 20, TokensOut: 30, Kind: "request"}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	archived := true
	m, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "archive-op"})
	if err != nil || !m.Archived || m.Revision != 1 {
		t.Fatalf("archive %+v %v", m, err)
	}
	b.FromOffset = 6
	b.ToOffset = 13
	b.Events[0].ID = "event-duplicate"
	b.Usage[0].TokensIn = 120
	b.Usage[0].TokensOut = 25
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EventCount != 1 || got.TokensIn != 120 || got.TokensOut != 25 || got.TokensCache != 20 || !got.Metadata.Archived || got.NativeID != "actual-native" {
		t.Fatalf("wrong aggregate %+v", got)
	}
	usage, err := s.GetUsage(ctx, "session-1", "", 100)
	if err != nil || len(usage) != 1 || usage[0].TokensOut != 25 {
		t.Fatalf("usage %+v %v", usage, err)
	}
	hits, err := s.Search(ctx, SearchQuery{Text: "needle"})
	if err != nil || len(hits.Events) != 1 || hits.Events[0].Text != "preview" {
		t.Fatalf("full search / bounded preview: %+v %v", hits, err)
	}
	if err = s.CommitIndex(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale parse cursor accepted: %v", err)
	}
	changes, err := s.Changes(ctx, 0, 100)
	if err != nil || len(changes) != 3 || changes[1].Kind != "metadata" {
		t.Fatalf("changes %+v %v", changes, err)
	}
	dir := s.dir
	s.Close()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replayed, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "archive-op"})
	if err != nil || !reflect.DeepEqual(replayed, m) {
		t.Fatalf("idempotent metadata receipt %+v %+v %v", replayed, m, err)
	}
	archived = false
	if _, err = s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "archive-op"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation ID reuse accepted: %v", err)
	}
	if _, err = s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: 0, OperationID: "new-op"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale archive revision accepted: %v", err)
	}
}

func TestGenerationRewritePreservesEvidenceWithoutDoubling(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "old\n")
	b := batch(src, 0, 4)
	b.Events = []Event{{ID: "old-event", Text: "old generation", SourceLength: 4}}
	b.Usage = []UsageObservation{{ID: "old-usage", TokensOut: 50}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	newSource := src
	newSource.Generation = "generation-2"
	ingest(t, s, newSource, 0, "new\n")
	b = batch(newSource, 0, 4)
	b.Events = []Event{{ID: "new-event", Text: "new generation", SourceLength: 4}}
	b.Usage = []UsageObservation{{ID: "new-usage", TokensOut: 10}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, src, 4, "later\n")
	b = batch(src, 4, 10)
	b.Usage = []UsageObservation{{ID: "old-later-usage", TokensOut: 100}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	total, err := s.SessionTotals(ctx, SessionQuery{})
	if err != nil || total.Sessions != 1 || total.TokensOut != 10 {
		t.Fatalf("double/stale generation totals %+v %v", total, err)
	}
	events, err := s.ListEvents(ctx, "session-1", 0, 100)
	if err != nil || len(events.Events) != 1 || events.Events[0].ID != "new-event" {
		t.Fatalf("generations %+v %v", events, err)
	}
	r, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(raw) != "old\nlater\n" {
		t.Fatalf("old raw generation lost %q %v", raw, err)
	}
}

func TestLegacyArchiveImportHasNoFiveThousandRowEviction(t *testing.T) {
	s := openTestStore(t, Options{})
	entries := map[string]json.RawMessage{}
	aliases := map[string]string{}
	for i := range 5001 {
		k := fmt.Sprintf("R:old:claude:%05d", i)
		entries[k] = json.RawMessage(`{"archived":true,"note":"preserved","unknownLegacyField":123}`)
		aliases[k] = fmt.Sprintf("imported-%05d", i)
	}
	if err := s.ImportLegacyMetadata(context.Background(), aliases, entries); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyMetadata(context.Background(), aliases, entries); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"imported-00000", "imported-05000"} {
		m, err := s.GetMetadata(context.Background(), id)
		if err != nil || !m.Archived || m.Note != "preserved" || m.Revision != 1 {
			t.Fatalf("lost legacy state %+v %v", m, err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_metadata`).Scan(&count); err != nil || count != 5001 {
		t.Fatalf("original evidence count %d %v", count, err)
	}
}

func TestSessionAndSourcePagination(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for i := range 7 {
		src := testSource()
		src.SourceID = fmt.Sprintf("source-%d", i)
		ingest(t, s, src, 0, "line\n")
		b := batch(src, 0, 5)
		b.Session.ID = fmt.Sprintf("session-%d", i)
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		p, err := s.ListSessions(ctx, SessionQuery{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range p.Sessions {
			if seen[item.ID] {
				t.Fatal("duplicate page item")
			}
			seen[item.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 7 {
		t.Fatal(seen)
	}
	count := 0
	cursor = ""
	for {
		p, err := s.ListSources(ctx, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		count += len(p)
		if len(p) < 3 {
			break
		}
		cursor = SourceCursor(p[len(p)-1])
	}
	if count != 7 {
		t.Fatal(count)
	}
}

func TestBackupRestoreVerifiesEvidenceAndChangesRecoveryEpoch(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "backed up\n")
	b := batch(src, 0, 10)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, OperationID: "backup-archive"}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "manual-backup")
	m, err := s.Backup(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	if m.BlobCount != 1 || m.RawBytes != 10 {
		t.Fatalf("manifest %+v", m)
	}
	if _, err = VerifyBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, src, 10, "after backup\n")
	restored, err := RestoreBackup(ctx, backup, filepath.Join(t.TempDir(), "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.RecoveryEpoch() == s.RecoveryEpoch() {
		t.Fatal("restore must advertise new recovery epoch")
	}
	st, err := restored.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || st.DurableOffset != 10 {
		t.Fatalf("restore offset %+v %v", st, err)
	}
	meta, err := restored.GetMetadata(ctx, "session-1")
	if err != nil || !meta.Archived {
		t.Fatalf("restored archive %+v %v", meta, err)
	}
	ingest(t, restored, src, 10, "after backup\n")
	c := makeChunk(src, 0, []byte("backed up\n"))
	blob, _ := blobName(backup, c.SHA256)
	if err = os.WriteFile(blob, []byte("corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBackup(ctx, backup); err == nil {
		t.Fatal("corrupted raw backup passed verification")
	}
}
