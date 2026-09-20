package hub

import (
	"context"
	"errors"
	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"net/http"
	"strconv"
	"time"
)

func (h *Hub) accountingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v2/pricing-default", func(w http.ResponseWriter, r *http.Request) {
		p, err := h.Store.ComparisonDefault(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("PUT /api/v2/pricing-default", func(w http.ResponseWriter, r *http.Request) {
		var p store.ComparisonDefault
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		if err := accounting.EnableComparisonDefault(r.Context(), h.Store, p); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"state": "enabled-for-new-sessions"})
	})
	mux.HandleFunc("DELETE /api/v2/pricing-default", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.SetComparisonDefault(r.Context(), nil); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v2/sessions/{id}/reconcile-inheritance", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ParentSessionID string `json:"parentSessionId"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, err)
			return
		}
		if body.ParentSessionID == "" {
			fail(w, store.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		result, err := accounting.StageForkBaseline(ctx, h.Store, r.PathValue("id"), body.ParentSessionID, 200)
		if errors.Is(err, accounting.ErrInheritanceScanIncomplete) {
			writeJSON(w, http.StatusOK, map[string]any{"selected": false, "inspection": accounting.ForkBaselineInspection{State: "unverified", Reason: err.Error(), Pages: result.Inspection.Pages}})
			return
		}
		if err == nil && result.ProofID != "" {
			err = accounting.SelectForkBaseline(ctx, h.Store, result.ProofID)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"selected": result.ProofID != "", "proofId": result.ProofID, "inspection": result.Inspection})
	})
	mux.HandleFunc("POST /api/v2/sessions/{id}/reconcile-duplicates", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OtherSessionID string `json:"otherSessionId"`
			Cursor         string `json:"cursor"`
			Snapshot       string `json:"snapshot"`
			Limit          int    `json:"limit"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, err)
			return
		}
		if body.Limit == 0 {
			body.Limit = 20
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		page, err := accounting.ReconcileDuplicateUsagePage(ctx, h.Store, r.PathValue("id"), body.OtherSessionID, body.Cursor, body.Snapshot, body.Limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v2/contribution-totals", func(w http.ResponseWriter, r *http.Request) {
		q, err := query(r)
		if err != nil {
			fail(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		totals, err := h.Store.ContributionTotals(ctx, q)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, totals)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/contributions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 100
		for key, values := range q {
			if (key != "cursor" && key != "snapshot" && key != "limit") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if q.Has("limit") {
			var err error
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		page, err := h.Store.UsageContributions(ctx, r.PathValue("id"), q.Get("cursor"), limit, q.Get("snapshot"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/inheritance", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if len(q) != 0 && (len(q) != 1 || len(q["parent"]) != 1 || q.Get("parent") == "") {
			fail(w, store.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		parentID := q.Get("parent")
		var result accounting.ForkBaselineInspection
		var err error
		if parentID == "" {
			result, err = accounting.InspectCapturedForkBaseline(ctx, h.Store, r.PathValue("id"), 200)
		} else {
			result, err = accounting.InspectForkBaseline(ctx, h.Store, r.PathValue("id"), parentID, 200)
		}
		if errors.Is(err, accounting.ErrInheritanceScanIncomplete) {
			result = accounting.ForkBaselineInspection{State: "unverified", Reason: err.Error(), Pages: result.Pages}
		} else if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/capture-resolutions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 100
		for key, values := range q {
			if (key != "cursor" && key != "limit") || len(values) != 1 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		if q.Has("limit") {
			var err error
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				fail(w, store.ErrInvalid)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		page, err := h.Store.EconomicsCaptureResolutions(ctx, q.Get("cursor"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
	})
	mux.HandleFunc("POST /api/v2/analytics/economics/capture-resolutions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationID   string `json:"operationId"`
			OriginalEpoch string `json:"originalEpoch"`
			RecoveryEpoch string `json:"recoveryEpoch"`
		}
		if err := decode(w, r, &request); err != nil {
			fail(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		resolution, err := h.Store.ResolveEconomicsCapture(ctx, request.OperationID, request.OriginalEpoch, request.RecoveryEpoch)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, resolution)
	})
	mux.HandleFunc("GET /api/v2/analytics/economics/capture-resolutions/{id}", func(w http.ResponseWriter, r *http.Request) {
		resolution, err := h.Store.EconomicsCaptureResolution(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, resolution)
	})
	mux.HandleFunc("POST /api/v2/analytics/economics/history", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationID   string `json:"operationId"`
			RecoveryEpoch string `json:"recoveryEpoch"`
		}
		if err := decode(w, r, &request); err != nil {
			fail(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		entry, err := h.Store.CaptureEconomicsAtEpoch(ctx, request.OperationID, request.RecoveryEpoch)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, entry)
	})
	mux.HandleFunc("GET /api/v2/rate-catalogs", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.InitializeAccounting(r.Context()); err != nil {
			fail(w, err)
			return
		}
		ids, err := h.Store.RateCatalogIDs(r.Context(), r.URL.Query().Get("after"))
		if err != nil {
			fail(w, err)
			return
		}
		next := ""
		if len(ids) > 100 {
			ids = ids[:100]
			next = ids[99]
		}
		writeJSON(w, 200, map[string]any{"ids": ids, "next": next})
	})
	mux.HandleFunc("GET /api/v2/rate-catalogs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.InitializeAccounting(r.Context()); err != nil {
			fail(w, err)
			return
		}
		c, err := h.Store.RateCatalog(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/pricing-policy", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.InitializePricingQueue(r.Context()); err != nil {
			fail(w, err)
			return
		}
		p, err := h.Store.PricingPolicy(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("DELETE /api/v2/sessions/{id}/pricing-policy", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.InitializePricingQueue(r.Context()); err != nil {
			fail(w, err)
			return
		}
		if err := h.Store.DisablePricingPolicy(r.Context(), r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/v2/sessions/{id}/pricing-policy", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			CatalogID    string     `json:"catalogId"`
			Context      string     `json:"context"`
			ComparisonAt *time.Time `json:"comparisonAt"`
		}
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		if err := accounting.EnableAutomaticAt(r.Context(), h.Store, r.PathValue("id"), p.CatalogID, p.Context, p.ComparisonAt); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "queued"})
	})
	mux.HandleFunc("POST /api/v2/rate-catalogs", func(w http.ResponseWriter, r *http.Request) {
		var c accounting.Catalog
		if err := decode(w, r, &c); err != nil {
			fail(w, err)
			return
		}
		if err := accounting.SaveCatalog(r.Context(), h.Store, c); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]string{"catalogId": c.ID})
	})
	mux.HandleFunc("POST /api/v2/sessions/{id}/pricing", func(w http.ResponseWriter, r *http.Request) {
		var options struct {
			CatalogID    string     `json:"catalogId"`
			Context      string     `json:"context"`
			ComparisonAt *time.Time `json:"comparisonAt"`
		}
		if err := decode(w, r, &options); err != nil {
			fail(w, err)
			return
		}
		if options.CatalogID == "" || options.Context == "" {
			fail(w, store.ErrInvalid)
			return
		}
		result, err := accounting.Reprice(r.Context(), h.Store, r.PathValue("id"), options.CatalogID, options.Context, options.ComparisonAt)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("GET /api/v2/sessions/{id}/pricing", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Store.InitializeAccounting(r.Context()); err != nil {
			fail(w, err)
			return
		}
		rows, err := h.Store.EstimateHistory(r.Context(), r.PathValue("id"), r.URL.Query().Get("after"), 100)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, rows)
	})
}
