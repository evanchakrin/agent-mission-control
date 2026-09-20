package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestMachineLabelsOwnerOnlyAndRevisionChecks(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	path := "/api/v2/machines/stable/label"
	body := `{"machineId":"stable","displayName":"Owner label","revision":0,"operationId":"rename"}`
	request := func(handler http.Handler, method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{"GET", "POST"} {
		if w := request(h.IngestionHandler(), method, body); w.Code != 404 {
			t.Fatal("network owner access", method, w.Code)
		}
	}
	owner := h.OwnerHandler()
	for i := 0; i < 2; i++ {
		w := request(owner, "POST", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var label store.MachineLabel
		if err = json.Unmarshal(w.Body.Bytes(), &label); err != nil || label.Revision != 1 || label.DisplayName != "Owner label" {
			t.Fatal(label, err)
		}
	}
	w := request(owner, "GET", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"revision":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(owner, "POST", strings.Replace(body, `"rename"`, `"stale"`, 1)); w.Code != 409 {
		t.Fatal("stale revision", w.Code)
	}
	if w = request(owner, "POST", strings.Replace(body, `"stable"`, `"different"`, 1)); w.Code != 400 {
		t.Fatal("path mismatch", w.Code)
	}
	path += "/history?limit=1"
	if w = request(h.IngestionHandler(), "GET", ""); w.Code != 404 {
		t.Fatal("network audit access", w.Code)
	}
	w = request(owner, "GET", "")
	var history store.MachineLabelAuditPage
	if err = json.Unmarshal(w.Body.Bytes(), &history); w.Code != 200 || err != nil || len(history.Items) != 1 || history.Items[0].After.DisplayName != "Owner label" || history.Next != 0 {
		t.Fatal(w.Code, history, err)
	}
	path += "&after=-1"
	if w = request(owner, "GET", ""); w.Code != 400 {
		t.Fatal("negative history cursor", w.Code)
	}
}
