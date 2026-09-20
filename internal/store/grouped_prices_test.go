package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGroupedUsagePricesOnlySelectedPublishedEvidence(t *testing.T) {
	s, _ := publishedRevisionFixture(t)
	ctx := context.Background()
	usage, err := s.GetUsage(ctx, "session-1", "", 10)
	if err != nil || len(usage) != 1 {
		t.Fatalf("usage: %+v %v", usage, err)
	}
	u := usage[0]
	session, err := s.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	check := func(priced int64, cost *float64) {
		t.Helper()
		for _, dimension := range []string{"model", "agentKind", "machine", "provider", "project", "day", "week", "month", "year"} {
			page, e := s.GroupedUsage(ctx, GroupQuery{Dimension: dimension})
			if e != nil || len(page.Groups) != 1 {
				t.Fatalf("%s: %+v %v", dimension, page, e)
			}
			g := page.Groups[0]
			if g.PricedTokens != priced || g.UnpricedTokens != 13-priced || g.TokensIn+g.TokensCache+g.TokensCacheWrite+g.TokensOut != 13 || (cost == nil) != (g.CostEstimate == nil) || cost != nil && *g.CostEstimate != *cost {
				t.Fatalf("%s: %+v", dimension, g)
			}
			if priced == 6 && g.PricingCoverage != "partially-priced" {
				t.Fatalf("coverage: %+v", g)
			}
		}
	}
	check(0, nil)
	for _, entry := range []struct {
		id     string
		cost   float64
		priced int64
	}{{"partial", 1, 6}, {"free", 0, 13}, {"new-rate", 2, 13}} {
		row := ObservationPrice{ObservationID: u.ID, AgentID: u.AgentID, Model: u.Model, Cost: entry.cost, RecordedTokens: 13, PricedTokens: entry.priced, UnpricedTokens: 13 - entry.priced}
		if err = s.StageObservationPrices(ctx, entry.id, []ObservationPrice{row}); err != nil {
			t.Fatal(err)
		}
		if entry.id == "partial" {
			check(0, nil)
		}
		if entry.id == "free" {
			before := 1.0
			check(6, &before)
		}
		if entry.id == "new-rate" {
			before := 0.0
			check(13, &before)
		}
		payload, _ := json.Marshal(map[string]any{"id": entry.id, "sessionId": session.ID, "generation": session.Generation, "projectionRevision": session.ProjectionRevision, "indexedOffset": 13, "catalogId": "fixture", "context": "test", "attributionVersion": 1, "observations": 1, "estimate": map[string]any{"cost": entry.cost, "recordedTokens": 13, "pricedTokens": entry.priced, "unpricedTokens": 13 - entry.priced, "unattributedTokens": 0}})
		if err = s.SaveProjectionEstimate(ctx, entry.id, session.ID, session.Generation, session.ProjectionRevision, 13, payload); err != nil {
			t.Fatal(err)
		}
		check(entry.priced, &entry.cost)
	}
	// Historical aggregate-only estimates cannot be distributed across models.
	if _, err = s.db.Exec(`UPDATE sessions SET projection=json_remove(projection,'$.pricing') WHERE id='session-1'`); err != nil {
		t.Fatal(err)
	}
	check(0, nil)
}
