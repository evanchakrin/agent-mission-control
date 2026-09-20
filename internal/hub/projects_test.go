package hub

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestProjectRegistryOwnerBoundaryAndRevisionHTTP(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	body := `{"id":"p","name":"Project","color":"#123456","revision":0,"operationId":"create"}`
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "/api/v2/projects", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.IngestionHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("network project access", method, w.Code)
		}
	}
	owner := h.OwnerHandler()
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest("POST", "/api/v2/projects", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/projects?limit=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Project"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("POST", "/api/v2/projects", strings.NewReader(strings.ReplaceAll(body, `"create"`, `"stale"`))))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/projects?limit=banana", nil))
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/v2/projects/p/history?after=-1", "/api/v2/projects/p/history?after=bad"} {
		w = httptest.NewRecorder()
		owner.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/projects/p/history?limit=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"operationId":"create"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/v2/projects/p/history", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.IngestionHandler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("network audit access", w.Code)
	}
}
