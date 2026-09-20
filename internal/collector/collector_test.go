package collector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func testCollector(t *testing.T, handler http.HandlerFunc) (*Collector, string) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	root := filepath.Join(t.TempDir(), "explicit-transcripts")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Open(Config{DataDir: filepath.Join(t.TempDir(), "spool"), Roots: []Root{{Path: root, Provider: "codex"}}, HubURL: server.URL, MachineID: "machine-test", Token: "test-token", BytesPerSecond: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	c.freeSpace = func(string) (uint64, uint64, error) { return 90 << 30, 100 << 30, nil }
	t.Cleanup(func() { _ = c.Close() })
	return c, root
}

func writeSource(t *testing.T, root, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func reconcile(t *testing.T, c *Collector) {
	t.Helper()
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func capture(t *testing.T, c *Collector, n int) {
	t.Helper()
	for range n {
		if err := c.CaptureOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func stats(t *testing.T, c *Collector) Status {
	t.Helper()
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func decodeChunk(t *testing.T, r *http.Request) protocol.Chunk {
	t.Helper()
	var ch protocol.Chunk
	b, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-AMC-Chunk"))
	if err != nil {
		t.Error(err)
	}
	if err = json.Unmarshal(b, &ch); err != nil {
		t.Error(err)
	}
	return ch
}
func ack(w http.ResponseWriter, ch protocol.Chunk) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(protocol.Receipt{ReceiptID: ch.SHA256, SourceID: ch.Source.SourceID, Generation: ch.Source.Generation, DurableOffset: ch.Offset + ch.Length, RecoveryEpoch: "epoch-1"})
}

func TestCaptureArchiveRootsAndDurableMachineIdentity(t *testing.T) {
	c, root := testCollector(t, nil)
	writeSource(t, root, "sessions/a.jsonl", []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"live-session\"}}\n"))
	writeSource(t, root, "archived_sessions/b.jsonl", []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"archived-session\"}}\n"))
	writeSource(t, root, "ignore.txt", []byte("not a transcript"))
	reconcile(t, c)
	capture(t, c, 2)
	s := stats(t, c)
	if s.Sources != 2 || s.CapturedBytes == 0 || s.BacklogBytes != s.CapturedBytes {
		t.Fatalf("unexpected status %+v", s)
	}
	cfg := c.cfg
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MachineID = ""
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.cfg.MachineID != "machine-test" {
		t.Fatal("durable identity changed")
	}
	if got := stats(t, reopened); got.CapturedBytes != s.CapturedBytes || got.BacklogBytes != s.BacklogBytes {
		t.Fatalf("progress did not persist: %+v", got)
	}
}

func TestCaptureFairnessAndSpoolBackpressure(t *testing.T) {
	c, root := testCollector(t, nil)
	c.cfg.SpoolMaxBytes = 2 * protocol.MaxChunkBytes
	writeSource(t, root, "a.jsonl", bytes.Repeat([]byte("a"), 3*protocol.MaxChunkBytes))
	writeSource(t, root, "b.jsonl", bytes.Repeat([]byte("b"), 3*protocol.MaxChunkBytes))
	reconcile(t, c)
	capture(t, c, 2)
	var distinct int
	if err := c.db.QueryRow("SELECT count(DISTINCT source_id) FROM chunks").Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 2 {
		t.Fatal("one source monopolized capture")
	}
	if err := c.CaptureOnce(context.Background()); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("expected spool backpressure, got %v", err)
	}
	s := stats(t, c)
	if s.BacklogBytes != 2*protocol.MaxChunkBytes || s.SpoolBytes != s.BacklogBytes {
		t.Fatalf("unacknowledged history was reclaimed %+v", s)
	}
}

func TestRenameKeepsSourceAndRewriteKeepsPriorEvidence(t *testing.T) {
	c, root := testCollector(t, nil)
	raw := bytes.Repeat([]byte("a"), 4*protocol.MaxChunkBytes)
	path := writeSource(t, root, "a.jsonl", raw)
	reconcile(t, c)
	capture(t, c, 4)
	var beforeID, beforeGen string
	if err := c.db.QueryRow("SELECT id,current_generation FROM sources").Scan(&beforeID, &beforeGen); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(root, "renamed.jsonl")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c)
	var afterID, afterGen string
	if err := c.db.QueryRow("SELECT id,current_generation FROM sources").Scan(&afterID, &afterGen); err != nil {
		t.Fatal(err)
	}
	if beforeID != afterID || beforeGen != afterGen {
		t.Fatal("rename changed source identity")
	}
	f, err := os.OpenFile(renamed, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("changed-middle"), protocol.MaxChunkBytes+500); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reconcile(t, c)
	if err = c.db.QueryRow("SELECT current_generation FROM sources").Scan(&afterGen); err != nil {
		t.Fatal(err)
	}
	if beforeGen == afterGen {
		t.Fatal("middle rewrite did not create a generation")
	}
	var generationSequence int64
	if err = c.db.QueryRow("SELECT generation_sequence FROM generations WHERE generation=?", afterGen).Scan(&generationSequence); err != nil {
		t.Fatal(err)
	}
	if generationSequence != 2 {
		t.Fatalf("rewrite ordinal=%d, want2", generationSequence)
	}
	var oldChunks int
	if err = c.db.QueryRow("SELECT count(*) FROM chunks WHERE generation=?", beforeGen).Scan(&oldChunks); err != nil {
		t.Fatal(err)
	}
	if oldChunks != 4 {
		t.Fatal("rewrite discarded prior evidence")
	}
	capture(t, c, 1)
	if s := stats(t, c); s.CapturedBytes != 5*protocol.MaxChunkBytes {
		t.Fatalf("unexpected preserved evidence size %+v", s)
	}
}

func TestLostAcknowledgementRetriesSameChunkAfterRestart(t *testing.T) {
	var calls atomic.Int32
	var unique sync.Map
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		ch := decodeChunk(t, r)
		body, _ := io.ReadAll(r.Body)
		if int64(len(body)) != ch.Length {
			t.Error("wrong length")
		}
		unique.Store(ch.Source.SourceID+ch.Source.Generation+ch.SHA256, true)
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		ack(w, ch)
	})
	writeSource(t, root, "a.jsonl", []byte("captured evidence\n"))
	reconcile(t, c)
	capture(t, c, 1)
	if worked, err := c.UploadOnce(context.Background()); !worked || err == nil {
		t.Fatal("first lost acknowledgement should retry")
	}
	if s := stats(t, c); s.UploadedBytes != 0 || s.BacklogBytes == 0 {
		t.Fatal("unacknowledged chunk was marked uploaded")
	}
	cfg := c.cfg
	c.Close()
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.freeSpace = c.freeSpace
	if _, err = reopened.db.Exec(`UPDATE settings SET value=json_set(value,'$.until',0) WHERE key='upload_pause'`); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.db.Exec("UPDATE chunks SET next_attempt=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.UploadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := 0
	unique.Range(func(_, _ any) bool { count++; return true })
	if count != 1 || calls.Load() != 2 {
		t.Fatal("retry changed chunk identity")
	}
	if s := stats(t, reopened); s.BacklogBytes != 0 || s.UploadedBytes != s.CapturedBytes {
		t.Fatalf("receipt not committed %+v", s)
	}
}

