package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestEconomicsHistoryEndpointIsWholeFleetAndPaginated(t *testing.T) {
	s, mux := queryFixture(t)
	for _, id := range []string{"first", "second"} {
		if _, err := s.CaptureEconomics(context.Background(), id, "manual"); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/analytics/economics/history?limit=1", nil))
	var page store.EconomicsHistoryPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != "second" || page.NextCursor == "" {
		t.Fatalf("history page: %d %s", w.Code, w.Body.String())
	}
	for _, query := range []string{"provider=claude", "machineId=x", "limit=0", "limit=101", "cursor=bad"} {
		response(t, mux, "/api/v2/analytics/economics/history?"+query, 400)
	}
}

func TestLifetimeEndpointDoesNotInventChildIdentity(t *testing.T) {
	_, mux := queryFixture(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/analytics/economics/lifetimes?machineId=missing", nil))
	var result store.EconomicsLifetime
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ChildSources != 0 || len(result.Buckets) != 8 {
		t.Fatalf("unexpected lifetime response: %d %s", w.Code, w.Body.String())
	}
	response(t, mux, "/api/v2/analytics/economics/lifetimes?archived=invalid", 400)
}

func TestEconomicsMeasurementEndpoint(t *testing.T) {
	_, mux := queryFixture(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/analytics/economics/measurement?machineId=missing", nil))
	var result store.EconomicsMeasurement
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Version != 1 || result.MeasuredAt.IsZero() || result.Costs.Sessions != 0 || result.Costs.KnownCost != nil || len(result.Lifetimes.Buckets) != 8 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected measurement response: %d %s", w.Code, w.Body.String())
	}
	response(t, mux, "/api/v2/analytics/economics/measurement?archived=invalid", 400)
}
