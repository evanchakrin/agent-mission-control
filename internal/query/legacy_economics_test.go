package query

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestLegacyEconomicsRawDownloadPreservesUnsupportedBytes(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = os.MkdirAll(filepath.Join(dir, "legacy-assets"), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte{'{', '}', '\r', '\n', 255, 0, 'x'}
	if err = os.WriteFile(filepath.Join(dir, "legacy-assets", "econ-history.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, s)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/analytics/economics/legacy/raw", nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw) || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal(w.Code, w.Body.Bytes(), w.Header())
	}
	w = httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/v2/analytics/economics/legacy/raw", nil)
	request.Header.Set("Range", "bytes=4-6")
	mux.ServeHTTP(w, request)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), raw[4:]) {
		t.Fatal("raw resume differs", w.Code, w.Body.Bytes())
	}
	response(t, mux, "/api/v2/analytics/economics/legacy?provider=claude", 400)
	response(t, mux, "/api/v2/analytics/economics/legacy?limit=101", 400)
	page := response(t, mux, "/api/v2/analytics/economics/legacy?limit=1", 200)
	if page["evidence"] != "legacy-unverified" || page["nextCursor"] == nil {
		t.Fatal(page)
	}
}
