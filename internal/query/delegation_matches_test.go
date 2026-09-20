package query

import "testing"

func TestDelegationMatchRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	base := "/api/v2/sessions/session/delegations/1/matches"
	page := response(t, mux, base, 200)
	if page["state"] != "not-delegation" || page["snapshot"] == "" || page["matches"] == nil || page["coverage"] != "indexed-fingerprints-only" {
		t.Fatal(page)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=no", "after=-1", "after=1", "after=no", "limit=1&limit=2", "archived=false"} {
		response(t, mux, base+"?"+q, 400)
	}
	response(t, mux, base+"?snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/session/delegations/0/matches", 400)
	response(t, mux, "/api/v2/sessions/session/delegations/no/matches", 400)
	response(t, mux, "/api/v2/sessions/missing/delegations/1/matches", 404)
}

func TestDelegationChildRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	base := "/api/v2/sessions/session/delegations/1/child"
	value := response(t, mux, base, 200)
	if value["state"] != "not-supported-spawn" || value["snapshot"] == "" {
		t.Fatal(value)
	}
	for _, q := range []string{"limit=1", "snapshot=a&snapshot=b"} {
		response(t, mux, base+"?"+q, 400)
	}
	response(t, mux, base+"?snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/session/delegations/0/child", 400)
	response(t, mux, "/api/v2/sessions/session/delegations/no/child", 400)
	response(t, mux, "/api/v2/sessions/session/delegations/999/child", 404)
}
