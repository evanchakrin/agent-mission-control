package query

import "testing"

func TestTraceStepRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	base := "/api/v2/sessions/session/trace-steps"
	page := response(t, mux, base+"?agent=main", 200)
	if page["comparison"] != "exact-tool-and-json-arguments-v1" || page["snapshot"] == "" || page["steps"] == nil {
		t.Fatal(page)
	}
	for _, q := range []string{"", "agent=", "agent=main&limit=0", "agent=main&limit=101", "agent=main&limit=no", "agent=main&after=-1", "agent=main&after=1", "agent=main&agent=other", "agent=main&unknown=x"} {
		response(t, mux, base+"?"+q, 400)
	}
	response(t, mux, base+"?agent=main&snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/missing/trace-steps?agent=main", 404)
}
