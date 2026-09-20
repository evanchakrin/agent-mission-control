// Package assets embeds the exact dashboard release with the executable. No
// service-account working directory or mutable web root is used at runtime.
package assets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"net/http"
	"time"
)

//go:embed index.html app.js v2-data.js v2-secret-hook.js v2-dashboard.js v2-replay.js v2-replay-ui.js replay.html style.css favicon.svg amc.ico
var files embed.FS

// HTML parsers normalize line endings before checking inline-script CSP hashes.
// Normalize explicitly so Windows CRLF checkouts produce identical hashes.
func inlineScript(body []byte) []byte {
	return bytes.ReplaceAll(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
}

// ReplayHTML returns the fixed self-contained, network-disabled offline viewer.
func ReplayHTML() []byte {
	body, _ := files.ReadFile("replay.html")
	reader, _ := files.ReadFile("v2-replay.js")
	ui, _ := files.ReadFile("v2-replay-ui.js")
	reader, ui = inlineScript(reader), inlineScript(ui)
	readerHash, uiHash := sha256.Sum256(reader), sha256.Sum256(ui)
	policy := "'sha256-" + base64.StdEncoding.EncodeToString(readerHash[:]) + "' 'sha256-" + base64.StdEncoding.EncodeToString(uiHash[:]) + "'"
	body = bytes.ReplaceAll(body, []byte("__AMC_REPLAY_HASHES__"), []byte(policy))
	body = bytes.ReplaceAll(body, []byte("__AMC_REPLAY_READER__"), reader)
	body = bytes.ReplaceAll(body, []byte("__AMC_REPLAY_UI__"), ui)
	return body
}

func RegisterPreview(mux *http.ServeMux) {
	mux.HandleFunc("GET /replay.html", func(w http.ResponseWriter, r *http.Request) {
		body := ReplayHTML()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="amc-offline-replay.html"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "amc-offline-replay.html", time.Time{}, bytes.NewReader(body))
	})
	mux.HandleFunc("GET /preview/{$}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := files.ReadFile("index.html")
		body = bytes.Replace(body, []byte(`<html lang="en">`), []byte(`<html lang="en" data-amc-backend="v2">`), 1)
		body = bytes.Replace(body, []byte(`<script src="/app.js"></script>`), []byte(`<script src="/v2-data.js"></script><script src="/v2-secret-hook.js"></script><script src="/v2-dashboard.js"></script><script src="/app.js"></script>`), 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body))
	})
	for _, name := range []string{"app.js", "v2-data.js", "v2-secret-hook.js", "v2-dashboard.js", "style.css", "favicon.svg", "amc.ico"} {
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			body, err := files.ReadFile(name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
		})
	}
}