func TestAcknowledgedCopiesReclaimedBeforeUnacknowledged(t *testing.T) {
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		ch := decodeChunk(t, r)
		_, _ = io.Copy(io.Discard, r.Body)
		ack(w, ch)
	})
	c.cfg.SpoolMaxBytes = protocol.MaxChunkBytes
	writeSource(t, root, "a.jsonl", bytes.Repeat([]byte("a"), 2*protocol.MaxChunkBytes))
	reconcile(t, c)
	capture(t, c, 1)
	if _, err := c.UploadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := stats(t, c); s.SpoolBytes != protocol.MaxChunkBytes {
		t.Fatal("acknowledged chunk should remain during grace period")
	}
	capture(t, c, 1)
	s := stats(t, c)
	if s.SpoolBytes != protocol.MaxChunkBytes || s.CapturedBytes != 2*protocol.MaxChunkBytes || s.BacklogBytes != protocol.MaxChunkBytes {
		t.Fatalf("wrong reclamation %+v", s)
	}
}

func TestSourceDisappearanceRecordsGapWithoutDeletingHistory(t *testing.T) {
	c, root := testCollector(t, nil)
	path := writeSource(t, root, "a.jsonl", bytes.Repeat([]byte("x"), 2*protocol.MaxChunkBytes))
	reconcile(t, c)
	capture(t, c, 1)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c)
	s := stats(t, c)
	if s.Gaps != 1 || s.CapturedBytes != protocol.MaxChunkBytes || s.BacklogBytes != protocol.MaxChunkBytes {
		t.Fatalf("missing source not represented honestly %+v", s)
	}
}

func TestCorruptSpoolCannotUpload(t *testing.T) {
	var calls atomic.Int32
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	writeSource(t, root, "a.jsonl", []byte("original\n"))
	reconcile(t, c)
	capture(t, c, 1)
	var name string
	if err := c.db.QueryRow("SELECT filename FROM chunks").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.cfg.DataDir, "chunks", name), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("corrupted data reached hub")
	}
}

func TestAuthenticationFailureHasDurableSlowProbe(t *testing.T) {
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	writeSource(t, root, "a.jsonl", []byte("history\n"))
	reconcile(t, c)
	capture(t, c, 1)
	if _, err := c.UploadOnce(context.Background()); err == nil {
		t.Fatal("expected authentication failure")
	}
	var next int64
	if err := c.db.QueryRow("SELECT next_attempt FROM chunks").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if time.Until(time.Unix(0, next)) < 4*time.Minute {
		t.Fatal("authentication failure was scheduled as a tight retry")
	}
	if s := stats(t, c); s.ConnectionState != "blocked_auth" || s.BacklogBytes == 0 {
		t.Fatalf("wrong authentication status %+v", s)
	}
}

