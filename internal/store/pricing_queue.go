package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Policies are explicit owner choices. The queue is reconstructible work, while
// policies and immutable estimate snapshots are durable accounting history.
func (s *Store) InitializePricingQueue(ctx context.Context) error {
	if s.pricingReady.Load() {
		return nil
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.pricingReady.Load() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS pricing_policies(session_id TEXT PRIMARY KEY REFERENCES sessions(id),catalog_id TEXT NOT NULL REFERENCES rate_catalogs(id),billing_context TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS pricing_policy_comparisons(session_id TEXT PRIMARY KEY REFERENCES pricing_policies(session_id) ON DELETE CASCADE,comparison_at TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS pricing_policy_changed AFTER UPDATE ON pricing_policies BEGIN DELETE FROM pricing_policy_comparisons WHERE session_id=NEW.session_id; END;
CREATE TABLE IF NOT EXISTS pricing_jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL UNIQUE REFERENCES pricing_policies(session_id),next_attempt TEXT NOT NULL DEFAULT '',problem TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS pricing_jobs_ready ON pricing_jobs(next_attempt,id);
CREATE TABLE IF NOT EXISTS pricing_checkpoints(job_id INTEGER PRIMARY KEY REFERENCES pricing_jobs(id) ON DELETE CASCADE, checkpoint BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS pricing_comparison_default(id INTEGER PRIMARY KEY CHECK(id=1),catalog_id TEXT NOT NULL REFERENCES rate_catalogs(id),billing_context TEXT NOT NULL,comparison_at TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS pricing_default_new_session AFTER INSERT ON sessions
WHEN EXISTS(SELECT 1 FROM pricing_comparison_default WHERE id=1) AND NOT EXISTS(SELECT 1 FROM pricing_policies WHERE session_id=NEW.id)
BEGIN
 INSERT INTO pricing_policies SELECT NEW.id,catalog_id,billing_context FROM pricing_comparison_default WHERE id=1;
 INSERT INTO pricing_policy_comparisons SELECT NEW.id,comparison_at FROM pricing_comparison_default WHERE id=1;
 INSERT INTO pricing_jobs(session_id) VALUES(NEW.id);
END;
DROP TRIGGER IF EXISTS pricing_queue_index;
CREATE TRIGGER pricing_queue_index AFTER UPDATE OF projection ON sessions
WHEN json_extract(NEW.projection,'$.pricing') IS NULL AND EXISTS(SELECT 1 FROM pricing_policies WHERE session_id=NEW.id)
BEGIN DELETE FROM pricing_jobs WHERE session_id=NEW.id; INSERT INTO pricing_jobs(session_id) VALUES(NEW.id); END;`)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.pricingReady.Store(true)
	return nil
}

type PricingJob struct {
	ID           int64           `json:"id"`
	SessionID    string          `json:"sessionId"`
	CatalogID    string          `json:"catalogId"`
	Context      string          `json:"context"`
	Problem      string          `json:"problem,omitempty"`
	Checkpoint   json.RawMessage `json:"-"`
	ComparisonAt *time.Time      `json:"comparisonAt,omitempty"`
}

func (s *Store) PricingPolicy(ctx context.Context, sessionID string) (PricingJob, error) {
	var j PricingJob
	var comparison string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(j.id,0),p.session_id,p.catalog_id,p.billing_context,COALESCE(j.problem,''),COALESCE(d.comparison_at,'') FROM pricing_policies p LEFT JOIN pricing_policy_comparisons d ON d.session_id=p.session_id LEFT JOIN pricing_jobs j ON j.session_id=p.session_id WHERE p.session_id=?`, sessionID).Scan(&j.ID, &j.SessionID, &j.CatalogID, &j.Context, &j.Problem, &comparison)
	if err == sql.ErrNoRows {
		return j, ErrNotFound
	}
	if err == nil && comparison != "" {
		var at time.Time
		at, err = time.Parse(time.RFC3339Nano, comparison)
		j.ComparisonAt = &at
	}
	return j, err
}

func (s *Store) DisablePricingPolicy(ctx context.Context, sessionID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM pricing_jobs WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM pricing_policies WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPricingPolicy(ctx context.Context, sessionID, catalogID, billingContext string) error {
	return s.SetPricingPolicyAt(ctx, sessionID, catalogID, billingContext, nil)
}

func (s *Store) SetPricingPolicyAt(ctx context.Context, sessionID, catalogID, billingContext string, comparisonAt *time.Time) error {
	if !validID(sessionID) || !validID(catalogID) || billingContext == "" || len(billingContext) > 1024 {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	comparison := ""
	if comparisonAt != nil {
		if comparisonAt.IsZero() {
			return ErrInvalid
		}
		comparison = comparisonAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_policies VALUES(?,?,?) ON CONFLICT(session_id) DO UPDATE SET catalog_id=excluded.catalog_id,billing_context=excluded.billing_context`, sessionID, catalogID, billingContext); err != nil {
		return err
	}
	if comparison != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_policy_comparisons VALUES(?,?)`, sessionID, comparison); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO pricing_jobs(session_id) VALUES(?)`, sessionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('pricing-policy',?,?)`, sessionID, stamp(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NextPricingJob(ctx context.Context) (PricingJob, error) {
	var j PricingJob
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return j, err
	}
	defer s.maintenanceMu.Unlock()
	var comparison string
	err := s.db.QueryRowContext(ctx, `SELECT j.id,j.session_id,p.catalog_id,p.billing_context,j.problem,COALESCE(c.checkpoint,X''),COALESCE(d.comparison_at,'') FROM pricing_jobs j JOIN pricing_policies p ON p.session_id=j.session_id LEFT JOIN pricing_policy_comparisons d ON d.session_id=p.session_id LEFT JOIN pricing_checkpoints c ON c.job_id=j.id WHERE j.next_attempt<=? ORDER BY j.next_attempt,j.id LIMIT 1`, stamp(time.Now())).Scan(&j.ID, &j.SessionID, &j.CatalogID, &j.Context, &j.Problem, &j.Checkpoint, &comparison)
	if err == sql.ErrNoRows {
		return j, ErrNotFound
	}
	if err == nil && comparison != "" {
		var at time.Time
		at, err = time.Parse(time.RFC3339Nano, comparison)
		j.ComparisonAt = &at
	}
	return j, err
}

// Checkpoint CAS prevents repeated or concurrent dispatches from double-adding a
// page. Queue replacement cascades away obsolete checkpoints transactionally.
func (s *Store) SavePricingCheckpoint(ctx context.Context, j PricingJob, checkpoint json.RawMessage) error {
	if !json.Valid(checkpoint) || len(checkpoint) > 1<<20 {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pricing_jobs j LEFT JOIN pricing_checkpoints c ON c.job_id=j.id WHERE j.id=? AND COALESCE(c.checkpoint,X'')=?)`, j.ID, []byte(nonNilCheckpoint(j.Checkpoint))).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_checkpoints VALUES(?,?) ON CONFLICT(job_id) DO UPDATE SET checkpoint=excluded.checkpoint`, j.ID, []byte(checkpoint)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE pricing_jobs SET next_attempt=?,problem='' WHERE id=?`, stamp(time.Now()), j.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func nonNilCheckpoint(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func (s *Store) FinishPricingJob(ctx context.Context, j PricingJob, problem error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var err error
	if problem == nil {
		_, err = s.db.ExecContext(ctx, `DELETE FROM pricing_jobs WHERE id=? AND COALESCE((SELECT checkpoint FROM pricing_checkpoints WHERE job_id=pricing_jobs.id),X'')=?`, j.ID, nonNilCheckpoint(j.Checkpoint))
	} else {
		message := problem.Error()
		if len(message) > 4096 {
			message = message[:4096]
		}
		_, err = s.db.ExecContext(ctx, `UPDATE pricing_jobs SET problem=?,next_attempt=? WHERE id=? AND COALESCE((SELECT checkpoint FROM pricing_checkpoints WHERE job_id=pricing_jobs.id),X'')=?`, message, stamp(time.Now().Add(time.Minute)), j.ID, nonNilCheckpoint(j.Checkpoint))
	}
	return err
}
