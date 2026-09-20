// Package hub exposes separate network-ingestion and owner-only query surfaces.
// It intentionally contains no repository, guidance-file or Git write handlers.
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	queries "github.com/evanchakrin/agent-mission-control/internal/query"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type Hub struct {
	Store           *store.Store
	Indexer         *indexer.Indexer
	Token           string
	Version         string
	HubID           string
	Started         time.Time
	StorageStatus   func() any
	LedgerStatus    func() LedgerObservation
	RuntimeStatus   func() any
	EconomicsStatus func() any
	HealthStall     func(string, time.Duration)
	capacity        chan struct{}
	updates         updateChecker
}

func New(s *store.Store, token, version string) *Hub {
	return &Hub{Store: s, Token: token, Version: version, Started: time.Now(), capacity: make(chan struct{}, 16)}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	code, status, retry := "internal_error", http.StatusInternalServerError, true
	message := "The operation could not complete; durable state has been retained."
	switch {
	case errors.Is(err, store.ErrHistoryChanged):
		code, status, retry = "history_changed", 409, false
		message = err.Error()
	case errors.Is(err, store.ErrConflict):
		code, status, retry = "conflict", 409, false
		message = err.Error()
	case errors.Is(err, store.ErrNotFound):
		code, status, retry = "not_found", 404, false
		message = "Record not found"
	case errors.Is(err, store.ErrInvalid):
		code, status, retry = "invalid_request", 400, false
		message = err.Error()
	case errors.Is(err, store.ErrCapacity):
		code, status, retry = "storage_blocked", 507, true
		message = "Storage reserve reached; pending data has not been acknowledged"
	case errors.Is(err, context.DeadlineExceeded):
		code, status, retry = "deadline", 503, true
		message = "Request deadline exceeded; retry safely using the same operation ID"
	case errors.Is(err, store.ErrWriterQueueFull):
		code, status, retry = "writer_busy", 503, true
		message = "Writer queue is full; retry safely using the same operation ID"
	}
	if retry {
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, status, protocol.APIError{Code: code, Error: message, Retryable: retry})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("%w: JSON body", store.ErrInvalid)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", store.ErrInvalid)
	}
	return nil
}
func (h *Hub) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || len(h.Token) < 24 || subtle.ConstantTimeCompare([]byte(supplied), []byte(h.Token)) != 1 {
			writeJSON(w, 401, protocol.APIError{Code: "authentication_blocked", Error: "Collector credentials were not accepted", Retryable: false})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) IngestionHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/ingest/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var beat protocol.Heartbeat
		if err := decode(w, r, &beat); err != nil {
			fail(w, err)
			return
		}
		if err := h.Store.RecordHeartbeat(ctx, beat); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("X-AMC-Recovery-Epoch", h.Store.RecoveryEpoch())
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v2/ingest/reconcile", func(w http.ResponseWriter, r *http.Request) {
		var source protocol.Source
		if err := decode(w, r, &source); err != nil {
			fail(w, err)
			return
		}
		state, err := h.Store.SourceState(r.Context(), source.SourceID, source.Generation)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, 200, protocol.Receipt{SourceID: source.SourceID, Generation: source.Generation, RecoveryEpoch: h.Store.RecoveryEpoch()})
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		if state.Source.MachineID != source.MachineID {
			fail(w, store.ErrConflict)
			return
		}
		writeJSON(w, 200, protocol.Receipt{SourceID: source.SourceID, Generation: source.Generation, DurableOffset: state.DurableOffset, IndexedOffset: state.IndexedOffset, RecoveryEpoch: h.Store.RecoveryEpoch()})
	})
	mux.HandleFunc("POST /v2/ingest/chunks", func(w http.ResponseWriter, r *http.Request) {
		select {
		case h.capacity <- struct{}{}:
			defer func() { <-h.capacity }()
		default:
			w.Header().Set("Retry-After", "2")
			writeJSON(w, 503, protocol.APIError{Code: "busy", Error: "Ingestion queue is full; retry this chunk", Retryable: true})
			return
		}
		header := r.Header.Get("X-AMC-Chunk")
		if len(header) > 16384 {
			fail(w, store.ErrInvalid)
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(header)
		var chunk protocol.Chunk
		if err != nil || json.Unmarshal(b, &chunk) != nil {
			fail(w, store.ErrInvalid)
			return
		}
		if r.ContentLength > protocol.MaxChunkBytes {
			fail(w, store.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxChunkBytes)
		defer r.Body.Close()
		receipt, err := h.Store.IngestChunk(ctx, chunk, r.Body)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, receipt)
	})
	return h.authenticate(mux)
}

