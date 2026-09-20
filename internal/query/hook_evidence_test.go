package query

import "testing"

func TestModelHookSummaryRejectsIgnoredFilters(t *testing.T) {
	_, mux := queryFixture(t)
	path := "/api/v2/analytics/hooks/models"
	value := response(t, mux, path, 200)
	if value["scope"] != "all-current-session-agent-pairs-including-archived" {
		t.Fatal(value)
	}
	for _, query := range []string{"?limit=5", "?archived=false", "?cursor=next"} {
		response(t, mux, path+query, 400)
	}
}

func TestHookSummaryRouteIsWholeFleetAndRejectsIgnoredFilters(t *testing.T) {
	_, mux := queryFixture(t)
	path := "/api/v2/analytics/hooks/javascript"
	value := response(t, mux, path, 200)
	if value["scope"] != "all-current-sessions-including-archived" || value["sessions"] != float64(1) || value["proposed"] != false {
		t.Fatal(value)
	}
	for _, query := range []string{"?limit=5", "?archived=false", "?cursor=next"} {
		response(t, mux, path+query, 400)
	}
}

func TestHookEvidenceRouteBindsSessionAndSnapshot(t *testing.T) {
	_, mux := queryFixture(t)
	path := "/api/v2/sessions/session/hook-evidence/javascript"
	value := response(t, mux, path, 200)
	if value["sessionId"] != "session" || value["state"] != "indexed-history" || value["snapshot"] == "" || value["indexedOffset"] != float64(16) {
		t.Fatal(value)
	}
	response(t, mux, path+"?snapshot="+value["snapshot"].(string), 200)
	response(t, mux, path+"?snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/missing/hook-evidence/javascript", 404)
}
