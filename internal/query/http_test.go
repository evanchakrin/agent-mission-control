package query

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestRhythmRouteHasFixedBucketsAndValidatesTimezone(t *testing.T) {
	_, mux := queryFixture(t)
	value := response(t, mux, "/api/v2/analytics/rhythm?timezone=America%2FNew_York&archived=false", 200)
	if value["sessions"] != float64(1) || len(value["hours"].([]any)) != 24 || len(value["weekdays"].([]any)) != 7 {
		t.Fatal(value)
	}
	for _, query := range []string{"", "timezone=Local", "timezone=UTC&limit=100", "timezone=UTC&cursor=next"} {
		response(t, mux, "/api/v2/analytics/rhythm?"+query, 400)
	}
}

func TestToolSpanRouteBoundsAndPinsContinuation(t *testing.T) {
	_, mux := queryFixture(t)
	value := response(t, mux, "/api/v2/sessions/session/tool-spans", 200)
	if value["snapshot"] == "" || value["spans"] == nil {
		t.Fatal(value)
	}
	for _, query := range []string{"limit=101", "limit=0", "after=-1", "after=1", "after=oops", "limit=oops"} {
		response(t, mux, "/api/v2/sessions/session/tool-spans?"+query, 400)
	}
	response(t, mux, "/api/v2/sessions/session/tool-spans?snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/missing/tool-spans", 404)
}

