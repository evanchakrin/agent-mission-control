package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOwnerAutomaticComparisonPolicy(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"r\",\"model\":\"known\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n")
	src := protocol.Source{MachineID: "fixture", SourceID: "fixture", Generation: "g", Provider: "claude", Size: int64(len(raw))}
	hash := sha256.Sum256(raw)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	x := indexer.Indexer{Store: s}
	if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	end := at.Add(24 * time.Hour)
	c := accounting.Catalog{ID: "comparison", ComparisonOnly: true, Rates: []accounting.Rate{{ID: "r", Model: "known", Context: "standard", EffectiveFrom: at, EffectiveTo: &end, Source: "fixture", Input: 1, Output: 1}}}
	if err = accounting.SaveCatalog(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	h := New(s, testToken, "test")
	owner := h.OwnerHandler()
	path := "/api/v2/sessions/" + parser.SessionID(src) + "/pricing-policy"
	for _, body := range []string{`{"catalogId":"comparison","context":"standard"}`, `{"catalogId":"comparison","context":"standard","comparisonAt":"2026-09-10T00:00:00Z"}`, `{"catalogId":"comparison","context":"standard","comparisonAt":"bad"}`} {
		w := httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest("PUT", path, strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	body := `{"catalogId":"comparison","context":"standard","comparisonAt":"2026-09-09T12:00:00Z"}`
	w := httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("PUT", path, strings.NewReader(body)))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var p store.PricingJob
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.ComparisonAt == nil || !p.ComparisonAt.Equal(at.Add(12*time.Hour)) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("PUT", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	h.IngestionHandler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("network policy exposed", w.Code)
	}
	if worked, e := accounting.PriceNext(ctx, s); !worked || e != nil {
		t.Fatal(worked, e)
	}
	rows, err := s.EstimateHistory(ctx, parser.SessionID(src), "", 100)
	if err != nil || len(rows) != 1 {
		t.Fatal(len(rows), err)
	}
	session, err := s.GetSession(ctx, parser.SessionID(src))
	if err != nil || session.CostEstimate != nil || session.Pricing != nil {
		t.Fatal("comparison published as history", err)
	}
	for _, endpoint := range []string{"/api/v2/sessions/" + session.ID, "/api/v2/sessions?provider=claude&limit=1"} {
		w = httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest("GET", endpoint, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"apiComparison":`) || !strings.Contains(w.Body.String(), `"comparisonAt":`) || !strings.Contains(w.Body.String(), `"costEstimate":null`) {
			t.Fatal("session API lost separate comparison", endpoint, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/run-activity", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"runs":`) {
		t.Fatal("run activity route", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	runRequest := httptest.NewRequest("GET", "/api/v2/run-activity", nil)
	runRequest.Header.Set("Authorization", "Bearer "+testToken)
	h.IngestionHandler().ServeHTTP(w, runRequest)
	if w.Code != 404 {
		t.Fatal("run activity exposed to satellite", w.Code)
	}
	staleSession := session
	staleSession.ProjectionRevision += "-stale"
	views, err := s.SessionResponses(ctx, []store.Session{session, staleSession})
	if err != nil || len(views) != 2 || len(views[0].APIComparison) == 0 || len(views[1].APIComparison) != 0 {
		t.Fatal("stale projection comparison leaked", views, err)
	}
	if _, err := s.SessionResponses(ctx, make([]store.Session, 501)); err == nil {
		t.Fatal("unbounded comparison page accepted")
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/totals?provider=claude", nil))
	var totals store.Totals
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &totals) != nil || totals.CurrentComparisons == nil || totals.CurrentComparisons.SessionsWithComparison != 1 {
		t.Fatal("owner totals lost comparison", w.Code, w.Body.String())
	}
	for _, invalid := range []string{`{"catalogId":"comparison","context":"standard"}`, `{"catalogId":"comparison","context":"standard","comparisonAt":"2026-09-10T00:00:00Z"}`} {
		w = httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v2/pricing-default", strings.NewReader(invalid)))
		if w.Code != 400 {
			t.Fatal("invalid default accepted", w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v2/pricing-default", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/pricing-default", nil))
	var configured store.ComparisonDefault
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &configured) != nil || configured.CatalogID != "comparison" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		w = httptest.NewRecorder()
		r = httptest.NewRequest(method, "/api/v2/pricing-default", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		h.IngestionHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("default exposed on ingestion", method, w.Code)
		}
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/v2/pricing-default", nil))
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	preserved, e := s.PricingPolicy(ctx, parser.SessionID(src))
	if e != nil || preserved.ComparisonAt == nil {
		t.Fatal("clearing default altered existing policy", preserved, e)
	}
}
