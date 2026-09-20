package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestSourceCatalogOwnerBoundary(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	for _, tc := range []struct {
		handler http.Handler
		path    string
		status  int
	}{
		{h.IngestionHandler(), "/api/v2/machines/machine/sources", 404},
		{h.OwnerHandler(), "/api/v2/machines/machine/sources", 200},
		{h.OwnerHandler(), "/api/v2/machines/machine/sources?limit=no", 400},
		{h.OwnerHandler(), "/api/v2/machines/machine/sources?limit=101", 400},
		{h.OwnerHandler(), "/api/v2/machines/machine/sources?cursor=bad", 400},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		tc.handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}
