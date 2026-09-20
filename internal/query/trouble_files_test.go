package query

import "testing"

func TestTroubleFileRoutesAndBounds(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/trouble-files?limit=20", 200)
	if page["minSessions"] != float64(5) || page["snapshot"] == "" {
		t.Fatal(page)
	}
	for _, field := range []string{"path", "sessions", "badSessions", "unknownSessions", "rate", "lastTouched"} {
		value := response(t, mux, "/api/v2/trouble-files?sort="+field+"&direction=asc", 200)
		if value["sort"] != field || value["direction"] != "asc" {
			t.Fatal(value)
		}
	}
	response(t, mux, "/api/v2/trouble-files?sort=untrusted", 400)
	response(t, mux, "/api/v2/trouble-files?direction=sideways", 400)
	for _, q := range []string{"limit=101", "machineId=x", "cursor=bad", "limit=1&limit=2"} {
		response(t, mux, "/api/v2/trouble-files?"+q, 400)
	}
	response(t, mux, "/api/v2/trouble-files/sessions?machineId=m&project=p&path=file.go&limit=25", 200)
	for _, q := range []string{"", "machineId=m", "machineId=m&path=p&archived=false", "machineId=m&path=p&limit=101"} {
		response(t, mux, "/api/v2/trouble-files/sessions?"+q, 400)
	}
}

func TestUnsavedCandidateRouteBounds(t *testing.T) {
	_, mux := queryFixture(t)
	page := response(t, mux, "/api/v2/unsaved-candidates?machineId=fixture&limit=20", 200)
	if page["machineId"] != "fixture" || page["snapshot"] == "" || page["totalFiles"] != float64(0) {
		t.Fatal(page)
	}
	for _, query := range []string{"", "machineId=x&limit=101", "machineId=x&limit=0", "machineId=x&limit=", "machineId=x&limit=1&limit=2", "machineId=x&project=p", "machineId=x&machineId=y", "machineId=x&cursor=bad"} {
		response(t, mux, "/api/v2/unsaved-candidates?"+query, 400)
	}
}
