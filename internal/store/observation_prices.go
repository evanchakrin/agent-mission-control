package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
)

// ObservationPrice is attributable pricing evidence, not an allocation of a
// session total. Snapshot publication remains a separate guarded transaction.
type ObservationPrice struct {
	Components         *CostComponents `json:"components,omitempty"`
	ObservationID      string          `json:"observationId"`
	AgentID            string          `json:"agentId"`
	Model              string          `json:"model"`
	Cost               float64         `json:"cost"`
	RecordedTokens     int64           `json:"recordedTokens"`
	PricedTokens       int64           `json:"pricedTokens"`
	UnpricedTokens     int64           `json:"unpricedTokens"`
	UnattributedTokens int64           `json:"unattributedTokens"`
	RateIDs            []string        `json:"rateIds"`
}

// Components describe only directly priced tokens, never a proportional share
// of an older total. Nil means the historical estimate did not record the split.
type CostComponents struct {
	Input        float64 `json:"input"`
	CacheRead    float64 `json:"cacheRead"`
	CacheWrite   float64 `json:"cacheWrite"`
	Output       float64 `json:"output"`
	PricedTokens int64   `json:"pricedTokens"`
}

// Called inside the consistent read transaction preceding guarded publication.
// Each staged row must name a current observation with matching attribution and
// token total; equal cardinalities then establish complete coverage, not a subset.
func validateObservationPrices(ctx context.Context, tx *sql.Tx, id string, s Session, observations int64, p SessionPricing) error {
	if observations < 0 {
		return ErrInvalid
	}
	var actual, staged, invalid, recorded, priced, unpriced, unattributed int64
	var cost float64
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE session_id=? AND source_id=? AND generation=? AND projection_revision=?`, s.ID, s.SourceID, s.Generation, s.ProjectionRevision).Scan(&actual)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN u.id IS NULL OR p.agent_id<>COALESCE(json_extract(u.observation,'$.agentId'),'') OR p.model<>COALESCE(json_extract(u.observation,'$.model'),'') OR json_extract(p.evidence,'$.recordedTokens')<>(COALESCE(json_extract(u.observation,'$.tokensIn'),0)+COALESCE(json_extract(u.observation,'$.tokensCache'),0)+COALESCE(json_extract(u.observation,'$.tokensCacheWrite'),0)+COALESCE(json_extract(u.observation,'$.tokensOut'),0)) THEN 1 ELSE 0 END),0),
 COALESCE(sum(json_extract(p.evidence,'$.recordedTokens')),0),COALESCE(sum(json_extract(p.evidence,'$.pricedTokens')),0),COALESCE(sum(json_extract(p.evidence,'$.unpricedTokens')),0),COALESCE(sum(json_extract(p.evidence,'$.unattributedTokens')),0),COALESCE(sum(json_extract(p.evidence,'$.cost')),0)
 FROM observation_prices p LEFT JOIN usage_observations u ON u.id=p.observation_id AND u.session_id=? AND u.source_id=? AND u.generation=? AND u.projection_revision=? WHERE p.snapshot_id=?`, s.ID, s.SourceID, s.Generation, s.ProjectionRevision, id).Scan(&staged, &invalid, &recorded, &priced, &unpriced, &unattributed, &cost)
	if err != nil {
		return err
	}
	if actual != observations || staged != actual || invalid != 0 || recorded != p.RecordedTokens || priced != p.PricedTokens || unpriced != p.UnpricedTokens || unattributed != p.UnattributedTokens || math.Abs(cost-p.Cost) > 1e-9*math.Max(1, math.Abs(p.Cost)) {
		return ErrInvalid
	}
	var componentRows int64
	var components CostComponents
	err = tx.QueryRowContext(ctx, `SELECT count(json_extract(evidence,'$.components')),COALESCE(sum(json_extract(evidence,'$.components.pricedTokens')),0),COALESCE(sum(json_extract(evidence,'$.components.input')),0),COALESCE(sum(json_extract(evidence,'$.components.cacheRead')),0),COALESCE(sum(json_extract(evidence,'$.components.cacheWrite')),0),COALESCE(sum(json_extract(evidence,'$.components.output')),0) FROM observation_prices WHERE snapshot_id=?`, id).Scan(&componentRows, &components.PricedTokens, &components.Input, &components.CacheRead, &components.CacheWrite, &components.Output)
	if err != nil {
		return err
	}
	if p.Components == nil {
		if componentRows != 0 {
			return ErrInvalid
		}
	} else {
		if componentRows == 0 || p.Components.PricedTokens != priced || components.PricedTokens != priced {
			return ErrInvalid
		}
		for _, pair := range [][2]float64{{components.Input, p.Components.Input}, {components.CacheRead, p.Components.CacheRead}, {components.CacheWrite, p.Components.CacheWrite}, {components.Output, p.Components.Output}} {
			if math.IsNaN(pair[1]) || math.IsInf(pair[1], 0) || pair[1] < 0 || math.Abs(pair[0]-pair[1]) > 1e-9*math.Max(1, p.Cost) {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Verify against one WAL read snapshot without holding the application writer
// mutex. Every staged insertion advances a durable per-snapshot revision; final
// publication compares it after acquiring the writer transaction.
func (s *Store) verifyObservationPrices(ctx context.Context, id, sessionID string, raw json.RawMessage) (int64, bool, error) {
	var snapshot struct {
		AttributionVersion int            `json:"attributionVersion"`
		Observations       int64          `json:"observations"`
		Estimate           SessionPricing `json:"estimate"`
	}
	if json.Unmarshal(raw, &snapshot) != nil {
		return 0, false, ErrInvalid
	}
	if snapshot.AttributionVersion == 0 {
		if snapshot.Estimate.Components != nil {
			return 0, false, ErrInvalid
		}
		return 0, false, nil
	}
	if snapshot.AttributionVersion != 1 {
		return 0, false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, true, err
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT revision FROM observation_price_versions WHERE snapshot_id=?),0)", id).Scan(&revision); err != nil {
		return 0, true, err
	}
	var projection []byte
	if err = tx.QueryRowContext(ctx, "SELECT projection FROM sessions WHERE id=?", sessionID).Scan(&projection); err != nil {
		return 0, true, err
	}
	var session Session
	if err = json.Unmarshal(projection, &session); err != nil {
		return 0, true, err
	}
	if err = validateObservationPrices(ctx, tx, id, session, snapshot.Observations, snapshot.Estimate); err != nil {
		return 0, true, err
	}
	return revision, true, tx.Commit()
}

// StageObservationPrices preserves immutable batches of at most 500 rows.
// Repeated bytes are idempotent; any conflicting row rolls back the whole batch.
// Merely staging rows does not publish them or mutate the session estimate.
func (s *Store) StageObservationPrices(ctx context.Context, snapshot string, rows []ObservationPrice) error {
	if !validID(snapshot) || len(rows) > 500 {
		return ErrInvalid
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return err
	}
	payloads := make([][]byte, len(rows))
	for i, r := range rows {
		if c := r.Components; c != nil {
			if c.PricedTokens != r.PricedTokens {
				return ErrInvalid
			}
			for _, v := range []float64{c.Input, c.CacheRead, c.CacheWrite, c.Output} {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					return ErrInvalid
				}
			}
			total := c.Input + c.CacheRead + c.CacheWrite + c.Output
			if math.IsInf(total, 0) || math.Abs(total-r.Cost) > 1e-10*math.Max(1, r.Cost) {
				return ErrInvalid
			}
		}
		if !validID(r.ObservationID) || len(r.AgentID) > 512 || len(r.Model) > 512 || r.RecordedTokens < 0 || r.PricedTokens < 0 || r.PricedTokens > r.RecordedTokens || r.UnpricedTokens != r.RecordedTokens-r.PricedTokens || r.UnattributedTokens < 0 || r.UnattributedTokens > r.RecordedTokens || r.Cost < 0 || math.IsNaN(r.Cost) || math.IsInf(r.Cost, 0) || (r.PricedTokens == 0 && r.Cost != 0) || len(r.RateIDs) > 100 {
			return ErrInvalid
		}
		for _, id := range r.RateIDs {
			if !validID(id) {
				return ErrInvalid
			}
		}
		b, err := json.Marshal(r)
		if err != nil || len(b) > 16<<10 {
			return ErrInvalid
		}
		payloads[i] = b
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var published bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM accounting_estimates WHERE id=?)", snapshot).Scan(&published); err != nil {
		return err
	}
	for i, r := range rows {
		var prior []byte
		err = tx.QueryRowContext(ctx, "SELECT evidence FROM observation_prices WHERE snapshot_id=? AND observation_id=?", snapshot, r.ObservationID).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, payloads[i]) {
				return ErrConflict
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if published {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO observation_prices VALUES(?,?,?,?,?)", snapshot, r.ObservationID, r.AgentID, r.Model, payloads[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
