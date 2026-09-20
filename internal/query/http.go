// Package query registers owner-only read surfaces. It has no ingestion,
// organization-write, local-file, repository, or Git capabilities.
package query

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
	assets "github.com/evanchakrin/agent-mission-control/public"
)

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	status := 500
	code := "query_failed"
	if errors.Is(err, store.ErrHistoryChanged) {
		status = 409
		code = "history_changed"
	} else if errors.Is(err, store.ErrInvalid) {
		status = 400
		code = "invalid_query"
	} else if errors.Is(err, store.ErrNotFound) {
		status = 404
		code = "not_found"
	}
	jsonResponse(w, status, map[string]any{"code": code, "error": err.Error(), "retryable": status >= 500})
}

func ParseSessionQuery(r *http.Request) (store.SessionQuery, error) {
	v := r.URL.Query()
	q := store.SessionQuery{Cursor: v.Get("cursor"), MachineID: v.Get("machineId"), Provider: v.Get("provider"), Project: v.Get("project"), Text: v.Get("q"), Sort: v.Get("sort"), Direction: v.Get("direction")}
	var err error
	if v.Has("missingUndoEvidence") {
		if r.URL.Path != "/api/v2/sessions" && r.URL.Path != "/api/v2/totals" {
			return q, store.ErrInvalid
		}
		q.MissingUndoEvidence, err = strconv.ParseBool(v.Get("missingUndoEvidence"))
		if err != nil {
			return q, store.ErrInvalid
		}
	}
	if v.Has("missingJavaScriptEvidence") {
		if r.URL.Path != "/api/v2/sessions" && r.URL.Path != "/api/v2/totals" {
			return q, store.ErrInvalid
		}
		q.MissingJavaScriptEvidence, err = strconv.ParseBool(v.Get("missingJavaScriptEvidence"))
		if err != nil {
			return q, store.ErrInvalid
		}
	}
	if v.Has("projectAssignment") {
		assignment := v.Get("projectAssignment")
		if len(assignment) > 1024 {
			return q, store.ErrInvalid
		}
		q.ProjectAssignment = &assignment
	}
	if v.Get("pinnedFirst") != "" {
		q.PinnedFirst, err = strconv.ParseBool(v.Get("pinnedFirst"))
		if err != nil {
			return q, store.ErrInvalid
		}
	}
	if v.Get("limit") != "" {
		q.Limit, err = strconv.Atoi(v.Get("limit"))
		if err != nil || q.Limit < 1 {
			return q, store.ErrInvalid
		}
	}
	if v.Get("archived") != "" {
		value, e := strconv.ParseBool(v.Get("archived"))
		if e != nil {
			return q, store.ErrInvalid
		}
		q.Archived = &value
	}
	if v.Get("unassignedProject") != "" {
		q.UnassignedProject, err = strconv.ParseBool(v.Get("unassignedProject"))
		if err != nil {
			return q, store.ErrInvalid
		}
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if raw := v.Get(field.name); raw != "" {
			parsed, e := time.Parse(time.RFC3339Nano, raw)
			if e != nil {
				parsed, e = time.Parse("2006-01-02", raw)
			}
			if e != nil {
				return q, store.ErrInvalid
			}
			*field.target = &parsed
		}
	}
	return q, nil
}

