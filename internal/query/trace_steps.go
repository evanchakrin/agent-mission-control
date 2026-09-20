package query

import (
	"net/http"
	"strconv"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func registerTraceSteps(mux *http.ServeMux, s *store.Store) {
	mux.HandleFunc("GET /api/v2/sessions/{id}/trace-steps", func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		for key, list := range values {
			if len(list) != 1 || key != "agent" && key != "after" && key != "limit" && key != "snapshot" {
				fail(w, store.ErrInvalid)
				return
			}
		}
		var after int64
		limit := 20
		var err error
		if values.Has("after") {
			after, err = strconv.ParseInt(values.Get("after"), 10, 64)
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if values.Has("limit") {
			limit, err = strconv.Atoi(values.Get("limit"))
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := s.TraceSteps(r.Context(), r.PathValue("id"), values.Get("agent"), after, limit, values.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
}
