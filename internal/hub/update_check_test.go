package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateCheckBoundedCachedAndOwnerOnly(t *testing.T) {
	for _, body := range []string{`{"version":"8.1.0"}`, `{"version":"invalid"}`, strings.Repeat("x", 65537)} {
		calls := 0
		checker := updateChecker{client: &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != versionSource || r.Header.Get("Authorization") != "" {
				t.Fatal("unexpected outbound request")
			}
			if _, ok := r.Context().Deadline(); !ok {
				t.Fatal("no deadline")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}
		first := checker.check(context.Background(), "8.0.0-candidate.1")
		second := checker.check(context.Background(), "8.0.0-candidate.1")
		if calls != 1 || first != second {
			t.Fatal("not cached", calls, first, second)
		}
		if first.UpdateAvailable != (body == `{"version":"8.1.0"}`) {
			t.Fatal(first)
		}
		h := New(nil, testToken, "8.0.0-candidate.1")
		h.updates.value = first
		h.updates.next = checker.next
		w := httptest.NewRecorder()
		h.OwnerHandler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/update-check", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		w = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v2/update-check", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		h.IngestionHandler().ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatal("ingestion exposed release check", w.Code)
		}
	}
}

func TestNewerStable(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		newer, valid    bool
	}{
		{"8.0.0", "8.0.0-candidate.1", true, true}, {"7.34.0", "8.0.0-candidate.1", false, true},
		{"8.0.0", "8.0.0", false, true}, {"8.10.0", "8.9.0", true, true},
		{"8.0.0-next", "8.0.0", false, false}, {"08.0.0", "8.0.0", false, false},
	} {
		n, v := newerStable(tc.latest, tc.current)
		if n != tc.newer || v != tc.valid {
			t.Fatal(tc, n, v)
		}
	}
}
