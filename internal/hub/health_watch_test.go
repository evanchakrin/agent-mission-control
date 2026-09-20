package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type blockedTotalsResponse struct {
	*httptest.ResponseRecorder
	release <-chan struct{}
}

func (w blockedTotalsResponse) WriteHeader(code int) {
	<-w.release
	w.ResponseRecorder.WriteHeader(code)
}

func TestOwnerAnalyticsTotalsHasItsOwnStallWatcher(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	warnings := make(chan string, 1)
	h.HealthStall = func(stage string, elapsed time.Duration) {
		if elapsed < time.Second {
			stage = "early-warning"
		}
		warnings <- stage
	}
	release := make(chan struct{})
	done := make(chan struct{})
	w := blockedTotalsResponse{httptest.NewRecorder(), release}
	// A deliberately blocked error response exercises the actual route wrapper
	// without timing filesystem I/O or changing the query's error semantics.
	r := httptest.NewRequest(http.MethodGet, "/api/v2/analytics/totals?limit=0", nil)
	go func() { defer close(done); h.OwnerHandler().ServeHTTP(w, r) }()
	defer func() { close(release); <-done }()
	select {
	case stage := <-warnings:
		if stage != "totals-query" {
			t.Fatal("wrong request stage", stage)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("analytics totals route did not report its own stall")
	}
	select {
	case <-done:
		t.Fatal("watcher completed a blocked response")
	default:
	}
}

func TestOrganizationWatchReportsDecodeWithoutCompletingMutation(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	warnings := make(chan string, 1)
	h.HealthStall = func(stage string, elapsed time.Duration) {
		if elapsed < 250*time.Millisecond {
			stage = "early-warning"
		}
		warnings <- stage
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan struct{})
	r := httptest.NewRequest("PATCH", "/api/v2/sessions/missing/organization", reader)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	go func() { defer close(done); h.OwnerHandler().ServeHTTP(w, r) }()
	defer func() { writer.Close(); <-done }()
	select {
	case stage := <-warnings:
		if stage != "organization-decode" {
			t.Fatal(stage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("organization stall was not reported")
	}
	select {
	case <-done:
		t.Fatal("watcher completed blocked request")
	default:
	}
	if _, err = writer.Write([]byte(`{"operationId":"decode-test","revision":0,"archived":true}`)); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	<-done
	if w.Code != 404 {
		t.Fatal("diagnostics changed not-found behavior", w.Code, w.Body.String())
	}
}

func TestHealthWatchReportsBlockedStageWithoutUnblockingRequest(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, testToken, "test")
	warnings := make(chan string, 2)
	h.HealthStall = func(stage string, elapsed time.Duration) {
		if elapsed < time.Second {
			warnings <- "early-warning"
			return
		}
		warnings <- stage
	}
	release := make(chan struct{})
	done := make(chan struct{})
	h.StorageStatus = func() any { <-release; return map[string]string{"state": "ready"} }
	r := httptest.NewRequest("GET", "/api/v2/health", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	go func() { defer close(done); h.OwnerHandler().ServeHTTP(w, r) }()
	defer func() { close(release); <-done }()
	select {
	case stage := <-warnings:
		if stage != "storage" {
			t.Fatal("wrong stalled stage", stage)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked health stage was not reported")
	}
	select {
	case <-done:
		t.Fatal("watcher changed request completion")
	default:
	}
}

func TestHealthWatchStopsWhenRequestCompletes(t *testing.T) {
	warnings := make(chan string, 2)
	stage, stop := watchHealth(func(s string, _ time.Duration) { warnings <- s }, 20*time.Millisecond)
	stage("response")
	stop()
	select {
	case got := <-warnings:
		t.Fatal("completed request warned", got)
	case <-time.After(50 * time.Millisecond):
	}
	stage, stop = watchHealth(nil, 0)
	stage("storage")
	stop()
}