func TestHeartbeatContinuesWhileUploadIsHung(t *testing.T) {
	var heartbeats atomic.Int32
	var uploads atomic.Int32
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/ingest/heartbeat":
			heartbeats.Add(1)
			w.WriteHeader(204)
		case "/v2/ingest/chunks":
			uploads.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		default:
			w.WriteHeader(204)
		}
	})
	c.cfg.HeartbeatInterval = 40 * time.Millisecond
	c.cfg.RequestTimeout = 500 * time.Millisecond
	c.cfg.ProgressTimeout = 120 * time.Millisecond
	c.cfg.Debounce = 20 * time.Millisecond
	writeSource(t, root, "a.jsonl", []byte("history\n"))
	reconcile(t, c)
	capture(t, c, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected run result %v", err)
	}
	if uploads.Load() < 1 || heartbeats.Load() < 5 {
		t.Fatalf("heartbeat delayed by transfer: uploads=%d heartbeats=%d", uploads.Load(), heartbeats.Load())
	}
}

func TestHubRestoreRequeuesAndRebuildsReclaimedPayload(t *testing.T) {
	var restored atomic.Bool
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/ingest/reconcile" {
			var src protocol.Source
			_ = json.NewDecoder(r.Body).Decode(&src)
			_ = json.NewEncoder(w).Encode(protocol.Receipt{SourceID: src.SourceID, Generation: src.Generation, DurableOffset: 0, RecoveryEpoch: "restored"})
			restored.Store(true)
			return
		}
		ch := decodeChunk(t, r)
		_, _ = io.Copy(io.Discard, r.Body)
		ack(w, ch)
	})
	writeSource(t, root, "a.jsonl", []byte("recoverable history\n"))
	reconcile(t, c)
	capture(t, c, 1)
	if _, err := c.UploadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec("UPDATE chunks SET ack_at=?", time.Now().Add(-25*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := c.reclaim(context.Background(), false, 0); err != nil {
		t.Fatal(err)
	}
	if s := stats(t, c); s.SpoolBytes != 0 {
		t.Fatal("test did not reclaim spool copy")
	}
	if err := c.ReconcileRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !restored.Load() {
		t.Fatal("hub reconciliation was skipped")
	}
	if s := stats(t, c); s.BacklogBytes == 0 || s.UploadedBytes != 0 {
		t.Fatalf("restore did not requeue %+v", s)
	}
	if _, err := c.UploadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := stats(t, c); s.BacklogBytes != 0 || s.Gaps != 0 {
		t.Fatalf("restore reconciliation failed %+v", s)
	}
}

func TestDiskReservePausesWithoutCapture(t *testing.T) {
	c, root := testCollector(t, nil)
	c.freeSpace = func(string) (uint64, uint64, error) { return 4 << 30, 100 << 30, nil }
	writeSource(t, root, "a.jsonl", []byte("history"))
	reconcile(t, c)
	if err := c.CaptureOnce(context.Background()); !errors.Is(err, ErrDiskReserve) {
		t.Fatalf("expected reserve error, got %v", err)
	}
	if s := stats(t, c); s.CapturedBytes != 0 {
		t.Fatal("capture continued through disk reserve")
	}
}

func TestRecoveryIntentSurvivesRestart(t *testing.T) {
	c, _ := testCollector(t, nil)
	c.noteEpoch(context.Background(), "restored-epoch")
	var pending string
	if err := c.db.QueryRow("SELECT value FROM settings WHERE key='recovery_pending'").Scan(&pending); err != nil || pending != "restored-epoch" {
		t.Fatalf("recovery intent not durable: %q %v", pending, err)
	}
	cfg := c.cfg
	c.Close()
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	select {
	case <-reopened.recover:
	default:
		t.Fatal("restarted collector did not resume pending recovery")
	}
}

func TestUploadFairnessRotatesSourcesNotIndividualChunkAttempts(t *testing.T) {
	var received []string
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		ch := decodeChunk(t, r)
		_, _ = io.Copy(io.Discard, r.Body)
		received = append(received, ch.Source.SourceID)
		ack(w, ch)
	})
	writeSource(t, root, "a.jsonl", bytes.Repeat([]byte("a"), 2*protocol.MaxChunkBytes))
	writeSource(t, root, "b.jsonl", bytes.Repeat([]byte("b"), 2*protocol.MaxChunkBytes))
	reconcile(t, c)
	capture(t, c, 4)
	for range 2 {
		if _, err := c.UploadOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 2 || received[0] == received[1] {
		t.Fatalf("one transcript monopolized upload: %v", received)
	}
}
