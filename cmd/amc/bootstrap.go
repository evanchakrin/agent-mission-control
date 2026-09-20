package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// Reuse the desktop-owned client; per-bootstrap transports retain idle pipes.
func bootstrapHandler(client *http.Client, csrf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://amc/api/v2/health", nil)
		if err != nil {
			http.Error(w, "Hub unavailable", http.StatusServiceUnavailable)
			return
		}
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "Hub unavailable", http.StatusServiceUnavailable)
			return
		}
		defer response.Body.Close()
		var health map[string]any
		if response.StatusCode != http.StatusOK {
			http.Error(w, "Hub status unavailable", http.StatusServiceUnavailable)
			return
		}
		if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil || health == nil {
			http.Error(w, "Hub status unavailable", http.StatusServiceUnavailable)
			return
		}
		health["csrf"] = csrf
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(health)
	}
}
