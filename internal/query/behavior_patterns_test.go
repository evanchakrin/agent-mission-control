package query

import "testing"

func TestBehaviorPatternRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/behavior-patterns", 200)
	if page["snapshot"] == "" || page["patterns"] == nil {
		t.Fatal(page)
	}
	for _, q := range []string{"limit=101", "limit=0", "limit=1&limit=2", "cursor=bad", "archived=wrong", "sort=cost"} {
		response(t, mux, "/api/v2/behavior-patterns?"+q, 400)
	}
}

func TestBehaviorRoleRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/behavior-roles", 200)
	if page["snapshot"] == "" || page["roles"] == nil {
		t.Fatal(page)
	}
	for _, q := range []string{"limit=101", "limit=0", "cursor=bad", "limit=1&limit=2", "archived=wrong", "sort=cost"} {
		response(t, mux, "/api/v2/behavior-roles?"+q, 400)
	}
}