func query(r *http.Request) (store.SessionQuery, error) {
	return queries.ParseSessionQuery(r)
}
func (h *Hub) OwnerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/update-check", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, h.updates.check(r.Context(), h.Version))
	})
	mux.HandleFunc("GET /api/v2/run-activity", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		page, err := h.Store.RecentRunActivity(ctx, time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
	})
	h.accountingRoutes(mux)
	h.legacyOrganizationRoutes(mux)
	queries.Register(mux, h.Store)
	mux.HandleFunc("POST /api/v2/sessions/{id}/rebuild", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationID string `json:"operationId"`
		}
		if err := decode(w, r, &request); err != nil {
			fail(w, err)
			return
		}
		job, err := h.Store.BeginRebuild(r.Context(), r.PathValue("id"), request.OperationID, parser.Version)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	})
	mux.HandleFunc("GET /api/v2/rebuilds/{revision}", func(w http.ResponseWriter, r *http.Request) {
		job, err := h.Store.ProjectionRevision(r.Context(), r.PathValue("revision"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, job)
	})
	mux.HandleFunc("GET /api/v2/indexing/versions", func(w http.ResponseWriter, r *http.Request) {
		if h.Indexer == nil {
			fail(w, store.ErrNotFound)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page, err := h.Indexer.CheckVersions(r.Context(), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
	})
	mux.HandleFunc("GET /api/v2/health", func(w http.ResponseWriter, r *http.Request) {
		stage, stopWatch := watchHealth(h.HealthStall, time.Second)
		defer stopWatch()
		status := map[string]any{"version": h.Version, "hubId": h.HubID, "process": "running", "startedAt": h.Started, "recoveryEpoch": h.Store.RecoveryEpoch(), "authoritative": false}
		if h.LedgerStatus != nil {
			stage("ledger-observation")
			observation := h.LedgerStatus()
			status["ledgerObservation"] = observation
			if observation.Diagnostics != nil {
				status["ledger"] = observation.Diagnostics
			}
			if observation.State != "ready" {
				status["ledgerError"] = observation.Problem
			}
		} else {
			// Direct diagnostic use for embedded callers without a service monitor.
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if diagnostics, err := h.Store.DiagnosticsWithStage(ctx, stage); err == nil {
				status["ledger"] = diagnostics
			} else {
				status["ledgerError"] = "Database diagnostics unavailable"
			}
		}
		if h.StorageStatus != nil {
			stage("storage")
			status["storage"] = h.StorageStatus()
		}
		if h.RuntimeStatus != nil {
			stage("runtime")
			status["runtime"] = h.RuntimeStatus()
		}
		if h.EconomicsStatus != nil {
			stage("economics-history")
			status["economicsHistory"] = h.EconomicsStatus()
		}
		if h.Indexer != nil {
			stage("indexing")
			status["indexing"] = h.Indexer.Status()
		}
		stage("response")
		writeJSON(w, 200, status)
	})
	mux.HandleFunc("GET /api/v2/sessions", func(w http.ResponseWriter, r *http.Request) {
		stage, stopWatch := watchHealth(h.HealthStall, time.Second)
		defer stopWatch()
		stage("sessions-query")
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := h.Store.ListSessions(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		stage("sessions-response")
		rows, err := h.Store.SessionResponses(r.Context(), page.Sessions)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, struct {
			Sessions   []store.SessionResponse `json:"sessions"`
			NextCursor string                  `json:"nextCursor,omitempty"`
		}{rows, page.NextCursor})
	})
	mux.HandleFunc("GET /api/v2/totals", func(w http.ResponseWriter, r *http.Request) {
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		totals, err := h.Store.SessionTotals(r.Context(), q)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, totals)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := h.Store.GetSession(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		rows, err := h.Store.SessionResponses(r.Context(), []store.Session{s})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, rows[0])
	})
	mux.HandleFunc("GET /api/v2/projects", func(w http.ResponseWriter, r *http.Request) {
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		page, err := h.Store.Projects(r.Context(), q.Cursor, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
	})
	mux.HandleFunc("POST /api/v2/projects", func(w http.ResponseWriter, r *http.Request) {
		var mutation store.ProjectMutation
		if err := decode(w, r, &mutation); err != nil {
			fail(w, err)
			return
		}
		if mutation.Delete && r.Header.Get("Prefer") == "respond-async" {
			job, err := h.Store.QueueProjectDeletion(r.Context(), mutation)
			if err != nil {
				fail(w, err)
				return
			}
			w.Header().Set("Preference-Applied", "respond-async")
			status := http.StatusAccepted
			if job.State == "complete" {
				status = http.StatusOK
			}
			writeJSON(w, status, job)
			return
		}
		project, err := h.Store.MutateProject(r.Context(), mutation)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, project)
	})
	mux.HandleFunc("GET /api/v2/project-deletions/{operation}", func(w http.ResponseWriter, r *http.Request) {
		job, err := h.Store.ProjectDeletion(r.Context(), r.PathValue("operation"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("GET /api/v2/projects/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		var after int64
		if r.URL.Query().Has("after") {
			after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if err != nil || after < 0 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := h.Store.ProjectHistory(r.Context(), r.PathValue("id"), after, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
	})
	mux.HandleFunc("PATCH /api/v2/sessions/{id}/organization", func(w http.ResponseWriter, r *http.Request) {
		stage, stopWatch := watchHealth(h.HealthStall, 250*time.Millisecond)
		defer stopWatch()
		stage("organization-decode")
		var patch store.MetadataPatch
		if err := decode(w, r, &patch); err != nil {
			fail(w, err)
			return
		}
		value, err := h.Store.PatchMetadataWithStage(r.Context(), r.PathValue("id"), patch, stage)
		if err != nil {
			fail(w, err)
			return
		}
		stage("organization-response")
		writeJSON(w, 200, value)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/organization/history", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		p, err := h.Store.OrganizationHistory(r.Context(), r.PathValue("id"), after, limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var p store.EventPage
		var err error
		if r.URL.Query().Has("agentId") {
			p, err = h.Store.ListAgentEventsPinned(r.Context(), r.PathValue("id"), r.URL.Query().Get("agentId"), after, limit, r.URL.Query().Get("snapshot"))
		} else {
			p, err = h.Store.ListEventsPinned(r.Context(), r.PathValue("id"), after, limit, r.URL.Query().Get("snapshot"))
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("GET /api/v2/search", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		var after int64
		if raw := v.Get("after"); raw != "" {
			var err error
			after, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || after < 0 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		limit, _ := strconv.Atoi(v.Get("limit"))
		delegated := v.Get("delegatedOnly")
		if delegated != "" && delegated != "true" && delegated != "false" {
			fail(w, store.ErrInvalid)
			return
		}
		mode := v.Get("mode")
		if mode != "" && mode != "related" {
			fail(w, store.ErrInvalid)
			return
		}
		p, err := h.Store.SearchPinned(r.Context(), store.SearchQuery{Text: v.Get("q"), DelegatedOnly: delegated == "true", Related: mode == "related", SessionID: v.Get("sessionId"), AfterSequence: after, Limit: limit}, v.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("GET /api/v2/machines/{id}/sources", func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := h.Store.SearchSourceCatalog(r.Context(), r.PathValue("id"), r.URL.Query().Get("cursor"), limit, r.URL.Query().Get("q"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v2/sources/{source}/{generation}/raw", func(w http.ResponseWriter, r *http.Request) {
		if h.Store.StatsOnly() {
			http.Error(w, "raw history is not retained in stats-only mode", http.StatusGone)
			return
		}
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		raw, err := h.Store.OpenSource(r.Context(), r.PathValue("source"), r.PathValue("generation"), offset)
		if err != nil {
			fail(w, err)
			return
		}
		defer raw.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=source.jsonl")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, raw)
	})
	mux.HandleFunc("GET /api/v2/machines/{id}/label", func(w http.ResponseWriter, r *http.Request) {
		label, err := h.Store.MachineLabel(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, label)
	})
	mux.HandleFunc("GET /api/v2/machines/{id}/label/history", func(w http.ResponseWriter, r *http.Request) {
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		var after int64
		if r.URL.Query().Has("after") {
			after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if err != nil || after < 0 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		page, err := h.Store.MachineLabelHistory(r.Context(), r.PathValue("id"), after, q.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("POST /api/v2/machines/{id}/label", func(w http.ResponseWriter, r *http.Request) {
		var mutation store.MachineLabelMutation
		if err := decode(w, r, &mutation); err != nil {
			fail(w, err)
			return
		}
		if mutation.MachineID != r.PathValue("id") {
			fail(w, store.ErrInvalid)
			return
		}
		label, err := h.Store.MutateMachineLabel(r.Context(), mutation)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, label)
	})
	mux.HandleFunc("GET /api/v2/machines", func(w http.ResponseWriter, r *http.Request) {
		machines, err := h.Store.ListMachines(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		var out []map[string]any
		for _, m := range machines {
			age := time.Since(m.LastSeen)
			connection := "connected"
			if age > 180*time.Second {
				connection = "disconnected"
			} else if age > 90*time.Second {
				connection = "delayed"
			}
			out = append(out, map[string]any{"machine": m, "connection": connection, "connectionAgeSeconds": int64(age.Seconds())})
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /api/v2/changes", h.changes)
	mux.HandleFunc("GET /api/v2/changes/head", func(w http.ResponseWriter, r *http.Request) {
		head, err := h.Store.ChangeHead(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"sequence": head, "hubId": h.HubID, "recoveryEpoch": h.Store.RecoveryEpoch()})
	})
	capacity := make(chan struct{}, 64)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2/analytics/totals" {
			stage, stop := watchHealth(h.HealthStall, time.Second)
			defer stop()
			stage("totals-query")
		}
		select {
		case capacity <- struct{}{}:
			defer func() { <-capacity }()
		default:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 503, protocol.APIError{Code: "busy", Error: "Query queue is full", Retryable: true})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/export.ndjson") {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
			defer cancel()
			r = r.WithContext(ctx)
		} else if r.URL.Path != "/api/v2/changes" && !strings.HasSuffix(r.URL.Path, "/raw") {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *Hub) changes(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		after, _ = strconv.ParseInt(last, 10, 64)
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		changes, err := h.Store.Changes(r.Context(), after, 100)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, changes)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, errors.New("streaming unavailable"))
		return
	}
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	life := time.NewTimer(25 * time.Minute)
	defer life.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-life.C:
			return
		case <-tick.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			rows, err := h.Store.Changes(r.Context(), after, 100)
			if err != nil {
				return
			}
			for _, row := range rows {
				b, _ := json.Marshal(row)
				if _, err = fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", row.Sequence, b); err != nil {
					return
				}
				after = row.Sequence
			}
			if len(rows) == 0 {
				if _, err = io.WriteString(w, ": heartbeat\n\n"); err != nil {
					return
				}
			}
			if err = controller.Flush(); err != nil {
				return
			}
		}
	}
}

func Server(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 24 * 1024}
}
