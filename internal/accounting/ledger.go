package accounting

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"sort"
	"time"
)

type Catalog struct {
	ID    string `json:"id"`
	Rates []Rate `json:"rates"`
	// Observed current rates are not evidence of past prices or billing context.
	ComparisonOnly bool `json:"comparisonOnly,omitempty"`
}

func validateCatalogUse(c Catalog, comparisonAt *time.Time) error {
	if c.ComparisonOnly && comparisonAt == nil {
		return fmt.Errorf("%w: this rate catalog requires an explicit current-rate comparison date", store.ErrInvalid)
	}
	return Validate(c.Rates)
}

type Snapshot struct {
	ID                 string     `json:"id"`
	SessionID          string     `json:"sessionId"`
	Generation         string     `json:"generation"`
	ProjectionRevision string     `json:"projectionRevision"`
	IndexedOffset      int64      `json:"indexedOffset"`
	CatalogID          string     `json:"catalogId"`
	Context            string     `json:"context"`
	ComparisonAt       *time.Time `json:"comparisonAt,omitempty"`
	Estimate           Estimate   `json:"estimate"`
	AttributionVersion int        `json:"attributionVersion,omitempty"`
	Observations       int64      `json:"observations,omitempty"`
}

func SaveCatalog(ctx context.Context, s *store.Store, c Catalog) error {
	if err := Validate(c.Rates); err != nil {
		return fmt.Errorf("%w: %s", store.ErrInvalid, err)
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.PutRateCatalog(ctx, c.ID, b)
}
func Reprice(ctx context.Context, s *store.Store, sessionID, catalogID, billingContext string, comparisonAt *time.Time) (Snapshot, error) {
	return reprice(ctx, s, sessionID, catalogID, billingContext, comparisonAt, nil)
}

func reprice(ctx context.Context, s *store.Store, sessionID, catalogID, billingContext string, comparisonAt *time.Time, job *store.PricingJob) (Snapshot, error) {
	var result Snapshot
	if err := s.InitializeAccounting(ctx); err != nil {
		return result, err
	}
	raw, err := s.RateCatalog(ctx, catalogID)
	if err != nil {
		return result, err
	}
	var catalog Catalog
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return result, err
	}
	if err = validateCatalogUse(catalog, comparisonAt); err != nil {
		return result, err
	}
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return result, err
	}
	before, err := s.SourceState(ctx, session.SourceID, session.Generation)
	if err != nil {
		return result, err
	}
	result = Snapshot{SessionID: sessionID, Generation: session.Generation, ProjectionRevision: session.ProjectionRevision, IndexedOffset: before.IndexedOffset, CatalogID: catalogID, Context: billingContext, ComparisonAt: comparisonAt}
	at := "historical"
	if comparisonAt != nil {
		at = comparisonAt.UTC().Format(time.RFC3339Nano)
	}
	result.AttributionVersion = 1
	result.ID = parser.ID(sessionID, session.Generation, session.ProjectionRevision, fmt.Sprint(before.IndexedOffset), catalogID, billingContext, at, "attribution-v1", "components-v1")
	afterID := ""
	rateIDs := map[string]bool{}
	for {
		rows, err := s.GetUsage(ctx, sessionID, afterID, 500)
		if err != nil {
			return result, err
		}
		page := Price(rows, catalog.Rates, billingContext, comparisonAt)
		evidence := make([]store.ObservationPrice, 0, len(rows))
		for _, u := range rows {
			p := Price([]store.UsageObservation{u}, catalog.Rates, billingContext, comparisonAt)
			evidence = append(evidence, store.ObservationPrice{Components: p.Components, ObservationID: u.ID, AgentID: u.AgentID, Model: u.Model, Cost: p.Cost, RecordedTokens: p.RecordedTokens, PricedTokens: p.PricedTokens, UnpricedTokens: p.UnpricedTokens, UnattributedTokens: p.UnattributedTokens, RateIDs: p.RateIDs})
		}
		if err = s.StageObservationPrices(ctx, result.ID, evidence); err != nil {
			return result, err
		}
		result.Observations += int64(len(rows))
		result.Estimate.Cost += page.Cost
		addComponents(&result.Estimate, page.Components)
		result.Estimate.RecordedTokens += page.RecordedTokens
		result.Estimate.PricedTokens += page.PricedTokens
		result.Estimate.UnpricedTokens += page.UnpricedTokens
		result.Estimate.UnattributedTokens += page.UnattributedTokens
		for _, id := range page.RateIDs {
			rateIDs[id] = true
		}
		if len(rows) < 500 {
			break
		}
		afterID = rows[len(rows)-1].ID
	}
	// Short page transactions are optimistic. If indexing changed evidence during
	// pricing, do not publish a snapshot assembled from different ledger versions.
	after, err := s.SourceState(ctx, session.SourceID, session.Generation)
	if err != nil {
		return result, err
	}
	current, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return result, err
	}
	if after.IndexedOffset != before.IndexedOffset || current.Generation != session.Generation || current.ProjectionRevision != session.ProjectionRevision {
		return result, fmt.Errorf("%w: indexing changed during pricing; retry", store.ErrConflict)
	}
	if result.Estimate.RecordedTokens > 0 {
		result.Estimate.Coverage = float64(result.Estimate.PricedTokens) / float64(result.Estimate.RecordedTokens)
	}
	for id := range rateIDs {
		result.Estimate.RateIDs = append(result.Estimate.RateIDs, id)
	}
	sort.Strings(result.Estimate.RateIDs)
	b, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	return result, s.SaveProjectionEstimateForJob(ctx, result.ID, sessionID, session.Generation, session.ProjectionRevision, before.IndexedOffset, b, job)
}
