package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testToken = "this-is-only-an-isolated-test-token"

func TestIngestionOwnerBoundaryAndDurableArchive(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	network := httptest.NewServer(h.IngestionHandler())
	defer network.Close()
	owner := httptest.NewServer(h.OwnerHandler())
	defer owner.Close()
	request := func(method, url string, body []byte, headers map[string]string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+testToken)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	if res := request("GET", network.URL+"/api/v2/sessions", nil, nil); res.StatusCode != 404 {
		t.Fatal("network can access owner queries", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/changes/head", nil, nil); res.StatusCode != 404 {
		t.Fatal("network can access owner change head", res.StatusCode)
	}
	for _, base := range []string{network.URL, owner.URL} {
		if res := request("POST", base+"/api/v2/local/secrets", []byte(`{"files":[]}`), map[string]string{"Content-Type": "application/json"}); res.StatusCode != 404 {
			t.Fatal("hub gained secret-scan filesystem authority", res.StatusCode)
		}
		if res := request("GET", base+"/api/v2/local/identity", nil, nil); res.StatusCode != 404 {
			t.Fatal("hub exposes local identity configuration", res.StatusCode)
		}
		if res := request("POST", base+"/api/v2/local/unsaved", []byte(`{"paths":["C:/fixture.txt"]}`), map[string]string{"Content-Type": "application/json"}); res.StatusCode != 404 {
			t.Fatal("hub gained local filesystem authority", res.StatusCode)
		}
	}
	if res := request("GET", network.URL+"/api/v2/unsaved-candidates?machineId=machine", nil, nil); res.StatusCode != 404 {
		t.Fatal("network exposes local-work candidates", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/behavior-patterns", nil, nil); res.StatusCode != 404 {
		t.Fatal("network exposes behavior queries", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/sessions/session/delegations/1/matches", nil, nil); res.StatusCode != 404 {
		t.Fatal("delegation matching exposed on ingestion interface", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/sessions/session/delegations/1/child", nil, nil); res.StatusCode != 404 {
		t.Fatal("delegation child resolution exposed on ingestion interface", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/sessions/session/trace-steps?agent=main", nil, nil); res.StatusCode != 404 {
		t.Fatal("trace evidence exposed on ingestion interface", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/behavior-roles", nil, nil); res.StatusCode != 404 {
		t.Fatal("network exposes role accounting queries", res.StatusCode)
	}
	raw := []byte("{\"type\":\"user\",\"uuid\":\"u\",\"message\":{\"content\":\"complete searchable history\"}}\n")
	source := protocol.Source{MachineID: "machine", SourceID: "source", Generation: "first", Provider: "claude", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	meta, _ := json.Marshal(protocol.Chunk{Source: source, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])})
	headers := map[string]string{"X-AMC-Chunk": base64.RawURLEncoding.EncodeToString(meta)}
	var receipt protocol.Receipt
	for i := 0; i < 2; i++ {
		res := request("POST", network.URL+"/v2/ingest/chunks", raw, headers)
		if res.StatusCode != 200 {
			t.Fatal("chunk", res.StatusCode)
		}
		var got protocol.Receipt
		if err = json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if i == 1 && got.ReceiptID != receipt.ReceiptID {
			t.Fatal("lost ACK generated a second receipt")
		}
		receipt = got
	}
	x := &indexer.Indexer{Store: s}
	if _, err = x.Once(context.Background(), source.SourceID, source.Generation); err != nil {
		t.Fatal(err)
	}
	id := parser.SessionID(source)
	undoPath := "/api/v2/sessions/" + id + "/git-undos"
	if res := request("GET", network.URL+"/api/v2/sessions/"+id+"/agent-lifecycle?agentId=main", nil, nil); res.StatusCode != 404 {
		t.Fatal("collector can query owner lifecycle", res.StatusCode)
	}
	if res := request("GET", network.URL+undoPath+"/1/prior-edits?snapshot=x", nil, nil); res.StatusCode != 404 {
		t.Fatal("collector can access prior edit evidence", res.StatusCode)
	}
	for _, path := range []string{"/api/v2/trouble-files", "/api/v2/git-undos", "/api/v2/git-undos/projects", "/api/v2/analytics/git-undos"} {
		if res := request("GET", network.URL+path, nil, nil); res.StatusCode != 404 {
			t.Fatal("collector can access fleet undo", res.StatusCode)
		}
		if res := request("GET", owner.URL+path, nil, nil); res.StatusCode != 200 {
			t.Fatal("owner cannot access fleet undo", res.StatusCode)
		}
	}
	if res := request("GET", network.URL+undoPath, nil, nil); res.StatusCode != 404 {
		t.Fatal("collector credential can access undo history", res.StatusCode)
	}
	if res := request("GET", owner.URL+undoPath, nil, nil); res.StatusCode != 200 {
		t.Fatal("owner cannot access undo history", res.StatusCode)
	}
	hookPath := "/api/v2/sessions/" + id + "/hook-evidence/javascript"
	if res := request("GET", network.URL+"/api/v2/analytics/hooks/models", nil, nil); res.StatusCode != 404 {
		t.Fatal("collector credential can access model attribution", res.StatusCode)
	}
	if res := request("GET", owner.URL+"/api/v2/analytics/hooks/models", nil, nil); res.StatusCode != 200 {
		t.Fatal("owner cannot access model attribution", res.StatusCode)
	}
	if res := request("GET", network.URL+"/api/v2/analytics/hooks/javascript", nil, nil); res.StatusCode != 404 {
		t.Fatal("collector credential can access fleet hook evidence", res.StatusCode)
	}
	if res := request("GET", network.URL+hookPath, nil, nil); res.StatusCode != 404 {
		t.Fatal("collector credential can access owner hook evidence", res.StatusCode)
	}
	if res := request("GET", owner.URL+hookPath, nil, nil); res.StatusCode != 200 {
		t.Fatal("owner cannot access hook evidence", res.StatusCode)
	}
	mutation := []byte(`{"archived":true,"revision":0,"operationId":"save-1"}`)
	for i := 0; i < 2; i++ {
		res := request("PATCH", owner.URL+"/api/v2/sessions/"+id+"/organization", mutation, nil)
		if res.StatusCode != 200 {
			t.Fatal("archive retry", res.StatusCode)
		}
	}
	headResponse := request("GET", owner.URL+"/api/v2/changes/head", nil, nil)
	var head struct {
		Sequence      int64  `json:"sequence"`
		HubID         string `json:"hubId"`
		RecoveryEpoch string `json:"recoveryEpoch"`
	}
	if err := json.NewDecoder(headResponse.Body).Decode(&head); err != nil {
		t.Fatal(err)
	}
	wantHead, err := s.ChangeHead(context.Background())
	if err != nil || headResponse.StatusCode != 200 || head.Sequence != wantHead || head.Sequence == 0 || head.HubID != h.HubID || head.RecoveryEpoch != s.RecoveryEpoch() || headResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("change head: %+v, status %d, error %v", head, headResponse.StatusCode, err)
	}
	stale := request("PATCH", owner.URL+"/api/v2/sessions/"+id+"/organization", []byte(`{"archived":false,"revision":0,"operationId":"save-2"}`), nil)
	if stale.StatusCode != 409 {
		t.Fatal("stale edit not rejected", stale.StatusCode)
	}
	session, err := s.GetSession(context.Background(), id)
	if err != nil || !session.Metadata.Archived {
		t.Fatal("archive lost", err)
	}
	res := request("GET", owner.URL+"/api/v2/search?q=searchable", nil, nil)
	var page store.EventPage
	if err = json.NewDecoder(res.Body).Decode(&page); err != nil || len(page.Events) != 1 {
		t.Fatal("full search", page, err)
	}
	if page.Snapshot == "" || page.Events[0].HistorySnapshot == "" {
		t.Fatal("HTTP search omitted cursor bindings", page)
	}
	delegatedResponse := request("GET", owner.URL+"/api/v2/search?q=searchable&delegatedOnly=true", nil, nil)
	var delegatedPage store.EventPage
	if err = json.NewDecoder(delegatedResponse.Body).Decode(&delegatedPage); err != nil || delegatedResponse.StatusCode != 200 || len(delegatedPage.Events) != 0 {
		t.Fatal("delegated search included ordinary user text", delegatedPage, err)
	}
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"q=searchable&after=invalid", 400},
		{"q=searchable&delegatedOnly=invalid", 400},
		{"q=searchable&delegatedOnly=true&mode=unknown", 400},
		{"q=searchable&mode=related", 400},
		{"q=searchable&delegatedOnly=true&mode=related", 200},
		{"q=searchable&delegatedOnly=true&snapshot=" + page.Snapshot, 409},
		{"q=searchable&delegatedOnly=false&snapshot=" + page.Snapshot, 200},
		{"q=searchable&after=9223372036854775808", 400},
		{"q=searchable&after=-1", 400},
		{"q=searchable&after=1", 400},
		{"q=different&snapshot=" + page.Snapshot, 409},
		{"q=searchable&after=" + strconv.FormatInt(page.Events[0].Sequence, 10) + "&snapshot=" + page.Snapshot, 200},
	} {
		response := request("GET", owner.URL+"/api/v2/search?"+tc.query, nil, nil)
		if response.StatusCode != tc.status {
			t.Fatalf("search %s: got %d want %d", tc.query, response.StatusCode, tc.status)
		}
	}
	beat, _ := json.Marshal(protocol.Heartbeat{MachineID: "machine", Name: "Renamed machine", State: "idle"})
	res = request("POST", network.URL+"/v2/ingest/heartbeat", beat, nil)
	if res.StatusCode != 204 || res.Header.Get("X-AMC-Recovery-Epoch") == "" {
		t.Fatal("heartbeat epoch", res.StatusCode)
	}
	if res = request("POST", network.URL+"/v2/ingest/chunks", raw, map[string]string{"X-AMC-Chunk": headers["X-AMC-Chunk"], "Authorization": "bad"}); res.StatusCode != 401 {
		t.Fatal("unauthenticated accepted")
	}
}
func TestBoundedAndInvalidRequests(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	for _, body := range []string{`{}`, `null`, `{} {}`, strings.Repeat("x", 70<<10)} {
		r := httptest.NewRequest("POST", "/v2/ingest/heartbeat", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.IngestionHandler().ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("invalid heartbeat accepted", w.Code)
		}
	}
}
