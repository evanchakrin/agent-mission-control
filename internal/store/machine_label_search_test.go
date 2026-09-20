package store

import (
	"context"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestMachineLabelSearchTracksOwnerEditsAndRollback(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 26)
	count := func(term string, want int) {
		t.Helper()
		var got int
		where, args := catalogSearchPredicate(term)
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_search WHERE `+where, args...).Scan(&got); err != nil || got != want {
			t.Fatal(term, got, want, err)
		}
	}
	p := MachineLabelMutation{MachineID: "m0", DisplayName: "Owner alpha", OperationID: "alpha"}
	if _, err := s.MutateMachineLabel(ctx, p); err != nil {
		t.Fatal(err)
	}
	count("owner alpha", 2)
	if err := s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "m0", Name: "Reported host"}); err != nil {
		t.Fatal(err)
	}
	count("owner alpha", 2)
	count("reported host", 2)
	p.Revision = 1
	p.OperationID = "beta"
	p.DisplayName = "Owner beta"
	if _, err := s.MutateMachineLabel(ctx, p); err != nil {
		t.Fatal(err)
	}
	count("owner alpha", 0)
	count("owner beta", 2)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_label_search BEFORE INSERT ON machine_label_audit BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	p.Revision = 2
	p.OperationID = "clear"
	p.DisplayName = ""
	if _, err := s.MutateMachineLabel(ctx, p); err == nil {
		t.Fatal("audit failure ignored")
	}
	count("owner beta", 2)
	if _, err := s.db.Exec(`DROP TRIGGER reject_label_search`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateMachineLabel(ctx, p); err != nil {
		t.Fatal(err)
	}
	count("owner beta", 0)
	count("reported host", 2)
	if err := s.ImportLegacyMachineLabels(ctx, map[string]string{"m1": "Imported label"}); err != nil {
		t.Fatal(err)
	}
	count("imported label", 1)
}

func TestMachineLabelSearchMigrationBackfillsExistingLabels(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 2)
	label, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", DisplayName: "Existing owner", OperationID: "before-upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE properties SET value='2' WHERE key='catalog_search_schema';
 DROP TRIGGER catalog_search_label_insert; DROP TRIGGER catalog_search_label_update; DROP TRIGGER catalog_search_label_delete;
 UPDATE catalog_search SET text='old index text';
 CREATE TRIGGER forbid_session_rewrite BEFORE UPDATE ON query_sessions BEGIN SELECT RAISE(ABORT,'session rewrite'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureCatalogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureCatalogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM catalog_search WHERE catalog_search MATCH '"existing owner"'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("label not backfilled", count, err)
	}
	got, err := s.MachineLabel(ctx, "m0")
	if err != nil || got != label {
		t.Fatal("organization changed", got, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM catalog_search`).Scan(&count); err != nil || count != 2 {
		t.Fatal("index rows changed", count, err)
	}
}
