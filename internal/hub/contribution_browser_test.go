package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	assets "github.com/evanchakrin/agent-mission-control/public"
)

// Explicit, short-lived UI fixture: two sub-kilobyte sources in t.TempDir.
// No live config, transcript discovery, collector, or persistent service.
func TestContributionBrowserFixture(t *testing.T) {
	if os.Getenv("AMC_CONTRIBUTION_BROWSER_FIXTURE") != "1" {
		t.Skip("interactive fixture not requested")
	}
	ctx := context.Background()
	forkMode := os.Getenv("AMC_CONTRIBUTION_BROWSER_FORK") == "1"
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	var usage []store.UsageObservation
	for _, sourceID := range []string{"fixture-original", "fixture-copy"} {
		raw := []byte(`{"timestamp":"2026-09-01T01:02:00Z","type":"event_msg","payload":{"type":"user_message","message":"Contribution browser fixture"}}` + "\n" + `{"timestamp":"2026-09-01T01:02:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":10}}}}` + "\n")
		if sourceID == "fixture-copy" {
			raw = append([]byte("\n"), raw...)
		}
		if forkMode {
			header := `{"type":"session_meta","payload":{"id":"fixture-parent"}}` + "\n"
			if sourceID == "fixture-copy" {
				header = `{"type":"session_meta","payload":{"id":"fixture-child","forked_from_id":"fixture-parent"}}` + "\n"
			}
			raw = append([]byte(header), raw...)
		}
		src := protocol.Source{MachineID: "browser-fixture", SourceID: sourceID, Generation: "g", GenerationSequence: 1, Provider: "codex", NativeID: "fixture-thread", Size: int64(len(raw))}
		hash := sha256.Sum256(raw)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err = (&indexer.Indexer{Store: s}).Once(ctx, sourceID, "g"); err != nil {
			t.Fatal(err)
		}
		id := parser.SessionID(src)
		rows, err := s.GetUsage(ctx, id, "", 10)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		ids = append(ids, id)
		usage = append(usage, rows[0])
	}
	if !forkMode {
		proof, err := accounting.StageDuplicateUsage(ctx, s, ids[0], ids[1], usage[0], usage[1])
		if err != nil {
			t.Fatal(err)
		}
		if err = accounting.SelectDuplicateUsage(ctx, s, proof); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, strings.Repeat("fixture-only-", 3), "browser-fixture")
	h.HubID = "browser-fixture-" + s.RecoveryEpoch()
	ownerHandler := h.OwnerHandler()
	mux := http.NewServeMux()
	assets.RegisterPreview(mux)
	mux.HandleFunc("GET /api/v2/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"hubId": h.HubID, "recoveryEpoch": s.RecoveryEpoch(), "version": "DISPOSABLE BROWSER FIXTURE", "csrf": "fixture-read-only"})
	})
	mux.Handle("/api/v2/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !(forkMode && r.Method == http.MethodPost && r.URL.Path == "/api/v2/sessions/"+ids[1]+"/reconcile-inheritance") {
			http.Error(w, "read-only fixture", 405)
			return
		}
		ownerHandler.ServeHTTP(w, r)
	}))
	stop := make(chan struct{})
	var once sync.Once
	mux.HandleFunc("GET /__fixture/stop", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(stop) }); w.WriteHeader(204) })
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Logf("Disposable contribution UI: %s/preview/", server.URL)
	select {
	case <-stop:
	case <-time.After(10 * time.Minute):
		t.Log("fixture observation window ended")
	}
}
