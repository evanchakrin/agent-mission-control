package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEconomicsCostsSeparatePublishedBreakdownsAndMissingHistory(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		id := []string{"s-000000", "s-000001"}[i]
		row, err := s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cost := float64(i + 2)
		row.CostEstimate = &cost
		row.Pricing = &SessionPricing{Cost: cost, RecordedTokens: 4, PricedTokens: 4}
		if i == 0 {
			row.Pricing.Components = &CostComponents{Input: .5, Output: 1.5, PricedTokens: 4}
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("UPDATE sessions SET projection=? WHERE id=?", raw, id); err != nil {
			t.Fatal(err)
		}
	}
	// Pricing work not published to a session must not enter fleet economics.
	if err := s.StageObservationPrices(ctx, "not-published", []ObservationPrice{{ObservationID: "staged", Cost: 99, RecordedTokens: 4, PricedTokens: 4, Components: &CostComponents{Input: 99, PricedTokens: 4}}}); err != nil {
		t.Fatal(err)
	}
	total, err := s.EconomicsCostTotals(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if total.Sessions != 3 || total.RecordedTokens != 12 || total.PricedTokens != 8 || total.UnpricedOrUnmeasuredTokens != 4 || total.PricedTokensWithoutBreakdown != 4 || total.SessionsWithBreakdown != 1 || total.SessionsWithoutBreakdown != 2 || total.KnownCost == nil || *total.KnownCost != 5 || total.Components.Input != .5 || total.Components.Output != 1.5 {
		t.Fatalf("wrong coverage: %+v", total)
	}
	filtered, err := s.EconomicsCostTotals(ctx, SessionQuery{Provider: "claude"})
	if err != nil || filtered.Sessions != 2 || filtered.KnownCost == nil || *filtered.KnownCost != 2 || filtered.PricedTokensWithoutBreakdown != 0 {
		t.Fatalf("filter ignored: %+v %v", filtered, err)
	}
	empty, err := s.EconomicsCostTotals(ctx, SessionQuery{Provider: "otel"})
	if err != nil || empty.Sessions != 0 || empty.KnownCost != nil {
		t.Fatalf("empty cost invented: %+v %v", empty, err)
	}
}

func TestPublicationRejectsComponentTotalsNotSupportedByObservations(t *testing.T) {
	for _, mode := range []string{"valid", "different-split", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := publishedRevisionFixture(t)
			ctx := context.Background()
			u, err := s.GetUsage(ctx, "session-1", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			session, err := s.GetSession(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			c := &CostComponents{Input: .25, Output: .75, PricedTokens: 13}
			if err := s.StageObservationPrices(ctx, "components", []ObservationPrice{{ObservationID: u[0].ID, AgentID: u[0].AgentID, Model: u[0].Model, Cost: 1, RecordedTokens: 13, PricedTokens: 13, Components: c}}); err != nil {
				t.Fatal(err)
			}
			claimed := &CostComponents{Input: .25, Output: .75, PricedTokens: 13}
			if mode == "different-split" {
				claimed.Input, claimed.Output = .5, .5
			}
			if mode == "missing" {
				claimed = nil
			}
			raw, _ := json.Marshal(map[string]any{"id": "components", "sessionId": session.ID, "generation": session.Generation, "projectionRevision": session.ProjectionRevision, "indexedOffset": 13, "catalogId": "catalog", "context": "test", "attributionVersion": 1, "observations": 1, "estimate": SessionPricing{Cost: 1, RecordedTokens: 13, PricedTokens: 13, Components: claimed}})
			err = s.SaveProjectionEstimate(ctx, "components", session.ID, session.Generation, session.ProjectionRevision, 13, raw)
			if mode == "valid" && err != nil || mode != "valid" && err == nil {
				t.Fatalf("%s: %v", mode, err)
			}
		})
	}
}
