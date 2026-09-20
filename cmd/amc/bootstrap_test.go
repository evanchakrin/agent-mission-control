package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapReusesDesktopClient(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/health" {
			t.Error("wrong hub endpoint", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"version":"fixture"}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	handler := bootstrapHandler(&http.Client{Transport: transport, Timeout: time.Second}, "fixture-csrf")
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("GET", "/api/v2/bootstrap", nil))
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || value["version"] != "fixture" || value["csrf"] != "fixture-csrf" {
			t.Fatal("bootstrap changed", w.Body.String())
		}
	}
	if connections.Load() != 1 {
		t.Fatal("bootstrap did not reuse connection", connections.Load())
	}
}

type bootstrapTransport func(*http.Request) (*http.Response, error)

func (f bootstrapTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBootstrapCancelsHubRequest(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: bootstrapTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		defer close(done)
		bootstrapHandler(client, "fixture")(w, httptest.NewRequest("GET", "/api/v2/bootstrap", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hub request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hub request ignored browser cancellation")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("wrong canceled status", w.Code)
	}
}

func TestBootstrapRejectsInvalidHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unavailable", `{"error":"blocked"}`, 503},
		{"null", `null`, 200},
		{"malformed", `{`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: bootstrapTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			w := httptest.NewRecorder()
			bootstrapHandler(client, "fixture-csrf")(w, httptest.NewRequest("GET", "/api/v2/bootstrap", nil))
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "fixture-csrf") {
				t.Fatal("invalid health presented as bootstrap", w.Code, w.Body.String())
			}
		})
	}
}
