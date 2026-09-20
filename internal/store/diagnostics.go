package store

import (
	"context"
	"database/sql"
	"fmt"
)

type Diagnostics struct {
	SQLiteVersion        string           `json:"sqliteVersion"`
	JournalMode          string           `json:"journalMode"`
	Synchronous          int              `json:"synchronous"`
	Sources              int64            `json:"sources"`
	Generations          int64            `json:"generations"`
	DurableBytes         int64            `json:"durableBytes"`
	IndexedBytes         int64            `json:"indexedBytes"`
	IndexingBacklogBytes int64            `json:"indexingBacklogBytes"`
	IndexingBacklogScope string           `json:"indexingBacklogScope"`
	Checkpoint           CheckpointStatus `json:"checkpoint"`
}

func (s *Store) Diagnostics(ctx context.Context) (Diagnostics, error) {
	return s.DiagnosticsWithStage(ctx, nil)
}

// ConnectionStats reads pool counters without obtaining a database connection.
func (s *Store) ConnectionStats() sql.DBStats { return s.db.Stats() }

// DiagnosticsWithStage reports fixed operation names, never SQL inputs or data.
func (s *Store) DiagnosticsWithStage(ctx context.Context, stage func(string)) (Diagnostics, error) {
	if stage == nil {
		stage = func(string) {}
	}
	var d Diagnostics
	stage("ledger-background-admission")
	if err := s.maintenanceMu.LockContext(ctx); err != nil {
		return d, err
	}
	defer s.maintenanceMu.Unlock()
	stage("ledger-version")
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&d.SQLiteVersion); err != nil {
		return d, err
	}
	stage("ledger-journal-mode")
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&d.JournalMode); err != nil {
		return d, err
	}
	stage("ledger-synchronous")
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&d.Synchronous); err != nil {
		return d, err
	}
	stage("ledger-source-totals")
	if err := s.db.QueryRowContext(ctx, ledgerTotalsQuery).Scan(&d.Sources, &d.Generations, &d.DurableBytes, &d.IndexedBytes); err != nil {
		return d, err
	}
	d.IndexingBacklogBytes = d.DurableBytes - d.IndexedBytes
	d.IndexingBacklogScope = "all-retained-generations"
	stage("ledger-checkpoint-status")
	d.Checkpoint = s.CheckpointStatus()
	return d, nil
}
func (s *Store) IntegrityCheck(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return err
		}
		if value != "ok" {
			return fmt.Errorf("database integrity: %s", value)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return nil
}

// Checkpoint lets the owner schedule checkpoints between bounded batches. A
// busy reader is not killed or given an unbounded transaction to wait behind.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, log, checkpointed int
	return s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &log, &checkpointed)
}
