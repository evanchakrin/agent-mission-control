package query

import "testing"

func TestAgentToolRouteRequiresExplicitIdentityAndReturnsFullCounts(t *testing.T) {
	_, mux := queryFixture(t)
	response(t, mux, "/api/v2/sessions/session/agent-tools", 400)
	response(t, mux, "/api/v2/sessions/missing/agent-tools?agentId=main", 404)
	page := response(t, mux, "/api/v2/sessions/session/agent-tools?agentId=main&limit=1", 200)
	tools := page["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["calls"] != float64(1) {
		t.Fatal(page)
	}
	page = response(t, mux, "/api/v2/sessions/session/agent-tools?agentId=", 200)
	if len(page["tools"].([]any)) != 0 {
		t.Fatal("empty identity broadened to all agents", page)
	}
}
