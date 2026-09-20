package assets

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfflineReplayIsSelfContainedAndNetworkDisabled(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPreview(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/replay.html", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	if strings.Contains(body, "__AMC_REPLAY_") || strings.Contains(body, "<script src=") || !strings.Contains(body, "connect-src 'none'") {
		t.Fatal("viewer is not self-contained and network-disabled")
	}
	if !strings.Contains(body, `id="view"`) || !strings.Contains(body, `value="conversation"`) || !strings.Contains(body, `value="raw"`) {
		t.Fatal("offline display controls are missing from packaged viewer")
	}
	for _, name := range []string{"v2-replay.js", "v2-replay-ui.js"} {
		script, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		script = inlineScript(script)
		if strings.Contains(strings.ToLower(string(script)), "</script") {
			t.Fatal("unsafe script embedding")
		}
		sum := sha256.Sum256(script)
		if !strings.Contains(body, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || !strings.Contains(body, "<script>"+string(script)+"</script>") {
			t.Fatal("embedded script hash does not match exact bytes")
		}
	}
}

func TestPreviewEmbedsExistingDashboardWithoutFilesystemFallback(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPreview(mux)
	for _, path := range []string{"/preview/", "/app.js", "/v2-data.js", "/v2-secret-hook.js", "/v2-dashboard.js", "/style.css", "/favicon.svg"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		if path == "/preview/" && (!strings.Contains(w.Body.String(), `data-amc-backend="v2"`) || !strings.Contains(w.Body.String(), `src="/v2-dashboard.js"`)) {
			t.Fatal("missing candidate bootstrap")
		}
		if path == "/preview/" {
			secret := strings.Index(w.Body.String(), `src="/v2-secret-hook.js"`)
			dashboard := strings.Index(w.Body.String(), `src="/v2-dashboard.js"`)
			if secret < 0 || secret >= dashboard {
				t.Fatal("secret hook module must load before its dashboard consumer")
			}
		}
	}
	for _, path := range []string{"/assets.go", "/go.mod", "/preview/unknown", "/api/fleet"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("unexpected asset exposure", path, w.Code)
		}
	}
}
