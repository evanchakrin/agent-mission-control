package query

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplayPackageContainsFixedOfflineViewerAndExactSource(t *testing.T) {
	_, mux := queryFixture(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/sessions/session/replay.zip", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 4 {
		t.Fatal("unexpected bundle inventory", len(archive.File))
	}
	files := map[string]string{}
	for _, entry := range archive.File {
		if entry.Method != zip.Store || strings.ContainsAny(entry.Name, "/\\") {
			t.Fatal("unexpected compression or path", entry.Name)
		}
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name] = string(data)
	}
	if files["complete-source.jsonl"] != "source evidence\n" {
		t.Fatal("source bytes changed")
	}
	if !strings.Contains(files["replay.html"], "connect-src 'none'") || strings.Contains(files["replay.html"], "__AMC_REPLAY_") {
		t.Fatal("viewer is not self-contained")
	}
	if !strings.Contains(files["indexed-history.jsonl"], `"type":"complete"`) || !strings.Contains(files["README.txt"], "16 captured source bytes") {
		t.Fatal("completion or instructions missing")
	}
	notFound := httptest.NewRecorder()
	mux.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/api/v2/sessions/missing/replay.zip", nil))
	if notFound.Code != 404 {
		t.Fatal("missing session became partial download", notFound.Code)
	}
}
