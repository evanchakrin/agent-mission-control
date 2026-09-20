package store

import (
	"context"
	"testing"
)

func TestUsageContributionSchemaUpgradeIsAtomicAndPreservesSelections(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	_, err := s.db.Exec(`CREATE TABLE usage_contribution_selections(proof_id TEXT PRIMARY KEY,selected_at TEXT NOT NULL);
CREATE TABLE usage_contribution_owners(excluded_id TEXT PRIMARY KEY,proof_id TEXT NOT NULL UNIQUE);
INSERT INTO usage_contribution_selections VALUES('existing','saved-time');
INSERT INTO usage_contribution_owners VALUES('original','existing');`)
	if err != nil {
		t.Fatal(err)
	}
	// Interrupt after the schema copy and an additional group member, before commit.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = ensureUsageContributionSchema(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_contribution_owners VALUES('second','existing')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_contribution_owners`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rollback changed mappings", count, err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_contribution_owners VALUES('second','existing')`); err == nil {
		t.Fatal("rollback failed to restore the old uniqueness constraint")
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = ensureUsageContributionSchema(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_contribution_owners VALUES('second','existing')`); err != nil {
		t.Fatal(err)
	}
	if err = ensureUsageContributionSchema(ctx, tx); err != nil {
		t.Fatal("repeat schema setup", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_contribution_owners WHERE proof_id='existing' AND excluded_id IN ('original','second')`).Scan(&count); err != nil || count != 2 {
		t.Fatal("upgrade lost group members", count, err)
	}
	var saved string
	if err = s.db.QueryRow(`SELECT selected_at FROM usage_contribution_selections WHERE proof_id='existing'`).Scan(&saved); err != nil || saved != "saved-time" {
		t.Fatal("upgrade changed historical receipt", saved, err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='usage_contribution_owners_upgrade'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("temporary migration table remains", count, err)
	}
}
