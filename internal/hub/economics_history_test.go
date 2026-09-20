package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestEconomicsCaptureIsOwnerOnlyAndRetrySafe(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	const path = "/api/v2/analytics/economics/history"
	body, err := json.Marshal(map[string]string{"operationId": "explicit-capture", "recoveryEpoch": s.RecoveryEpoch()})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	h.IngestionHandler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("ingestion can capture history", w.Code)
	}
	owner := h.OwnerHandler()
	var original []byte
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("capture: %d %s", w.Code, w.Body.String())
		}
		if i == 0 {
			original = append([]byte{}, w.Body.Bytes()...)
		} else if !bytes.Equal(original, w.Body.Bytes()) {
			t.Fatal("retry changed measurement")
		}
	}
	var entry store.EconomicsHistoryEntry
	if json.Unmarshal(original, &entry) != nil || entry.Reason != "manual" || entry.Measurement.Costs.KnownCost != nil {
		t.Fatal(string(original))
	}
	page, err := s.EconomicsHistory(context.Background(), "", 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"operationId":""}`))))
	if w.Code != 400 {
		t.Fatal("missing identity accepted", w.Code)
	}
}

func TestEconomicsCaptureResolutionIsOwnerOnlyAndDoesNotCreateHistory(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	const path = "/api/v2/analytics/economics/capture-resolutions"
	body, err := json.Marshal(map[string]string{"operationId": "unavailable", "originalEpoch": "old", "recoveryEpoch": s.RecoveryEpoch()})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		target := path
		if method == http.MethodGet {
			target += "/unavailable"
		}
		r := httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.IngestionHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("ingestion exposed reconciliation", method, w.Code)
		}
	}
	var original string
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if i == 0 {
			original = w.Body.String()
		} else if original != w.Body.String() {
			t.Fatal("retry changed audit")
		}
	}
	w := httptest.NewRecorder()
	h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+"/unavailable", nil))
	if w.Code != 200 || w.Body.String() != original {
		t.Fatal("audit not readable", w.Code, w.Body.String())
	}
	page, err := s.EconomicsHistory(context.Background(), "", 100)
	if err != nil || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	for _, suffix := range []string{"?limit=1", "?limit=0", "?limit=101", "?provider=codex", "?limit=1&limit=2"} {
		w = httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+suffix, nil))
		if suffix == "?limit=1" {
			var listed store.EconomicsResolutionPage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 || listed.Items[0].ID != "unavailable" {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 400 {
			t.Fatal("invalid audit filter accepted", suffix, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.IngestionHandler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("ingestion exposed audit list", w.Code)
	}
}
