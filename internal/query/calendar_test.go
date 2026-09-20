package query

import (
	"net/url"
	"testing"
)

func TestCalendarRouteCountsSessionsWithoutMultiplyingUsage(t *testing.T) {
	_, mux := queryFixture(t)
	base := "/api/v2/analytics/calendar?start=2026-09-03&end=2026-09-05&timezone=America%2FNew_York&limit=1"
	page := response(t, mux, base, 200)
	days := page["days"].([]any)
	d := days[0].(map[string]any)
	if len(days) != 1 || d["day"] != "2026-09-03" || d["sessions"] != float64(1) || d["errors"] != float64(1) || d["agentScopes"] != float64(2) {
		t.Fatalf("wrong calendar: %+v", page)
	}
	if d["costEstimate"] != nil || d["topTierCost"] != nil || d["sessionsWithEstimate"] != float64(0) {
		t.Fatal("invented pricing", d)
	}
	continuation := "&cursor=" + url.QueryEscape(page["nextCursor"].(string)) + "&snapshot=" + page["snapshot"].(string)
	next := response(t, mux, base+continuation, 200)
	if next["days"].([]any)[0].(map[string]any)["sessions"] != float64(0) || next["nextCursor"] != nil {
		t.Fatal("wrong empty day or continuation", next)
	}
	response(t, mux, base+"&provider=claude"+continuation, 400)
	for _, query := range []string{"", "start=bad&end=2026-09-05&timezone=UTC", "start=2026-09-05&end=2026-09-03&timezone=UTC", "start=2020-01-01&end=2026-01-01&timezone=UTC", "start=2026-01-01&end=2026-01-02&timezone=Local", "start=2026-01-01&end=2026-01-02&timezone=UTC&limit=101", "start=2026-01-01&end=2026-01-02&timezone=UTC&from=2026-01-01"} {
		response(t, mux, "/api/v2/analytics/calendar?"+query, 400)
	}
}
