package accounting

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"math"
	"sort"
	"time"
)

type pricingCheckpoint struct {
	Version  int      `json:"version"`
	After    string   `json:"after"`
	Snapshot Snapshot `json:"snapshot"`
}

func priceBatch(ctx context.Context, s *store.Store, j store.PricingJob) (bool, error) {
	session, err := s.GetSession(ctx, j.SessionID)
	if err != nil {
		return false, err
	}
	source, err := s.SourceState(ctx, session.SourceID, session.Generation)
	if err != nil {
		return false, err
	}
	fresh := pricingCheckpoint{Version: 3, Snapshot: Snapshot{SessionID: j.SessionID, Generation: session.Generation, ProjectionRevision: session.ProjectionRevision, IndexedOffset: source.IndexedOffset, CatalogID: j.CatalogID, Context: j.Context, AttributionVersion: 1}}
	fresh.Snapshot.ComparisonAt = j.ComparisonAt
	pricingAt := "historical"
	if j.ComparisonAt != nil {
		pricingAt = j.ComparisonAt.UTC().Format(time.RFC3339Nano)
	}
	fresh.Snapshot.ID = parser.ID(j.SessionID, session.Generation, session.ProjectionRevision, fmt.Sprint(source.IndexedOffset), j.CatalogID, j.Context, pricingAt, "attribution-v1", "components-v1")
	c := fresh
	if len(j.Checkpoint) > 0 {
		c = pricingCheckpoint{}
		if err = json.Unmarshal(j.Checkpoint, &c); err != nil {
			return false, fmt.Errorf("damaged pricing checkpoint: %w", err)
		}
		p := c.Snapshot
		if (c.Version != 1 && c.Version != 2 && c.Version != 3) || c.After == "" || len(c.After) > 1024 || p.SessionID != j.SessionID || p.CatalogID != j.CatalogID || p.Context != j.Context || !sameComparison(p.ComparisonAt, j.ComparisonAt) || !validPricingProgress(p.Estimate) {
			return false, fmt.Errorf("damaged pricing checkpoint identity")
		}
		if p.Generation != session.Generation || p.ProjectionRevision != session.ProjectionRevision || p.IndexedOffset != source.IndexedOffset {
			return false, store.ErrConflict
		}
		if c.Version < 3 {
			if c.Version == 2 && (p.ID != parser.ID(j.SessionID, session.Generation, session.ProjectionRevision, fmt.Sprint(source.IndexedOffset), j.CatalogID, j.Context, "historical", "attribution-v1") || p.AttributionVersion != 1 || p.Observations < 0) {
				return false, fmt.Errorf("damaged legacy attribution checkpoint")
			}
			c = fresh
		} else if p.ID != fresh.Snapshot.ID || p.AttributionVersion != 1 || p.Observations < 0 {
			return false, fmt.Errorf("damaged attribution checkpoint")
		}
	}
	raw, err := s.RateCatalog(ctx, j.CatalogID)
	if err != nil {
		return false, err
	}
	var catalog Catalog
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return false, err
	}
	if err = validateCatalogUse(catalog, j.ComparisonAt); err != nil {
		return false, err
	}
	rows, err := s.GetUsage(ctx, j.SessionID, c.After, 500)
	if err != nil {
		return false, err
	}
	p := Price(rows, catalog.Rates, j.Context, j.ComparisonAt)
	evidence := make([]store.ObservationPrice, 0, len(rows))
	for _, u := range rows {
		item := Price([]store.UsageObservation{u}, catalog.Rates, j.Context, j.ComparisonAt)
		evidence = append(evidence, store.ObservationPrice{Components: item.Components, ObservationID: u.ID, AgentID: u.AgentID, Model: u.Model, Cost: item.Cost, RecordedTokens: item.RecordedTokens, PricedTokens: item.PricedTokens, UnpricedTokens: item.UnpricedTokens, UnattributedTokens: item.UnattributedTokens, RateIDs: item.RateIDs})
	}
	if err = s.StageObservationPrices(ctx, c.Snapshot.ID, evidence); err != nil {
		return false, err
	}
	c.Snapshot.Observations += int64(len(rows))
	e := &c.Snapshot.Estimate
	e.Cost += p.Cost
	addComponents(e, p.Components)
	e.RecordedTokens += p.RecordedTokens
	e.PricedTokens += p.PricedTokens
	e.UnpricedTokens += p.UnpricedTokens
	e.UnattributedTokens += p.UnattributedTokens
	if !validPricingProgress(*e) {
		return false, fmt.Errorf("invalid or overflowing pricing totals")
	}
	ids := map[string]bool{}
	for _, id := range e.RateIDs {
		ids[id] = true
	}
	for _, id := range p.RateIDs {
		ids[id] = true
	}
	e.RateIDs = nil
	for id := range ids {
		e.RateIDs = append(e.RateIDs, id)
	}
	sort.Strings(e.RateIDs)
	if len(rows) == 500 {
		c.After = rows[len(rows)-1].ID
		b, err := json.Marshal(c)
		if err != nil {
			return false, err
		}
		return false, s.SavePricingCheckpoint(ctx, j, b)
	}
	if e.RecordedTokens > 0 {
		e.Coverage = float64(e.PricedTokens) / float64(e.RecordedTokens)
	}
	b, err := json.Marshal(c.Snapshot)
	if err != nil {
		return false, err
	}
	return true, s.SaveProjectionEstimateForJob(ctx, c.Snapshot.ID, j.SessionID, session.Generation, session.ProjectionRevision, source.IndexedOffset, b, &j)
}

func sameComparison(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func validPricingProgress(e Estimate) bool {
	if c := e.Components; c != nil {
		if c.PricedTokens != e.PricedTokens {
			return false
		}
		for _, v := range []float64{c.Input, c.CacheRead, c.CacheWrite, c.Output} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return false
			}
		}
		total := c.Input + c.CacheRead + c.CacheWrite + c.Output
		if math.IsInf(total, 0) || math.Abs(total-e.Cost) > 1e-10*math.Max(1, e.Cost) {
			return false
		}
	}
	return e.Cost >= 0 && !math.IsNaN(e.Cost) && !math.IsInf(e.Cost, 0) && e.RecordedTokens >= 0 && e.PricedTokens >= 0 && e.UnpricedTokens >= 0 && e.PricedTokens <= e.RecordedTokens && e.UnpricedTokens == e.RecordedTokens-e.PricedTokens && e.UnattributedTokens >= 0 && e.UnattributedTokens <= e.RecordedTokens
}
