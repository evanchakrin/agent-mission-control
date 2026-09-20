package store

import (
	"context"
	"database/sql"
	"fmt"
)

// These logical references predate foreign-key enforcement in the candidate.
// Check immutable backup snapshots, not a series of live reads that can span a
// publication. Historical checkpoints may intentionally contain an old or
// damaged parser state; preserving that evidence is not a backup failure.
func checkProjectionIntegrity(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < 2 {
		return nil // A pre-revision backup is upgraded only after verified restore.
	}
	checks := []struct{ name, query string }{
		{"revision source or bounds", `SELECT EXISTS(SELECT 1 FROM projection_revisions r LEFT JOIN sources s ON s.source_id=r.source_id AND s.generation=r.generation WHERE s.source_id IS NULL OR r.indexed_offset<0 OR r.target_offset<r.indexed_offset OR r.target_offset>s.durable_offset OR r.state NOT IN('building','verifying','ready','active','retired'))`},
		{"revision snapshot", `SELECT EXISTS(SELECT 1 FROM projection_revisions WHERE CASE WHEN json_valid(projection) THEN COALESCE(json_extract(projection,'$.sourceId'),'')<>source_id OR COALESCE(json_extract(projection,'$.generation'),'')<>generation OR COALESCE(json_extract(projection,'$.projectionRevision'),'')<>revision OR COALESCE(json_extract(projection,'$.id'),'')='' ELSE 1 END)`},
		{"active revision pointer", `SELECT EXISTS(SELECT 1 FROM active_projection p LEFT JOIN projection_revisions r ON r.revision=p.revision WHERE r.revision IS NULL OR r.source_id<>p.source_id OR r.generation<>p.generation OR r.state<>'active')`},
		{"active checkpoint", `SELECT EXISTS(SELECT 1 FROM projection_revisions r LEFT JOIN active_projection p ON p.source_id=r.source_id AND p.generation=r.generation LEFT JOIN sources s ON s.source_id=r.source_id AND s.generation=r.generation WHERE r.state='active' AND (p.revision IS NULL OR p.revision<>r.revision OR s.indexed_offset<>r.indexed_offset OR s.parser_state<>r.parser_state))`},
		{"published session revision", `SELECT EXISTS(SELECT 1 FROM sessions x LEFT JOIN active_projection p ON p.source_id=x.source_id AND p.generation=x.generation WHERE CASE WHEN json_valid(x.projection) THEN COALESCE(json_extract(x.projection,'$.projectionRevision'),'')<>COALESCE(p.revision,'') ELSE 1 END)`},
		{"event revision reference", `SELECT EXISTS(SELECT 1 FROM events e LEFT JOIN projection_revisions r ON r.revision=e.projection_revision WHERE e.projection_revision<>'' AND (r.revision IS NULL OR r.source_id<>e.source_id OR r.generation<>e.generation OR json_extract(r.projection,'$.id')<>e.session_id))`},
		{"usage revision reference", `SELECT EXISTS(SELECT 1 FROM usage_observations u LEFT JOIN projection_revisions r ON r.revision=u.projection_revision WHERE u.projection_revision<>'' AND (r.revision IS NULL OR r.source_id<>u.source_id OR r.generation<>u.generation OR json_extract(r.projection,'$.id')<>u.session_id))`},
	}
	if version >= 4 {
		checks = append(checks, struct{ name, query string }{"baseline source or bounds", `SELECT EXISTS(SELECT 1 FROM baseline_projections b LEFT JOIN sources s ON s.source_id=b.source_id AND s.generation=b.generation WHERE s.source_id IS NULL OR b.indexed_offset<0 OR b.indexed_offset>s.durable_offset OR NOT json_valid(b.projection))`})
	}
	for _, check := range checks {
		var invalid bool
		if err := db.QueryRowContext(ctx, check.query).Scan(&invalid); err != nil {
			return fmt.Errorf("backup %s check: %w", check.name, err)
		}
		if invalid {
			return fmt.Errorf("backup contains invalid %s", check.name)
		}
	}
	return checkPricingIntegrity(ctx, db)
}

func checkPricingIntegrity(ctx context.Context, db *sql.DB) error {
	var present bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE json_extract(projection,'$.pricing') IS NOT NULL)`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	query := `SELECT EXISTS(SELECT 1 FROM sessions s LEFT JOIN sources r ON r.source_id=s.source_id AND r.generation=s.generation
	 LEFT JOIN accounting_estimates a ON a.id=json_extract(s.projection,'$.pricing.snapshotId')
	 WHERE json_extract(s.projection,'$.pricing') IS NOT NULL AND (a.id IS NULL OR a.session_id<>s.id
	 OR json_extract(a.estimate,'$.comparisonAt') IS NOT NULL
	 OR json_extract(a.estimate,'$.generation') IS NOT s.generation
	 OR COALESCE(json_extract(a.estimate,'$.projectionRevision'),'')<>COALESCE(json_extract(s.projection,'$.projectionRevision'),'')
	 OR json_extract(a.estimate,'$.indexedOffset') IS NOT r.indexed_offset
	 OR json_extract(s.projection,'$.pricing.catalogId') IS NOT json_extract(a.estimate,'$.catalogId')
	 OR json_extract(s.projection,'$.pricing.context') IS NOT json_extract(a.estimate,'$.context')
	 OR json_extract(s.projection,'$.costEstimate') IS NOT CASE WHEN json_extract(a.estimate,'$.estimate.pricedTokens')>0 THEN json_extract(a.estimate,'$.estimate.cost') END`
	for _, field := range []string{"cost", "recordedTokens", "pricedTokens", "unpricedTokens", "unattributedTokens", "coverage"} {
		query += " OR json_extract(s.projection,'$.pricing." + field + "') IS NOT json_extract(a.estimate,'$.estimate." + field + "')"
	}
	var invalid bool
	if err := db.QueryRowContext(ctx, query+"))").Scan(&invalid); err != nil {
		return fmt.Errorf("backup pricing reference check: %w", err)
	}
	if invalid {
		return fmt.Errorf("backup contains invalid pricing reference or coverage")
	}
	return nil
}
