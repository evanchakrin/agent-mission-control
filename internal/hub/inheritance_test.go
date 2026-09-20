package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestInheritanceInspectionIsReadOnlyAndOwnerOnly(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, id := range []string{"parent", "child"} {
		header := map[string]string{"id": id}
		if id == "child" {
			header["forked_from_id"] = "parent"
		}
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": header})
		raw := append(meta, []byte("\n"+`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":5}}}}`+"\n")...)
		source := protocol.Source{MachineID: "machine", SourceID: id, Generation: "g", Provider: "codex", Size: int64(len(raw))}
		hash := sha256.Sum256(raw)
		if _, err := s.IngestChunk(ctx, protocol.Chunk{Source: source, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := (&indexer.Indexer{Store: s}).Once(ctx, id, "g"); err != nil {
			t.Fatal(err)
		}
		ids[id] = parser.SessionID(source)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, ids["child"], store.MetadataPatch{Archived: &yes, OperationID: "archive-inspection"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession(ctx, ids["child"])
	if err != nil {
		t.Fatal(err)
	}
	usageBefore, err := s.GetUsage(ctx, ids["child"], "", 100)
	if err != nil {
		t.Fatal(err)
	}
	headBefore, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, strings.Repeat("x", 32), "test")
	path := "/api/v2/sessions/" + ids["child"] + "/inheritance"
	w := httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+"?parent="+ids["parent"], nil))
	var result accounting.ForkBaselineInspection
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.State != "matched-baseline" || result.Match == nil || result.Match.TokensIn != 100 || result.Match.TokensOut != 5 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var automatic accounting.ForkBaselineInspection
	if err := json.Unmarshal(w.Body.Bytes(), &automatic); err != nil || w.Code != 200 || !reflect.DeepEqual(automatic, result) {
		t.Fatal("automatic parent inspection", w.Code, automatic, err)
	}
	for _, query := range []string{"?parent=", "?parent=a&parent=b", "?parent=a&unknown=b"} {
		w := httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+query, nil))
		if w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
	w = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path+"?parent="+ids["parent"], nil)
	request.Header.Set("Authorization", "Bearer "+h.Token)
	h.IngestionHandler().ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("collector reached owner inspection", w.Code)
	}
	contributionPath := "/api/v2/sessions/" + ids["child"] + "/contributions"
	w = httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, contributionPath+"?limit=1", nil))
	var contributionPage store.UsageContributionPage
	if err := json.Unmarshal(w.Body.Bytes(), &contributionPage); err != nil || w.Code != 200 || len(contributionPage.Items) != 1 || !contributionPage.Items[0].Counted || contributionPage.Scope != "verified-usage-exclusions-only" {
		t.Fatal("contributions", w.Code, contributionPage, err)
	}
	for _, q := range []string{"?limit=101", "?limit=0", "?limit=1&limit=2", "?unknown=x", "?cursor=x"} {
		w = httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, contributionPath+q, nil))
		if w.Code != 400 {
			t.Fatal("invalid contribution query", q, w.Code)
		}
	}
	w = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, contributionPath, nil)
	request.Header.Set("Authorization", "Bearer "+h.Token)
	h.IngestionHandler().ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("collector reached contribution query", w.Code)
	}
	w = httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/contribution-totals?provider=codex", nil))
	var contributionTotals store.UsageContributionTotals
	if err := json.Unmarshal(w.Body.Bytes(), &contributionTotals); err != nil || w.Code != 200 || contributionTotals.Recorded.Total != 210 || contributionTotals.Counted.Total != 210 || contributionTotals.ActiveExclusions != 0 {
		t.Fatal("contribution totals", w.Code, contributionTotals, err)
	}
	w = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v2/contribution-totals", nil)
	request.Header.Set("Authorization", "Bearer "+h.Token)
	h.IngestionHandler().ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("collector reached accounting aggregate", w.Code)
	}
	// A fork relationship is not authorization to treat two native threads as
	// transcript copies. The mutation route must remain off the collector mux.
	reconcilePath := "/api/v2/sessions/" + ids["child"] + "/reconcile-duplicates"
	payload, _ := json.Marshal(map[string]any{"otherSessionId": ids["parent"], "limit": 1})
	w = httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, reconcilePath, bytes.NewReader(payload)))
	if w.Code != 400 {
		t.Fatal("independent fork treated as duplicate", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, reconcilePath, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+h.Token)
	h.IngestionHandler().ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("collector reached reconciliation mutation", w.Code)
	}
	after, err := s.GetSession(ctx, ids["child"])
	if err != nil {
		t.Fatal(err)
	}
	usageAfter, err := s.GetUsage(ctx, ids["child"], "", 100)
	if err != nil {
		t.Fatal(err)
	}
	headAfter, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(usageBefore, usageAfter) || headBefore != headAfter {
		t.Fatal("read-only inspection changed usage, organization, or ledger")
	}
	t.Run("owner reconciliation", func(t *testing.T) {
		path := "/api/v2/sessions/" + ids["child"] + "/reconcile-inheritance"
		payload, _ := json.Marshal(map[string]string{"parentSessionId": ids["parent"]})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+h.Token)
		h.IngestionHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("collector reached fork mutation", w.Code)
		}
		w = httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if w.Code != 400 {
			t.Fatal("missing parent accepted", w.Code)
		}
		var previousProof string
		var selectedHead int64
		for attempt := 0; attempt < 2; attempt++ {
			w = httptest.NewRecorder()
			h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload)))
			var response struct {
				Selected bool   `json:"selected"`
				ProofID  string `json:"proofId"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || !response.Selected || response.ProofID == "" {
				t.Fatal("fork mutation", w.Code, w.Body.String(), err)
			}
			head, err := s.ChangeHead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if attempt == 1 && (previousProof != response.ProofID || selectedHead != head) {
				t.Fatal("retry changed receipt or ledger")
			}
			previousProof, selectedHead = response.ProofID, head
		}
		totals, err := s.ContributionTotals(ctx, store.SessionQuery{})
		if err != nil || totals.Recorded.Total != 210 || totals.Excluded.Total != 105 || totals.Counted.Total != 105 {
			t.Fatal(totals, err)
		}
		after, err := s.GetSession(ctx, ids["child"])
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("mutation changed source history or archive", err)
		}
	})
}
