package legacy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

const testToken = "legacy-original-token"

func fixture(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "store"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h, err := New(Config{Store: s, StateDir: filepath.Join(root, "tmp"), Token: testToken, ReserveBytes: 1, AvailableBytes: func(string) (int64, error) { return 1 << 40, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return h, s
}
func request(h http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("x-relay-token", testToken)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}
func appendHeaders(path string, off, length int64) map[string]string {
	return map[string]string{"x-relay-machine": "erp", "x-relay-path": url.PathEscape(path), "x-relay-offset": strconv.FormatInt(off, 10), "x-relay-bytes": strconv.FormatInt(length, 10)}
}
func anchor(body []byte, off int) string {
	start := off - 64
	if start < 0 {
		start = 0
	}
	h := sha1.Sum(body[start:off])
	return hex.EncodeToString(h[:])
}

func TestBootAuthenticationAndDurableHeartbeat(t *testing.T) {
	h, s := fixture(t)
	w := request(h, "GET", "/v1/boot", nil, map[string]string{"x-relay-machine": "erp", "x-relay-version": "7.34.1"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	b := decode(t, w)
	if b["archiveCreateOnly"] != true {
		t.Fatal("create-only recovery capability missing", b)
	}
	if b["boot"] != s.RecoveryEpoch() {
		t.Fatal(b)
	}
	machines, err := s.ListMachines(context.Background())
	if err != nil || len(machines) != 1 || machines[0].Heartbeat.Version != "7.34.1" {
		t.Fatalf("heartbeat %+v %v", machines, err)
	}
	w = request(h, "GET", "/v1/boot", nil, map[string]string{"x-relay-token": "wrong"})
	if w.Code != 401 {
		t.Fatal("unauthenticated boot accepted")
	}
}

func TestAppendLostACKReannounceAnchorAndRewrite(t *testing.T) {
	h, s := fixture(t)
	ctx := context.Background()
	path := "claude/project/main.jsonl"
	body := []byte("{\"type\":\"user\"}\n")
	head := appendHeaders(path, 0, int64(len(body)))
	head["x-relay-meta"] = url.PathEscape(`{"file":"project/main.jsonl","meta":{"session":"main"}}`)
	w := request(h, "POST", "/v1/relay/append", body, head)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sources, err := s.ListSources(ctx, "", 10)
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources %+v %v", sources, err)
	}
	original := sources[0].Source
	w = request(h, "POST", "/v1/relay/append", body, head)
	if w.Code != 409 || int(decode(t, w)["size"].(float64)) != len(body) {
		t.Fatal("lost ACK did not resume", w.Code, w.Body.String())
	}
	head = appendHeaders(path, int64(len(body)), 0)
	head["x-relay-anchor"] = anchor(body, len(body))
	w = request(h, "POST", "/v1/relay/append", nil, head)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	head["x-relay-anchor"] = strings.Repeat("0", 40)
	w = request(h, "POST", "/v1/relay/append", nil, head)
	if w.Code != 409 || decode(t, w)["size"].(float64) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(h, "POST", "/v1/relay/append", []byte("new\n"), appendHeaders(path, 0, 4))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	current, err := s.CurrentSource(ctx, original.SourceID)
	if err != nil || current.Source.GenerationSequence != 2 || current.Source.Generation == original.Generation {
		t.Fatalf("generation %+v %v", current, err)
	}
	r, err := s.OpenSource(ctx, original.SourceID, original.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(old, body) {
		t.Fatal("anchor recovery destroyed old raw")
	}
}

func TestAppendAdoptsArchiveImportedBeforeSource(t *testing.T) {
	h, s := fixture(t)
	ctx := context.Background()
	key := legacyKey("erp", "claude", "main")
	if err := s.ImportLegacyMetadata(ctx, nil, map[string]json.RawMessage{key: json.RawMessage(`{"archived":true,"note":"Keep archived"}`)}); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\"type\":\"user\"}\n")
	path := "claude/project/main.jsonl"
	w := request(h, "POST", "/v1/relay/append", body, appendHeaders(path, 0, int64(len(body))))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sources, err := s.ListSources(ctx, "", 10)
	if err != nil || len(sources) != 1 {
		t.Fatal(sources, err)
	}
	id := parser.SessionID(sources[0].Source)
	alias, err := s.ResolveAlias(ctx, key)
	if err != nil || alias != id {
		t.Fatal(alias, id, err)
	}
	m, err := s.GetMetadata(ctx, id)
	if err != nil || !m.Archived || m.Note != "Keep archived" {
		t.Fatal(m, err)
	}
	// Reannouncement after a lost ACK must preserve the adopted organization.
	head := appendHeaders(path, int64(len(body)), 0)
	head["x-relay-anchor"] = anchor(body, len(body))
	w = request(h, "POST", "/v1/relay/append", nil, head)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, err := s.GetMetadata(ctx, id)
	if err != nil || after.Revision != m.Revision || !after.Archived {
		t.Fatal(after, err)
	}
}

func TestInterruptedAppendResumesCommittedChunksAfterRestart(t *testing.T) {
	h, s := fixture(t)
	path := "claude/project/large.jsonl"
	first := bytes.Repeat([]byte("x"), protocol.MaxChunkBytes)
	head := appendHeaders(path, 0, int64(len(first)+5))
	w := request(h, "POST", "/v1/relay/append", first, head)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	restarted, err := New(h.config)
	if err != nil {
		t.Fatal(err)
	}
	w = request(restarted, "POST", "/v1/relay/append", append(first, []byte("tail\n")...), head)
	if w.Code != 409 || int(decode(t, w)["size"].(float64)) != len(first) {
		t.Fatal(w.Code, w.Body.String())
	}
	head = appendHeaders(path, int64(len(first)), 5)
	head["x-relay-anchor"] = anchor(first, len(first))
	w = request(restarted, "POST", "/v1/relay/append", []byte("tail\n"), head)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	manifest, err := s.LegacyManifest(context.Background(), "erp")
	if err != nil || manifest[path] != int64(len(first)+5) {
		t.Fatalf("manifest %+v %v", manifest, err)
	}
}

func TestArchiveTransportsManifestAndDeltaOwnership(t *testing.T) {
	h, _ := fixture(t)
	headers := map[string]string{"x-archive-machine": "erp", "x-archive-path": url.PathEscape("claude/project/archive.jsonl")}
	w := request(h, "POST", "/v1/archive/raw", []byte("one\n"), headers)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(h, "POST", "/v1/archive/raw", []byte("two\n"), headers)
	if w.Code != 200 {
		t.Fatal("raw-only snapshot wrongly treated as delta-owned", w.Code, w.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"machine": "erp", "relPath": "codex/rollout-other.jsonl", "data": base64.StdEncoding.EncodeToString([]byte("codex\n"))})
	w = request(h, "POST", "/v1/archive", payload, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(h, "GET", "/v1/archive/manifest?machine=erp", nil, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	manifest := decode(t, w)["manifest"].(map[string]any)
	if manifest["claude/project/archive.jsonl"] != float64(4) || manifest["codex/rollout-other.jsonl"] != float64(6) {
		t.Fatal(manifest)
	}
	w = request(h, "POST", "/v1/relay/append", []byte("live\n"), appendHeaders("claude/project/live.jsonl", 0, 5))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	headers["x-archive-path"] = url.PathEscape("claude/project/live.jsonl")
	w = request(h, "POST", "/v1/archive/raw", []byte("stale\n"), headers)
	if w.Code != 409 {
		t.Fatal("snapshot overwrote live delta mirror", w.Code)
	}
}

func TestParsedSnapshotTotalsReplaceAndArchivesPersist(t *testing.T) {
	h, s := fixture(t)
	ctx := context.Background()
	body := []byte(`{"machine":"erp","file":"project/parsed.jsonl","meta":{"session":"parsed","title":"Snapshot"},"result":{"agents":[{"id":"main","model":"test-model","inTokens":100,"outTokens":20}],"events":[{"ts":"2026-09-04T00:00:00Z","agent":"main","kind":"assistant-text","text":"preview","full":"deep searchable complete phrase"}]}}`)
	w := request(h, "POST", "/v1/relay", body, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	page, err := s.ListSessions(ctx, store.SessionQuery{})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("sessions %+v %v", page, err)
	}
	sess := page.Sessions[0]
	if sess.TokensOut != 20 || sess.TokensIn != 100 || sess.Completeness != "legacy-summary-only" {
		t.Fatalf("totals %+v", sess)
	}
	archived := true
	if _, err = s.PatchMetadata(ctx, sess.ID, store.MetadataPatch{Archived: &archived, OperationID: "archive-v1"}); err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`"outTokens":20`), []byte(`"outTokens":30`), 1)
	w = request(h, "POST", "/v1/relay", body, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sess, err = s.GetSession(ctx, sess.ID)
	if err != nil || sess.TokensOut != 30 || !sess.Metadata.Archived {
		t.Fatalf("snapshot or metadata doubled %+v %v", sess, err)
	}
	hits, err := s.Search(ctx, store.SearchQuery{Text: "searchable"})
	if err != nil || len(hits.Events) != 1 {
		t.Fatalf("search %+v %v", hits, err)
	}
}

func TestParsedSnapshotRecoversRawBeforeIndexAndDedupesACK(t *testing.T) {
	h, s := fixture(t)
	ctx := context.Background()
	body := []byte(`{"machine":"erp","file":"project/parsed.jsonl","meta":{"session":"parsed"},"result":{"agents":[{"id":"main","outTokens":20}],"events":[]}}`)
	src, _, err := h.resolve(ctx, "erp", "claude/project/parsed.jsonl", "claude", "parsed")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if err = s.RememberLegacySnapshot(ctx, src.SourceID, src.Generation, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if err = s.SetExternalIndex(ctx, src.SourceID, true); err != nil {
		t.Fatal(err)
	}
	src.Size = int64(len(body))
	if err = h.storeStream(ctx, src, 0, src.Size, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(h.config)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := request(restarted, "POST", "/v1/relay", body, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	st, err := s.CurrentSource(ctx, src.SourceID)
	if err != nil || st.Source.Generation != src.Generation || st.IndexedOffset != src.Size {
		t.Fatalf("snapshot retry rotated or stranded generation %+v %v", st, err)
	}
	total, err := s.SessionTotals(ctx, store.SessionQuery{})
	if err != nil || total.TokensOut != 20 {
		t.Fatalf("duplicate snapshot accounting %+v %v", total, err)
	}
}

func traceBody(output int) []byte {
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"agent-service"}}]},"scopeSpans":[{"spans":[{"traceId":"trace-1","spanId":"span-1","name":"chat","startTimeUnixNano":"1788480000000000000","endTimeUnixNano":"1788480001000000000","attributes":[{"key":"gen_ai.request.model","value":{"stringValue":"model"}},{"key":"gen_ai.usage.input_tokens","value":{"intValue":"100"}},{"key":"gen_ai.usage.output_tokens","value":{"intValue":"` + strconv.Itoa(output) + `"}},{"key":"gen_ai.completion","value":{"stringValue":"telemetry full text"}}]}]}]}]}`)
}
func TestOTLPRequestAndSpanReplayAreIdempotent(t *testing.T) {
	h, s := fixture(t)
	body := traceBody(10)
	for range 2 {
		w := request(h, "POST", "/v1/traces", body, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := request(h, "POST", "/v1/traces", traceBody(12), nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	page, err := s.ListSessions(context.Background(), store.SessionQuery{})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("service grouping %+v %v", page, err)
	}
	if page.Sessions[0].TokensOut != 12 || page.Sessions[0].EventCount != 1 {
		t.Fatalf("replayed span doubled %+v", page.Sessions[0])
	}
	st, err := s.CurrentSource(context.Background(), page.Sessions[0].SourceID)
	if err != nil || st.IndexedOffset != st.DurableOffset || !st.ExternalIndex {
		t.Fatalf("direct index checkpoint %+v %v", st, err)
	}
}

func TestOTLPRecoversRawCommittedBeforeIndex(t *testing.T) {
	h, s := fixture(t)
	ctx := context.Background()
	body := traceBody(10)
	service := "agent-service"
	path := "otel/" + parser.ID(service) + ".jsonl"
	src, _, err := h.resolve(ctx, "legacy-otel", path, "otel", service)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetExternalIndex(ctx, src.SourceID, true); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	frame, err := s.ReserveLegacyRequest(ctx, store.LegacyRequest{SourceID: src.SourceID, Generation: src.Generation, Hash: hex.EncodeToString(hash[:]), Length: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	src.Size = int64(len(body))
	if err = h.storeStream(ctx, src, 0, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(h.config)
	if err != nil {
		t.Fatal(err)
	}
	w := request(restarted, "POST", "/v1/traces", body, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	st, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || st.DurableOffset != frame.Length || st.IndexedOffset != frame.Length {
		t.Fatalf("raw-before-index recovery %+v %v", st, err)
	}
}

func TestMalformedPathsAndConcurrentLargeJSONAdmission(t *testing.T) {
	h, _ := fixture(t)
	for _, path := range []string{"../outside.jsonl", "claude/../../outside.jsonl", "C:/escape.jsonl", "claude/file.txt", "claude/\x00.jsonl"} {
		w := request(h, "POST", "/v1/relay/append", nil, appendHeaders(path, 0, 0))
		if w.Code != 400 {
			t.Fatalf("unsafe path %q: %d", path, w.Code)
		}
	}
	h.largeJSON <- struct{}{}
	w := request(h, "POST", "/v1/relay", []byte(`{}`), nil)
	if w.Code != 503 {
		t.Fatal("large JSON queue not bounded")
	}
	start := time.Now()
	w = request(h, "GET", "/v1/boot", nil, nil)
	if w.Code != 200 || time.Since(start) > time.Second {
		t.Fatal("heartbeat blocked by heavy ingestion")
	}
	<-h.largeJSON
}