func queryFixture(t *testing.T) (*store.Store, *http.ServeMux) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte("source evidence\n")
	hash := sha256.Sum256(body)
	source := protocol.Source{SourceID: "source", MachineID: "machine", Provider: "codex", NativeID: "native", Generation: "g1", GenerationSequence: 1, Size: int64(len(body))}
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: source, Length: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	err = s.CommitIndex(ctx, store.IndexBatch{SourceID: source.SourceID, Generation: source.Generation, ToOffset: int64(len(body)),
		Session: store.Session{ID: "session", Title: "Full history", Project: "amc", LastActivity: at},
		Events:  []store.Event{{ID: "event1", AgentID: "main", Kind: "tool-call", Timestamp: at, SourceLength: 1}, {ID: "event2", AgentID: "child", Kind: "tool-result", Timestamp: at.Add(time.Second), Data: json.RawMessage(`{"error":true}`), SourceLength: 1}},
		Usage:   []store.UsageObservation{{ID: "usage1", AgentID: "main", Model: "known", Timestamp: at, TokensIn: 5}, {ID: "usage2", AgentID: "child", Timestamp: at, TokensOut: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, s)
	return s, mux
}

func response(t *testing.T, mux *http.ServeMux, url string, status int) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != status {
		t.Fatalf("%s status=%d body=%s", url, w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("history response may be cached")
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUsageAndReplaySnapshotContract(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/sessions/session/usage?limit=1", 200)
	token, ok := page["snapshot"].(string)
	if !ok || len(token) != 64 {
		t.Fatal(page)
	}
	response(t, mux, "/api/v2/sessions/session/usage?limit=1&cursor=usage1", 400)
	response(t, mux, "/api/v2/sessions/session/usage?limit=1&cursor=usage1&snapshot="+token, 200)
	for _, path := range []string{"/api/v2/sessions/session/usage?snapshot=stale", "/api/v2/sessions/session/events/around?sequence=1&snapshot=stale"} {
		value := response(t, mux, path, 409)
		if value["code"] != "history_changed" {
			t.Fatal(value)
		}
	}
}

func TestReadOnlyQueryRoutesUseBoundedCompleteHistory(t *testing.T) {
	_, mux := queryFixture(t)
	totals := response(t, mux, "/api/v2/analytics/totals?project=amc&limit=1", 200)
	if totals["sessions"] != float64(1) || totals["tokensIn"] != float64(5) || totals["tokensOut"] != float64(3) || totals["costEstimate"] != nil {
		t.Fatalf("misleading totals: %+v", totals)
	}
	for _, path := range []string{"/api/v2/catalog/projects", "/api/v2/catalog/machines", "/api/v2/analytics/usage/provider", "/api/v2/analytics/usage/day", "/api/v2/sessions/session/models", "/api/v2/sessions/session/lineage"} {
		response(t, mux, path, 200)
	}
	stats := response(t, mux, "/api/v2/sessions/session/stats", 200)
	if stats["agentCount"] != float64(2) || stats["toolCalls"] != float64(1) || stats["errors"] != float64(1) {
		t.Fatalf("wrong statistics: %+v", stats)
	}
	first := response(t, mux, "/api/v2/sessions/session/agents?limit=1", 200)
	if len(first["agents"].([]any)) != 1 || first["nextCursor"] == "" {
		t.Fatal("first agent page unbounded or lacks continuation")
	}
	second := response(t, mux, "/api/v2/sessions/session/agents?limit=1&cursor="+first["nextCursor"].(string), 200)
	if len(second["agents"].([]any)) != 1 || second["nextCursor"] != nil {
		t.Fatal("agent continuation incomplete")
	}
	window := response(t, mux, "/api/v2/sessions/session/events/around?sequence=1&before=0&after=0", 200)
	if len(window["events"].([]any)) != 1 || window["afterSequence"] != float64(2) {
		t.Fatalf("incorrect zero-width replay anchor: %+v", window)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v2/analytics/totals", nil))
	if w.Code != 405 {
		t.Fatal("read registrar accepts mutations")
	}
}

func TestQueryHTTPValidation(t *testing.T) {
	_, mux := queryFixture(t)
	for _, url := range []string{
		"/api/v2/analytics/totals?limit=0", "/api/v2/analytics/totals?archived=wat", "/api/v2/analytics/totals?from=no-date",
		"/api/v2/analytics/totals?from=2026-09-05&to=2026-09-04", "/api/v2/catalog/projects?project=amc&unassignedProject=true",
		"/api/v2/analytics/usage/injection", "/api/v2/sessions/session/agents?cursor=invalid", "/api/v2/sessions/session/events/around?sequence=NaN",
		"/api/v2/sessions/session/events/around?sequence=1&before=-1",
	} {
		response(t, mux, url, 400)
	}
	large := response(t, mux, "/api/v2/sessions/session/events/around?sequence=1&before=999999&after=999999", 200)
	if len(large["events"].([]any)) > 499 {
		t.Fatal("replay window exceeded bounded page")
	}
	response(t, mux, "/api/v2/sessions/missing/stats", 404)
	response(t, mux, "/api/v2/sessions/session/events/around?sequence=999", 404)
}

func TestCostFlowRoutePinsContinuation(t *testing.T) {
	_, mux := queryFixture(t)
	first := response(t, mux, "/api/v2/sessions/session/cost-flow?limit=1", 200)
	if first["session"].(map[string]any)["id"] != "session" || len(first["agents"].([]any)) != 1 {
		t.Fatal(first)
	}
	base := "/api/v2/sessions/session/cost-flow?limit=1&cursor=" + first["nextCursor"].(string)
	response(t, mux, base, 400)
	response(t, mux, base+"&snapshot="+first["snapshot"].(string), 200)
	response(t, mux, base+"&snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/session/cost-flow?limit=101", 400)
	response(t, mux, "/api/v2/sessions/session/cost-flow", 200)
}

func TestParseSessionQueryPreservesWhitelistedInputsAndHalfOpenDates(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?q=needle&machineId=m&provider=codex&project=p&archived=false&limit=100000&sort=tokens&direction=asc&pinnedFirst=true&from=2026-09-04&to=2026-09-05", nil)
	q, err := ParseSessionQuery(r)
	if err != nil {
		t.Fatal(err)
	}
	if q.Text != "needle" || q.MachineID != "m" || q.Project != "p" || q.Sort != "tokens" || q.Direction != "asc" || !q.PinnedFirst || q.Archived == nil || *q.Archived || q.To.Sub(*q.From) != 24*time.Hour {
		t.Fatalf("lost filters: %+v", q)
	}
	// Store page methods clamp this to500; the HTTP parser doesn't allocate it.
	if q.Limit != 100000 {
		t.Fatal("unexpected parse result")
	}
	if _, err = ParseSessionQuery(httptest.NewRequest(http.MethodGet, "/?pinnedFirst=maybe", nil)); err == nil {
		t.Fatal("invalid pin ordering accepted")
	}
}
