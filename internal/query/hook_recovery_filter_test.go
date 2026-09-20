package query

import (
	"net/http/httptest"
	"testing"
)

func TestMissingHookQueryIsExplicitAndCatalogOnly(t *testing.T) {
	for _, path := range []string{"/api/v2/sessions", "/api/v2/totals"} {
		for _, value := range []string{"true", "false"} {
			q, err := ParseSessionQuery(httptest.NewRequest("GET", path+"?missingUndoEvidence="+value, nil))
			if err != nil || q.MissingUndoEvidence != (value == "true") {
				t.Fatal(q, err)
			}
		}
	}
	for _, path := range []string{"/api/v2/sessions?missingUndoEvidence=oops", "/api/v2/sessions?missingUndoEvidence=", "/api/v2/analytics/economics?missingUndoEvidence=true"} {
		if _, err := ParseSessionQuery(httptest.NewRequest("GET", path, nil)); err == nil {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/api/v2/sessions", "/api/v2/totals"} {
		for _, value := range []string{"true", "false"} {
			q, err := ParseSessionQuery(httptest.NewRequest("GET", path+"?missingJavaScriptEvidence="+value, nil))
			if err != nil || q.MissingJavaScriptEvidence != (value == "true") {
				t.Fatal(q, err)
			}
		}
	}
	for _, path := range []string{"/api/v2/sessions?missingJavaScriptEvidence=oops", "/api/v2/sessions?missingJavaScriptEvidence=", "/api/v2/analytics/economics?missingJavaScriptEvidence=true", "/api/v2/sessions/id/usage?missingJavaScriptEvidence=true"} {
		if _, err := ParseSessionQuery(httptest.NewRequest("GET", path, nil)); err == nil {
			t.Fatal("unsupported filter accepted", path)
		}
	}
}
