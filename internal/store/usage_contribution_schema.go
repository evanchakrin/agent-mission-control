package store

import (
	"context"
	"database/sql"
	"time"
)

func ensureUsageContributionSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_contribution_selections(proof_id TEXT PRIMARY KEY,selected_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS usage_contribution_owners(excluded_id TEXT PRIMARY KEY,proof_id TEXT NOT NULL);`); err != nil {
		return err
	}
	// Earlier unreleased candidates allowed only one excluded observation per
	// proof. Preserve every selection while permitting a two-piece fork group.
	var uniqueProof int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('usage_contribution_owners') l WHERE l."unique"=1 AND (SELECT COUNT(*) FROM pragma_index_info(l.name))=1 AND EXISTS(SELECT 1 FROM pragma_index_info(l.name) WHERE name='proof_id')`).Scan(&uniqueProof); err != nil {
		return err
	}
	if uniqueProof > 0 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE usage_contribution_owners_upgrade(excluded_id TEXT PRIMARY KEY,proof_id TEXT NOT NULL);
 INSERT INTO usage_contribution_owners_upgrade SELECT excluded_id,proof_id FROM usage_contribution_owners;
 DROP TABLE usage_contribution_owners;
 ALTER TABLE usage_contribution_owners_upgrade RENAME TO usage_contribution_owners;`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS usage_contribution_proof ON usage_contribution_owners(proof_id)`)
	return err
}

func writeUsageContributionSelection(ctx context.Context, tx *sql.Tx, p UsageReconciliationProof) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_contribution_selections VALUES(?,?)`, p.ID, stamp(time.Now())); err != nil {
		return err
	}
	for _, e := range append([]UsageProofEndpoint{p.Right}, p.ExtraRight...) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_contribution_owners VALUES(?,?) ON CONFLICT(excluded_id) DO UPDATE SET proof_id=excluded.proof_id`, e.ObservationID, p.ID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO changes(kind,session_id,at) VALUES('usage-reconciliation',?,?)`, p.Right.SessionID, stamp(time.Now()))
	return err
}
