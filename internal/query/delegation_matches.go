package query

import (
	"net/http"
	"strconv"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func registerDelegationMatches(mux *http.ServeMux, s *store.Store) {
	registerTraceSteps(mux, s)
	mux.HandleFunc("GET /api/v2/sessions/{id}/delegations/{sequence}/child", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if key != "snapshot" || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		anchor, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		value, err := s.ResolveDelegationChild(r.Context(), r.PathValue("id"), anchor, v.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/delegations/{sequence}/matches", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if len(values) != 1 || key != "after" && key != "limit" && key != "snapshot" {
				fail(w, store.ErrInvalid)
				return
			}
		}
		anchor, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		var after int64
		limit := 20
		if v.Has("after") {
			after, err = strconv.ParseInt(v.Get("after"), 10, 64)
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if v.Has("limit") {
			limit, err = strconv.Atoi(v.Get("limit"))
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := s.DelegationMatches(r.Context(), r.PathValue("id"), anchor, after, limit, v.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
}
