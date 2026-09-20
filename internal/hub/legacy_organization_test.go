package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestLegacyOrganizationOwnerImport(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	ctx := context.Background()
	if err := s.PutLegacyAlias(ctx, "old", "resolved"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/migrations/legacy-organization"
	body := `{"sessions":{"old":{"archived":true,"note":"preserved"},"late":{"archived":true}}}`
	for range 2 {
		w := httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, id := range []string{"resolved", "late"} {
		m, err := s.GetMetadata(ctx, id)
		if err != nil || !m.Archived || m.Revision != 1 {
			t.Fatal(id, m, err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	h.IngestionHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatal("network import exposed", w.Code)
	}
	for _, invalid := range []string{`{}`, `{"sessions":{"bad":null}}`, `{"sessions":{"bad":[]}}`, body + `{}`, `{"sessions":{},"aliases":{}}`, `{"sessions":{"big":{"note":"` + strings.Repeat("x", 1<<20) + `"}}}`} {
		w := httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(invalid)))
		if w.Code != 400 {
			t.Fatal("invalid import accepted", w.Code)
		}
	}
}
