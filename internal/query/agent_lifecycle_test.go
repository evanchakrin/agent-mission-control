package query

import "testing"

func TestAgentLifecycleRouteValidation(t *testing.T) {
	_, mux := queryFixture(t)
	value := response(t, mux, "/api/v2/sessions/session/agent-lifecycle?agentId=main", 200)
	if value["agentId"] != "main" || value["snapshot"] == "" {
		t.Fatal(value)
	}
	for _, suffix := range []string{"", "?agentId=", "?agentId=main&limit=5", "?agentId=main&agentId=child"} {
		response(t, mux, "/api/v2/sessions/session/agent-lifecycle"+suffix, 400)
	}
	response(t, mux, "/api/v2/sessions/session/agent-lifecycle?agentId=main&snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/missing/agent-lifecycle?agentId=main", 404)
}
