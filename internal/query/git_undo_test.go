package query

import "testing"

func TestFleetUndoRoutesBoundAndRejectIgnoredFilters(t *testing.T) {
	_, mux := queryFixture(t)
	for _, q := range []string{"", "?snapshot=x&limit=0", "?snapshot=x&before=bad", "?snapshot=x&before=2", "?snapshot=x&unknown=1", "?snapshot=x&limit=1&limit=2"} {
		response(t, mux, "/api/v2/sessions/session/git-undos/1/prior-edits"+q, 400)
	}
	response(t, mux, "/api/v2/sessions/session/git-undos/1/prior-edits?snapshot=stale", 409)
	groups := response(t, mux, "/api/v2/git-undos/projects", 200)
	if groups["projects"] == nil || groups["snapshot"] == "" {
		t.Fatal(groups)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=bad", "cursor=bad", "project=x", "limit=1&limit=2"} {
		response(t, mux, "/api/v2/git-undos/projects?"+q, 400)
	}
	page := response(t, mux, "/api/v2/git-undos", 200)
	if page["scope"] != "all-current-sessions-including-archived" || page["events"] == nil || page["snapshot"] == "" {
		t.Fatal(page)
	}
	summary := response(t, mux, "/api/v2/analytics/git-undos", 200)
	filtered := response(t, mux, "/api/v2/git-undos?project=C%3A%2Frepo", 200)
	if filtered["project"] != "C:/repo" {
		t.Fatal(filtered)
	}
	response(t, mux, "/api/v2/git-undos?project=a&project=b", 400)
	if summary["sessions"] != float64(1) {
		t.Fatal(summary)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=bad", "cursor=bad", "archived=false", "limit=1&limit=2"} {
		response(t, mux, "/api/v2/git-undos?"+q, 400)
	}
	response(t, mux, "/api/v2/analytics/git-undos?limit=1", 400)
}

func TestGitUndoRoutePinsAndBoundsHistory(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/sessions/session/git-undos", 200)
	if page["state"] != "indexed-history" || page["snapshot"] == "" || page["events"] == nil {
		t.Fatal(page)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=oops", "after=-1", "after=oops", "after=1", "after="} {
		response(t, mux, "/api/v2/sessions/session/git-undos?"+query, 400)
	}
	response(t, mux, "/api/v2/sessions/session/git-undos?snapshot=stale", 409)
	response(t, mux, "/api/v2/sessions/missing/git-undos", 404)
}
