package store

import "context"

// Seed and install the maintenance triggers in one writer transaction. The
// totals cover every retained generation, not only currently visible sessions.
// Trigger updates roll back with their source mutations, including failed
// ingestion, generation changes and staged-rebuild publication.
func (s *Store) migrateLedgerTotals(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 6 {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
CREATE TABLE ledger_totals (
 id INTEGER PRIMARY KEY CHECK(id=1),
 sources INTEGER NOT NULL CHECK(sources>=0),
 generations INTEGER NOT NULL CHECK(generations>=0),
 durable_bytes INTEGER NOT NULL CHECK(durable_bytes>=0),
 indexed_bytes INTEGER NOT NULL CHECK(indexed_bytes>=0));
INSERT INTO ledger_totals
 SELECT 1,(SELECT COUNT(*) FROM source_identity),COUNT(*),
 COALESCE(SUM(durable_offset),0),COALESCE(SUM(indexed_offset),0) FROM sources;
CREATE TRIGGER ledger_totals_identity_insert AFTER INSERT ON source_identity BEGIN
 UPDATE ledger_totals SET sources=sources+1 WHERE id=1;
END;
CREATE TRIGGER ledger_totals_identity_delete AFTER DELETE ON source_identity BEGIN
 UPDATE ledger_totals SET sources=sources-1 WHERE id=1;
END;
CREATE TRIGGER ledger_totals_source_insert AFTER INSERT ON sources BEGIN
 UPDATE ledger_totals SET generations=generations+1,
 durable_bytes=durable_bytes+NEW.durable_offset,
 indexed_bytes=indexed_bytes+NEW.indexed_offset WHERE id=1;
END;
CREATE TRIGGER ledger_totals_source_delete AFTER DELETE ON sources BEGIN
 UPDATE ledger_totals SET generations=generations-1,
 durable_bytes=durable_bytes-OLD.durable_offset,
 indexed_bytes=indexed_bytes-OLD.indexed_offset WHERE id=1;
END;
CREATE TRIGGER ledger_totals_source_update AFTER UPDATE OF durable_offset,indexed_offset ON sources
 WHEN NEW.durable_offset<>OLD.durable_offset OR NEW.indexed_offset<>OLD.indexed_offset BEGIN
 UPDATE ledger_totals SET durable_bytes=durable_bytes+NEW.durable_offset-OLD.durable_offset,
 indexed_bytes=indexed_bytes+NEW.indexed_offset-OLD.indexed_offset WHERE id=1;
END;
PRAGMA user_version=6;
`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

const ledgerTotalsQuery = `SELECT sources,generations,durable_bytes,indexed_bytes FROM ledger_totals WHERE id=1`
