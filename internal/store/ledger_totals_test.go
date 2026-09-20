package store

import (
	"context"
	"strings"
	"testing"
)

func dropLedgerTotalsForLegacyFixture(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DROP TRIGGER ledger_totals_identity_insert; DROP TRIGGER ledger_totals_identity_delete;
DROP TRIGGER ledger_totals_source_insert; DROP TRIGGER ledger_totals_source_delete; DROP TRIGGER ledger_totals_source_update;
DROP TABLE ledger_totals;`); err != nil {
		t.Fatal(err)
	}
}

func assertLedgerTotals(t *testing.T, s *Store) {
	t.Helper()
	var want, got [4]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM source_identity),COUNT(*),COALESCE(SUM(durable_offset),0),COALESCE(SUM(indexed_offset),0) FROM sources`).Scan(&want[0], &want[1], &want[2], &want[3]); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(ledgerTotalsQuery).Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("maintained totals differ from source evidence", got, want)
	}
	d, err := s.Diagnostics(context.Background())
	if err != nil || d.Sources != want[0] || d.Generations != want[1] || d.DurableBytes != want[2] || d.IndexedBytes != want[3] {
		t.Fatal(d, err)
	}
}

func TestLedgerTotalsTrackHistoryRollbackAndRestart(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	assertLedgerTotals(t, s)
	src := testSource()
	ingest(t, s, src, 0, "fixture\n")
	if err := s.CommitIndex(context.Background(), batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	src.Generation = "replacement"
	ingest(t, s, src, 0, "replacement\n")
	assertLedgerTotals(t, s)
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE sources SET durable_offset=durable_offset+100,indexed_offset=indexed_offset+50`); err != nil {
		t.Fatal(err)
	}
	// Another WAL reader must still observe the old committed totals.
	assertLedgerTotals(t, s)
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertLedgerTotals(t, s)
	if _, err = s.db.Exec(`INSERT INTO source_identity VALUES('delete-fixture','m','codex','','g');
INSERT INTO sources(source_id,generation,meta_json,durable_offset,indexed_offset,created_at,updated_at) VALUES('delete-fixture','g','{}',15,5,'','');`); err != nil {
		t.Fatal(err)
	}
	assertLedgerTotals(t, s)
	if _, err = s.db.Exec(`DELETE FROM sources WHERE source_id='delete-fixture'; DELETE FROM source_identity WHERE source_id='delete-fixture';`); err != nil {
		t.Fatal(err)
	}
	assertLedgerTotals(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.dir, Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLedgerTotals(t, reopened)
}

func TestLedgerTotalsMigrateExistingHistoryExactlyOnce(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	src := testSource()
	ingest(t, s, src, 0, "fixture\n")
	if err := s.CommitIndex(context.Background(), batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-v6 shape only in this disposable fixture.
	dropLedgerTotalsForLegacyFixture(t, s)
	if _, err := s.db.Exec(`PRAGMA user_version=5;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.dir, Options{ExternalCheckpointOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLedgerTotals(t, reopened)
	if err = reopened.migrateLedgerTotals(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertLedgerTotals(t, reopened)
	var version int
	if err = reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatal(version, err)
	}
	src.Generation = "new"
	ingest(t, reopened, src, 0, "new\n")
	assertLedgerTotals(t, reopened)
}

func TestLedgerTotalsHealthUsesOnePrimaryKeyLookup(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	rows, err := s.db.Query("EXPLAIN QUERY PLAN " + ledgerTotalsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	steps := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(detail, "SEARCH ledger_totals USING INTEGER PRIMARY KEY") {
			t.Fatal("health totals scan history", detail)
		}
		steps++
	}
	if err = rows.Err(); err != nil || steps != 1 {
		t.Fatal(steps, err)
	}
}