// Register must be attached behind the desktop/named-pipe owner boundary.
// The receiver's network ingestion mux must never register these routes.
func Register(mux *http.ServeMux, s *store.Store) {
	registerDelegationMatches(mux, s)
	mux.HandleFunc("GET /api/v2/sessions/{id}/git-undos/{sequence}/prior-edits", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if (key != "snapshot" && key != "before" && key != "limit") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		var before int64
		if v.Has("before") {
			before, err = strconv.ParseInt(v.Get("before"), 10, 64)
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 25
		if v.Has("limit") {
			limit, err = strconv.Atoi(v.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := s.PriorUndoEdits(r.Context(), r.PathValue("id"), v.Get("snapshot"), sequence, before, limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v2/git-undos/projects", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if (key != "cursor" && key != "limit") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 25
		if v.Has("limit") {
			var err error
			limit, err = strconv.Atoi(v.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		value, err := s.GitUndoProjects(r.Context(), v.Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/git-undos", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if (key != "cursor" && key != "limit" && key != "project") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 50
		if v.Has("limit") {
			var err error
			limit, err = strconv.Atoi(v.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		var project *string
		if v.Has("project") {
			value := v.Get("project")
			project = &value
		}
		value, err := s.FleetGitUndoHistoryForProject(r.Context(), v.Get("cursor"), limit, project)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/git-undos", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) != 0 {
			fail(w, store.ErrInvalid)
			return
		}
		value, err := s.GitUndoSummary(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/git-undos", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		var after int64
		limit := 50
		var err error
		if v.Has("after") {
			after, err = strconv.ParseInt(v.Get("after"), 10, 64)
			if err != nil || after < 0 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if v.Has("limit") {
			limit, err = strconv.Atoi(v.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		value, err := s.GitUndoHistory(r.Context(), r.PathValue("id"), v.Get("snapshot"), after, limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/hooks/models", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) != 0 {
			fail(w, store.ErrInvalid)
			return
		}
		value, err := s.ModelHookSummary(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/hooks/javascript", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) != 0 {
			fail(w, store.ErrInvalid)
			return
		}
		value, err := s.JavaScriptHookSummary(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/hook-evidence/javascript", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.JavaScriptHookEvidence(r.Context(), r.PathValue("id"), r.URL.Query().Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/replay.zip", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		row, err := s.GetSession(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		if _, err = s.SourceState(r.Context(), row.SourceID, row.Generation); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="amc-replay.zip"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err = s.WriteReplayBundle(r.Context(), id, assets.ReplayHTML(), w); err != nil {
			// Never finalize a ZIP containing mixed or interrupted evidence.
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/export.ndjson", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.GetSession(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="indexed-history.jsonl"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := s.WriteIndexedExport(r.Context(), id, w); err != nil {
			// Headers may already be sent. A stream is valid only with its complete
			// trailer; errors never append that trailer or pretend to change status.
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "complete": false, "error": err.Error()})
		}
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/usage", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := s.GetUsagePinned(r.Context(), r.PathValue("id"), q.Cursor, q.Limit, r.URL.Query().Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/catalog/projects-ranked", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := s.RankedProjects(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/catalog/projects", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := s.CatalogProjects(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/catalog/machines", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := s.CatalogMachines(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/analytics/rings", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.Rings(r.Context(), store.RhythmQuery{SessionQuery: q, Timezone: r.URL.Query().Get("timezone")})
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/rhythm", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.Rhythm(r.Context(), store.RhythmQuery{SessionQuery: q, Timezone: r.URL.Query().Get("timezone")})
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/calendar", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.Calendar(r.Context(), store.CalendarQuery{SessionQuery: q, Start: r.URL.Query().Get("start"), End: r.URL.Query().Get("end"), Timezone: r.URL.Query().Get("timezone"), Snapshot: r.URL.Query().Get("snapshot")})
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/analytics/totals", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		totals, err := s.CatalogTotals(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, totals)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/costs", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		result, err := s.EconomicsCostTotals(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/legacy/raw", func(w http.ResponseWriter, r *http.Request) {
		f, err := s.OpenLegacyEconomics()
		if err != nil {
			fail(w, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="legacy-econ-history.jsonl"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "legacy-econ-history.jsonl", info.ModTime(), f)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/legacy", func(w http.ResponseWriter, r *http.Request) {
		for key := range r.URL.Query() {
			if key != "cursor" && key != "limit" {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := s.LegacyEconomics(r.Context(), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/history", func(w http.ResponseWriter, r *http.Request) {
		// These are whole-fleet measurements taken in the past, not projections
		// that can be retroactively filtered to the current machine/project.
		for key := range r.URL.Query() {
			if key != "cursor" && key != "limit" {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		result, err := s.EconomicsHistory(r.Context(), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/measurement", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		result, err := s.MeasureEconomics(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/lifetimes", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		result, err := s.EconomicsLifetimes(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("GET /api/v2/analytics/usage/{dimension}", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := s.GroupedUsage(r.Context(), store.GroupQuery{SessionQuery: q, Dimension: r.PathValue("dimension")})
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.SessionStats(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/agents", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.SessionAgents(r.Context(), r.PathValue("id"), q.Cursor, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/fingerprint", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.SessionFingerprint(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/cost-flow", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if q.Limit == 0 {
			q.Limit = 100
		}
		value, err := s.SessionCostFlow(r.Context(), r.PathValue("id"), q.Cursor, q.Limit, r.URL.Query().Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/behavior-roles", func(w http.ResponseWriter, r *http.Request) {
		for key, values := range r.URL.Query() {
			if len(values) != 1 || (key != "cursor" && key != "limit" && key != "machineId" && key != "provider" && key != "project" && key != "q" && key != "archived") {
				fail(w, store.ErrInvalid)
				return
			}
		}
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if q.Limit == 0 {
			q.Limit = 20
		}
		page, err := s.BehaviorRoles(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/behavior-patterns", func(w http.ResponseWriter, r *http.Request) {
		for key, values := range r.URL.Query() {
			if len(values) != 1 || (key != "cursor" && key != "limit" && key != "machineId" && key != "provider" && key != "project" && key != "q" && key != "archived") {
				fail(w, store.ErrInvalid)
				return
			}
		}
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if q.Limit == 0 {
			q.Limit = 20
		}
		page, err := s.BehaviorPatterns(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/unsaved-candidates", func(w http.ResponseWriter, r *http.Request) {
		for key, values := range r.URL.Query() {
			if len(values) != 1 || (key != "machineId" && key != "cursor" && key != "limit") {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit := 20
		if raw, ok := r.URL.Query()["limit"]; ok {
			var err error
			limit, err = strconv.Atoi(raw[0])
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := s.UnsavedCandidates(r.Context(), r.URL.Query().Get("machineId"), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/trouble-files", func(w http.ResponseWriter, r *http.Request) {
		for key, values := range r.URL.Query() {
			if (key != "cursor" && key != "limit" && key != "sort" && key != "direction") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if !r.URL.Query().Has("limit") {
			q.Limit = 20
		}
		sort, direction := r.URL.Query().Get("sort"), r.URL.Query().Get("direction")
		if sort == "" {
			sort = "sessions"
		}
		if direction == "" {
			direction = "desc"
		}
		page, err := s.TroubleFilesSorted(r.Context(), q.Cursor, q.Limit, time.Now().UTC(), sort, direction)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/trouble-files/sessions", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if (key != "cursor" && key != "limit" && key != "machineId" && key != "project" && key != "path") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if !v.Has("limit") {
			q.Limit = 25
		}
		page, err := s.TroubleFileSessions(r.Context(), v.Get("machineId"), v.Get("project"), v.Get("path"), q.Cursor, q.Limit, time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/agent-lifecycle", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		for key, values := range v {
			if (key != "agentId" && key != "snapshot") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		value, err := s.AgentLifecycle(r.Context(), r.PathValue("id"), v.Get("agentId"), v.Get("snapshot"), time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/agent-tools", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		if !r.URL.Query().Has("agentId") {
			fail(w, store.ErrInvalid)
			return
		}
		value, err := s.AgentTools(r.Context(), r.PathValue("id"), r.URL.Query().Get("agentId"), q.Cursor, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/models", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.SessionModelUsage(r.Context(), r.PathValue("id"), q.Cursor, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/lineage", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSessionQuery(r)
		if err != nil {
			fail(w, err)
			return
		}
		value, err := s.SessionLineage(r.Context(), r.PathValue("id"), q.Cursor, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/tool-spans", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		after, limit := int64(0), 100
		var err error
		if v.Get("after") != "" {
			after, err = strconv.ParseInt(v.Get("after"), 10, 64)
		}
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		if v.Get("limit") != "" {
			limit, err = strconv.Atoi(v.Get("limit"))
		}
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		page, err := s.ToolSpans(r.Context(), r.PathValue("id"), after, limit, v.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/events/around", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		anchor, err := strconv.ParseInt(v.Get("sequence"), 10, 64)
		if err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		before, after := 50, 50
		if v.Get("before") != "" {
			before, err = strconv.Atoi(v.Get("before"))
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if v.Get("after") != "" {
			after, err = strconv.Atoi(v.Get("after"))
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		value, err := s.EventsAroundPinned(r.Context(), r.PathValue("id"), anchor, before, after, v.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, value)
	})
}
