package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestObservationPricesImmutableAtomicAndUnpublished(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	r := ObservationPrice{ObservationID: "first", AgentID: "main", Model: "model", Cost: 0.1, RecordedTokens: 10, PricedTokens: 7, UnpricedTokens: 3, UnattributedTokens: 2, RateIDs: []string{"rate"}}
	for range 2 {
		if err := s.StageObservationPrices(ctx, "snapshot", []ObservationPrice{r}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM observation_prices").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM accounting_estimates").Scan(&count); err != nil || count != 0 {
		t.Fatal("staging published a snapshot", count, err)
	}
	next := r
	next.ObservationID = "second"
	conflict := r
	conflict.Cost = 0.2
	if err := s.StageObservationPrices(ctx, "snapshot", []ObservationPrice{next, conflict}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM observation_prices").Scan(&count); err != nil || count != 1 {
		t.Fatal("partial batch committed", count, err)
	}
	dir := s.dir
	s.Close()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StageObservationPrices(ctx, "snapshot", []ObservationPrice{r}); err != nil {
		t.Fatal("retry after reopen", err)
	}
	if err = s.SaveEstimate(ctx, "snapshot", "session", []byte(`{"fixture":true}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.StageObservationPrices(ctx, "snapshot", []ObservationPrice{r}); err != nil {
		t.Fatal("published identical retry", err)
	}
	if err = s.StageObservationPrices(ctx, "snapshot", []ObservationPrice{next}); !errors.Is(err, ErrConflict) {
		t.Fatal("published snapshot gained evidence", err)
	}
}

func TestAttributionPublicationRejectsMissingExtraAndMismatchedRows(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "extra", "agent", "tokens", "cost", "proof-race", "proof-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := publishedRevisionFixture(t)
			ctx := context.Background()
			usage, err := s.GetUsage(ctx, "session-1", "", 10)
			if err != nil || len(usage) != 1 {
				t.Fatal(usage, err)
			}
			session, err := s.GetSession(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			u := usage[0]
			row := ObservationPrice{ObservationID: u.ID, AgentID: u.AgentID, Model: u.Model, Cost: 1, RecordedTokens: 13, PricedTokens: 13}
			rows := []ObservationPrice{row}
			switch mode {
			case "missing":
				rows = nil
			case "extra":
				extra := row
				extra.ObservationID = "extra"
				rows = append(rows, extra)
			case "agent":
				rows[0].AgentID = "invented"
			case "tokens":
				rows[0].RecordedTokens = 12
				rows[0].PricedTokens = 12
			case "cost":
				rows[0].Cost = 2
			}
			if err = s.StageObservationPrices(ctx, "attributed", rows); err != nil {
				t.Fatal(err)
			}
			before, err := s.CurrentAttributionPrices(ctx, session.ID, "agent", []string{u.AgentID})
			if err != nil || len(before) != 0 {
				t.Fatal("staged evidence exposed", before, err)
			}
			payload, _ := json.Marshal(map[string]any{"id": "attributed", "sessionId": session.ID, "generation": session.Generation, "projectionRevision": session.ProjectionRevision, "indexedOffset": 13, "catalogId": "catalog", "context": "test", "attributionVersion": 1, "observations": 1, "estimate": map[string]any{"cost": 1, "recordedTokens": 13, "pricedTokens": 13, "unpricedTokens": 0, "unattributedTokens": 0}})
			if mode == "proof-race" {
				s.options.AfterAttributionVerification = func() error {
					extra := row
					extra.ObservationID = "racing-extra"
					return s.StageObservationPrices(ctx, "attributed", []ObservationPrice{extra})
				}
			}
			injected := errors.New("injected failure after proof")
			if mode == "proof-failure" {
				s.options.AfterAttributionVerification = func() error { return injected }
			}
			err = s.SaveProjectionEstimate(ctx, "attributed", session.ID, session.Generation, session.ProjectionRevision, 13, payload)
			if mode == "proof-race" || mode == "proof-failure" {
				want := error(ErrConflict)
				if mode == "proof-failure" {
					want = injected
				}
				if !errors.Is(err, want) {
					t.Fatal("proof boundary not rejected", err)
				}
				var count int
				if e := s.db.QueryRow("SELECT count(*) FROM accounting_estimates WHERE id='attributed'").Scan(&count); e != nil || count != 0 {
					t.Fatal("rejected proof published", count, e)
				}
				current, e := s.GetSession(ctx, session.ID)
				if e != nil || current.Pricing != nil {
					t.Fatal("rejected proof changed session", current, e)
				}
				return
			}
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				for _, dimension := range []string{"agent", "model"} {
					key := u.AgentID
					if dimension == "model" {
						key = u.Model
					}
					prices, e := s.CurrentAttributionPrices(ctx, session.ID, dimension, []string{key})
					p := prices[key]
					if e != nil || len(prices) != 1 || p.RecordedTokens != 13 || p.PricedTokens != 13 || p.Cost != 1 || p.SnapshotID != "attributed" {
						t.Fatal("published attribution", prices, e)
					}
				}
				var replacement map[string]any
				if e := json.Unmarshal(payload, &replacement); e != nil {
					t.Fatal(e)
				}
				replacement["id"] = "replacement"
				replacement["estimate"].(map[string]any)["cost"] = 2
				row.Cost = 2
				if e := s.StageObservationPrices(ctx, "replacement", []ObservationPrice{row}); e != nil {
					t.Fatal(e)
				}
				newPayload, _ := json.Marshal(replacement)
				if e := s.SaveProjectionEstimate(ctx, "replacement", session.ID, session.Generation, session.ProjectionRevision, 13, newPayload); e != nil {
					t.Fatal(e)
				}
				prices, e := s.CurrentAttributionPrices(ctx, session.ID, "agent", []string{u.AgentID})
				if e != nil || prices[u.AgentID].Cost != 2 || prices[u.AgentID].SnapshotID != "replacement" {
					t.Fatal("superseded prices leaked", prices, e)
				}
				if _, e = s.db.Exec("UPDATE sessions SET projection=json_remove(projection,'$.pricing','$.costEstimate') WHERE id=?", session.ID); e != nil {
					t.Fatal(e)
				}
				prices, e = s.CurrentAttributionPrices(ctx, session.ID, "agent", []string{u.AgentID})
				if e != nil || len(prices) != 0 {
					t.Fatal("unselected historical prices leaked", prices, e)
				}
				var retained int
				if e = s.db.QueryRow("SELECT count(*) FROM observation_prices").Scan(&retained); e != nil || retained != 2 {
					t.Fatal("historical evidence was discarded", retained, e)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid attribution published", mode, err)
			}
		})
	}
}

func TestObservationPricesRejectInvalidAndUnboundedEvidence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	r := ObservationPrice{ObservationID: "o", RecordedTokens: 1, UnpricedTokens: 1}
	for _, mutate := range []func(*ObservationPrice){func(r *ObservationPrice) { r.Cost = math.NaN() }, func(r *ObservationPrice) { r.Cost = 1 }, func(r *ObservationPrice) { r.PricedTokens = 2 }, func(r *ObservationPrice) { r.UnpricedTokens = 0 }, func(r *ObservationPrice) { r.UnattributedTokens = 2 }} {
		bad := r
		mutate(&bad)
		if err := s.StageObservationPrices(ctx, "s", []ObservationPrice{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if err := s.StageObservationPrices(ctx, "s", make([]ObservationPrice, 501)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestAttributionFiveHundredRowPageUsesOnePublishedSnapshot(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Usage = nil
	prices := make([]ObservationPrice, 500)
	keys := make([]string, 500)
	for i := range prices {
		key := fmt.Sprintf("agent-%03d", i)
		keys[i] = key
		id := fmt.Sprintf("obs-%03d", i)
		b.Usage = append(b.Usage, UsageObservation{ID: id, AgentID: key, Model: key, TokensIn: 1})
		prices[i] = ObservationPrice{ObservationID: id, AgentID: key, Model: key, Cost: 0.001, RecordedTokens: 1, PricedTokens: 1}
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.StageObservationPrices(ctx, "wide", prices); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"id": "wide", "sessionId": "session-1", "generation": src.Generation, "projectionRevision": "", "indexedOffset": 13, "catalogId": "catalog", "context": "test", "attributionVersion": 1, "observations": 500, "estimate": map[string]any{"cost": 0.5, "recordedTokens": 500, "pricedTokens": 500, "unpricedTokens": 0, "unattributedTokens": 0}})
	if err := s.SaveProjectionEstimate(ctx, "wide", "session-1", src.Generation, "", 13, payload); err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []string{"agent", "model"} {
		groups, err := s.CurrentAttributionPrices(ctx, "session-1", dimension, keys)
		if err != nil || len(groups) != 500 {
			t.Fatal(dimension, len(groups), err)
		}
		for _, p := range groups {
			if p.SnapshotID != "wide" || p.PricedTokens != 1 || p.Cost != 0.001 {
				t.Fatal(p)
			}
		}
	}
	agents, err := s.SessionAgents(ctx, "session-1", "", 500)
	if err != nil || len(agents.Agents) != 500 {
		t.Fatal(len(agents.Agents), err)
	}
	models, err := s.SessionModelUsage(ctx, "session-1", "", 500)
	if err != nil || len(models.Models) != 500 {
		t.Fatal(len(models.Models), err)
	}
	for _, a := range agents.Agents {
		if a.Pricing == nil || a.CostEstimate == nil || *a.CostEstimate != 0.001 {
			t.Fatal(a)
		}
	}
	for _, m := range models.Models {
		if m.Pricing == nil || m.CostEstimate == nil || *m.CostEstimate != 0.001 {
			t.Fatal(m)
		}
	}
}
