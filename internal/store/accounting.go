package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"
)

// InitializeAccounting is an additive candidate migration. Catalogs and prior
// estimates are append-only evidence, included by the normal ledger backup.
func (s *Store) InitializeAccounting(ctx context.Context) error {
	if s.accountingReady.Load() {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.accountingReady.Load() {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS economics_history(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,reason TEXT NOT NULL,measured_at INTEGER NOT NULL,measurement BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS economics_history_measured ON economics_history(measured_at);
 CREATE TABLE IF NOT EXISTS economics_capture_resolutions(id TEXT PRIMARY KEY,original_epoch TEXT NOT NULL,resolved_epoch TEXT NOT NULL,resolved_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS economics_resolution_time ON economics_capture_resolutions(resolved_at,id);
 CREATE TABLE IF NOT EXISTS rate_catalogs(id TEXT PRIMARY KEY,created_at TEXT NOT NULL,catalog BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS accounting_estimates(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,created_at TEXT NOT NULL,estimate BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS accounting_session ON accounting_estimates(session_id,created_at,id);
 CREATE INDEX IF NOT EXISTS accounting_session_cursor ON accounting_estimates(session_id,id);
 CREATE TABLE IF NOT EXISTS current_comparisons(session_id TEXT PRIMARY KEY,snapshot_id TEXT NOT NULL,generation TEXT NOT NULL,projection_revision TEXT NOT NULL,indexed_offset INTEGER NOT NULL,cost REAL NOT NULL,priced_tokens INTEGER NOT NULL,unattributed_tokens INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS observation_prices(snapshot_id TEXT NOT NULL,observation_id TEXT NOT NULL,agent_id TEXT NOT NULL,model TEXT NOT NULL,evidence BLOB NOT NULL,PRIMARY KEY(snapshot_id,observation_id));
 CREATE INDEX IF NOT EXISTS observation_prices_agent ON observation_prices(snapshot_id,agent_id,observation_id);
 CREATE INDEX IF NOT EXISTS observation_prices_model ON observation_prices(snapshot_id,model,observation_id);
 CREATE TABLE IF NOT EXISTS observation_price_versions(snapshot_id TEXT PRIMARY KEY,revision INTEGER NOT NULL);
 CREATE TRIGGER IF NOT EXISTS observation_price_added AFTER INSERT ON observation_prices BEGIN
 INSERT INTO observation_price_versions VALUES(NEW.snapshot_id,1) ON CONFLICT(snapshot_id) DO UPDATE SET revision=revision+1; END;`)
	if err == nil {
		s.accountingReady.Store(true)
	}
	return err
}
func (s *Store) PutRateCatalog(ctx context.Context, id string, catalog json.RawMessage) error {
	if !validID(id) || !json.Valid(catalog) || len(catalog) > 1<<20 {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var prior []byte
	err := s.db.QueryRowContext(ctx, `SELECT catalog FROM rate_catalogs WHERE id=?`, id).Scan(&prior)
	if err == nil {
		if bytes.Equal(prior, catalog) {
			return nil
		}
		return ErrConflict
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO rate_catalogs VALUES(?,?,?)`, id, stamp(time.Now()), []byte(catalog))
	return err
}
func (s *Store) RateCatalog(ctx context.Context, id string) (json.RawMessage, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT catalog FROM rate_catalogs WHERE id=?`, id).Scan(&b)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return json.RawMessage(b), err
}

func (s *Store) RateCatalogIDs(ctx context.Context, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM rate_catalogs WHERE id>? ORDER BY id LIMIT 101`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) SaveEstimate(ctx context.Context, id, sessionID string, estimate json.RawMessage) error {
	if !validID(id) || !validID(sessionID) || !json.Valid(estimate) || len(estimate) > 1<<20 {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var prior []byte
	err := s.db.QueryRowContext(ctx, `SELECT estimate FROM accounting_estimates WHERE id=?`, id).Scan(&prior)
	if err == nil {
		if bytes.Equal(prior, estimate) {
			return nil
		}
		return ErrConflict
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounting_estimates VALUES(?,?,?,?)`, id, sessionID, stamp(time.Now()), []byte(estimate))
	return err
}
func (s *Store) EstimateHistory(ctx context.Context, sessionID, afterID string, limit int) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT estimate FROM accounting_estimates WHERE session_id=? AND id>? ORDER BY id LIMIT ?`, sessionID, afterID, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}

// SaveProjectionEstimate closes the race between a paginated usage read and
// publication of a new interpretation at the exact same raw byte offset.
func (s *Store) SaveProjectionEstimate(ctx context.Context, id, sessionID, generation, revision string, indexedOffset int64, estimate json.RawMessage) error {
	return s.SaveProjectionEstimateForJob(ctx, id, sessionID, generation, revision, indexedOffset, estimate, nil)
}

func (s *Store) SaveProjectionEstimateForJob(ctx context.Context, id, sessionID, generation, revision string, indexedOffset int64, estimate json.RawMessage, job *PricingJob) error {
	if !validID(id) || !validID(sessionID) || !json.Valid(estimate) || len(estimate) > 1<<20 {
		return ErrInvalid
	}
	proof, attributed, proofErr := s.verifyObservationPrices(ctx, id, sessionID, estimate)
	if proofErr != nil {
		return proofErr
	}
	if attributed && s.options.AfterAttributionVerification != nil {
		if err := s.options.AfterAttributionVerification(); err != nil {
			return err
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if attributed {
		var current int64
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT revision FROM observation_price_versions WHERE snapshot_id=?),0)", id).Scan(&current); err != nil {
			return err
		}
		if current != proof {
			return ErrConflict
		}
	}
	if job != nil {
		var valid bool
		comparison := ""
		if job.ComparisonAt != nil {
			comparison = job.ComparisonAt.UTC().Format(time.RFC3339Nano)
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pricing_jobs j JOIN pricing_policies p ON p.session_id=j.session_id LEFT JOIN pricing_policy_comparisons d ON d.session_id=p.session_id LEFT JOIN pricing_checkpoints c ON c.job_id=j.id WHERE j.id=? AND j.session_id=? AND p.catalog_id=? AND p.billing_context=? AND COALESCE(d.comparison_at,'')=? AND COALESCE(c.checkpoint,X'')=?)`, job.ID, sessionID, job.CatalogID, job.Context, comparison, nonNilCheckpoint(job.Checkpoint)).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrConflict
		}
	}
	var projection []byte
	var currentOffset int64
	err = tx.QueryRowContext(ctx, `SELECT s.projection,r.indexed_offset FROM sessions s JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation WHERE s.id=?`, sessionID).Scan(&projection, &currentOffset)
	if err != nil {
		return err
	}
	var current Session
	if err = json.Unmarshal(projection, &current); err != nil {
		return err
	}
	if generation != current.Generation || revision != current.ProjectionRevision || indexedOffset != currentOffset {
		return ErrConflict
	}
	var snapshot struct {
		ID                 string         `json:"id"`
		SessionID          string         `json:"sessionId"`
		Generation         string         `json:"generation"`
		ProjectionRevision string         `json:"projectionRevision"`
		IndexedOffset      int64          `json:"indexedOffset"`
		CatalogID          string         `json:"catalogId"`
		Context            string         `json:"context"`
		ComparisonAt       *time.Time     `json:"comparisonAt"`
		Estimate           SessionPricing `json:"estimate"`
		AttributionVersion int            `json:"attributionVersion"`
		Observations       int64          `json:"observations"`
	}
	if json.Unmarshal(estimate, &snapshot) != nil || snapshot.ID != id || snapshot.SessionID != sessionID || snapshot.Generation != generation || snapshot.ProjectionRevision != revision || snapshot.IndexedOffset != indexedOffset || !validID(snapshot.CatalogID) || snapshot.Context == "" {
		return ErrInvalid
	}
	p := snapshot.Estimate
	if job != nil {
		if snapshot.CatalogID != job.CatalogID || snapshot.Context != job.Context || (snapshot.ComparisonAt == nil) != (job.ComparisonAt == nil) {
			return ErrConflict
		}
		if snapshot.ComparisonAt != nil && !snapshot.ComparisonAt.Equal(*job.ComparisonAt) {
			return ErrConflict
		}
	}
	if snapshot.AttributionVersion != 0 {
		if snapshot.AttributionVersion != 1 {
			return ErrInvalid
		}
	}
	var recorded int64
	for _, n := range []int64{current.TokensIn, current.TokensCache, current.TokensCacheWrite, current.TokensOut} {
		if n < 0 || n > math.MaxInt64-recorded {
			return ErrInvalid
		}
		recorded += n
	}
	if p.RecordedTokens != recorded || p.PricedTokens < 0 || p.PricedTokens > recorded || p.UnpricedTokens != recorded-p.PricedTokens || p.UnattributedTokens < 0 || p.UnattributedTokens > recorded || p.Cost < 0 || math.IsNaN(p.Cost) || math.IsInf(p.Cost, 0) {
		return ErrInvalid
	}
	p.SnapshotID = id
	p.CatalogID = snapshot.CatalogID
	p.Context = snapshot.Context
	p.Coverage = 0
	if recorded > 0 {
		p.Coverage = float64(p.PricedTokens) / float64(recorded)
	}
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT estimate FROM accounting_estimates WHERE id=?`, id).Scan(&prior)
	existing := err == nil
	if err == nil {
		if !bytes.Equal(prior, estimate) {
			return ErrConflict
		}
		// A retry cannot re-select an older catalog over a later estimate. A
		// pre-publication-version snapshot may populate a missing display once.
		if (snapshot.ComparisonAt != nil && job == nil) || (current.Pricing != nil && job == nil) {
			return nil
		}
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !existing {
		if _, err = tx.ExecContext(ctx, `INSERT INTO accounting_estimates VALUES(?,?,?,?)`, id, sessionID, stamp(time.Now()), []byte(estimate)); err != nil {
			return err
		}
	}
	if snapshot.ComparisonAt == nil {
		var cost any
		if p.PricedTokens > 0 {
			cost = p.Cost
		}
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET projection=json_set(projection,'$.costEstimate',?,'$.pricing',json(?)) WHERE id=?`, cost, string(data), sessionID); err != nil {
			return err
		}
	}
	if snapshot.ComparisonAt != nil && job != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO current_comparisons VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET snapshot_id=excluded.snapshot_id,generation=excluded.generation,projection_revision=excluded.projection_revision,indexed_offset=excluded.indexed_offset,cost=excluded.cost,priced_tokens=excluded.priced_tokens,unattributed_tokens=excluded.unattributed_tokens`, sessionID, id, generation, revision, indexedOffset, p.Cost, p.PricedTokens, p.UnattributedTokens); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('pricing',?,?)`, sessionID, stamp(time.Now())); err != nil {
		return err
	}
	if s.options.BeforeCommit != nil {
		if err = s.options.BeforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}
