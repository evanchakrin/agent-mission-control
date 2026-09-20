package store

import (
	"context"
	"errors"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestMachineLabelIndependentAndIdempotent(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	p := MachineLabelMutation{MachineID: "stable", DisplayName: " Home hub ", OperationID: "name-1", RecoveryEpoch: s.RecoveryEpoch()}
	first, err := s.MutateMachineLabel(ctx, p)
	if err != nil || first.DisplayName != "Home hub" || first.Revision != 1 {
		t.Fatal(first, err)
	}
	if err = s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "stable", Name: "new-hostname"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MachineLabel(ctx, "stable")
	if err != nil || got != first {
		t.Fatal("heartbeat replaced label", got, err)
	}
	replay, err := s.MutateMachineLabel(ctx, p)
	if err != nil || replay != first {
		t.Fatal("receipt not stable", replay, err)
	}
	p.DisplayName = "changed"
	if _, err = s.MutateMachineLabel(ctx, p); !errors.Is(err, ErrConflict) {
		t.Fatal("reused key accepted", err)
	}
	p.OperationID = "name-2"
	if _, err = s.MutateMachineLabel(ctx, p); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision accepted", err)
	}
	p.Revision = 1
	p.DisplayName = " "
	cleared, err := s.MutateMachineLabel(ctx, p)
	if err != nil || cleared.DisplayName != "" || cleared.Revision != 2 {
		t.Fatal(cleared, err)
	}
	var audits, receipts int
	if err = s.db.QueryRow(`SELECT count(*) FROM machine_label_audit`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM machine_label_operations`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if audits != 2 || receipts != 2 {
		t.Fatal(audits, receipts)
	}
	p.OperationID = "name-3"
	p.Revision = 2
	p.RecoveryEpoch = "pre-restore"
	if _, err = s.MutateMachineLabel(ctx, p); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("old epoch accepted", err)
	}
}

func TestMachineLabelAuditFailureRollsBack(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_label_audit BEFORE INSERT ON machine_label_audit BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	p := MachineLabelMutation{MachineID: "stable", DisplayName: "Owner name", OperationID: "rename"}
	if _, err := s.MutateMachineLabel(ctx, p); err == nil {
		t.Fatal("audit failure hidden")
	}
	got, err := s.MachineLabel(ctx, p.MachineID)
	if err != nil || got.Revision != 0 || got.DisplayName != "" {
		t.Fatal("label escaped rollback", got, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_label_audit`); err != nil {
		t.Fatal(err)
	}
	got, err = s.MutateMachineLabel(ctx, p)
	if err != nil || got.Revision != 1 {
		t.Fatal("retry could not commit", got, err)
	}
}

func TestMachineLabelSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := MachineLabelMutation{MachineID: "stable", DisplayName: "Retained name", OperationID: "durable-name"}
	first, err := s.MutateMachineLabel(ctx, p)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.MachineLabel(ctx, p.MachineID)
	if err != nil || got != first {
		t.Fatal("reopen lost label", got, err)
	}
	got, err = s.MutateMachineLabel(ctx, p)
	if err != nil || got != first {
		t.Fatal("reopen lost receipt", got, err)
	}
}

func TestMachineLabelReadViewsPreserveStableIdentity(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	for _, id := range []string{"m0", "m1"} {
		if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: id, DisplayName: "Owner " + id, OperationID: "label-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordHeartbeat(ctx, protocol.Heartbeat{MachineID: "m0", Name: "Observed host"}); err != nil {
		t.Fatal(err)
	}
	live, err := s.ListMachines(ctx)
	if err != nil || len(live) != 1 {
		t.Fatal(live, err)
	}
	if live[0].Heartbeat.Name != "Observed host" || live[0].Heartbeat.MachineID != "m0" || live[0].Label.DisplayName != "Owner m0" || live[0].Label.Revision != 1 {
		t.Fatal(live)
	}
	page, err := s.CatalogMachines(ctx, SessionQuery{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "m0" || page.Items[0].Name != "Owner m0" || page.Items[0].Sessions != 1 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	next, err := s.CatalogMachines(ctx, SessionQuery{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != "m1" || next.Items[0].Name != "Owner m1" {
		t.Fatal("history-only label", next, err)
	}
	if _, err = s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", Revision: 1, OperationID: "clear"}); err != nil {
		t.Fatal(err)
	}
	page, err = s.CatalogMachines(ctx, SessionQuery{Limit: 1})
	if err != nil || page.Items[0].Name != "Observed host" || page.Items[0].ID != "m0" {
		t.Fatal("clear fallback", page, err)
	}
}

func TestLegacyMachineLabelsPreserveOwnerStateAndMissingHistory(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "owner", DisplayName: "Current owner label", OperationID: "owner-edit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "cleared", DisplayName: "", OperationID: "owner-clear"}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{"owner": "Old label", "cleared": "Must not resurrect", "missing-history": "Preserved label"}
	for i := 0; i < 2; i++ {
		if err := s.ImportLegacyMachineLabels(ctx, names); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"owner": "Current owner label", "cleared": "", "missing-history": "Preserved label"} {
		got, err := s.MachineLabel(ctx, id)
		if err != nil || got.DisplayName != want || got.Revision != 1 {
			t.Fatal(id, got, err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM machine_label_audit`).Scan(&count); err != nil || count != 3 {
		t.Fatal("duplicate import audits", count, err)
	}
}

func TestLegacyMachineLabelImportAuditFailureRollsBack(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_label_import BEFORE INSERT ON machine_label_audit BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyMachineLabels(ctx, map[string]string{"remote": "Imported"}); err == nil {
		t.Fatal("audit failure ignored")
	}
	got, err := s.MachineLabel(ctx, "remote")
	if err != nil || got.Revision != 0 {
		t.Fatal("partial import survived", got, err)
	}
}

func TestSessionReadsResolveMachineLabelWithoutRewritingProjection(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 2)
	var before string
	if err := s.db.QueryRow(`SELECT projection FROM sessions WHERE id='s-000000'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", DisplayName: "Owner label", OperationID: "session-label"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.QuerySessions(ctx, SessionQuery{MachineID: "m0", Limit: 1})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatal(page, err)
	}
	row := page.Sessions[0]
	if row.MachineID != "m0" || row.MachineName != "Owner label" || row.TokensIn != 1 || row.TokensOut != 2 {
		t.Fatal(row)
	}
	detail, err := s.GetSession(ctx, "s-000000")
	if err != nil || detail.MachineName != row.MachineName || detail.MachineID != row.MachineID {
		t.Fatal(detail, err)
	}
	if _, err = s.MutateMachineLabel(ctx, MachineLabelMutation{MachineID: "m0", Revision: 1, OperationID: "clear-session-label"}); err != nil {
		t.Fatal(err)
	}
	detail, err = s.GetSession(ctx, "s-000000")
	if err != nil || detail.MachineName != "m0" {
		t.Fatal("clear fallback", detail, err)
	}
	var after string
	if err = s.db.QueryRow(`SELECT projection FROM sessions WHERE id='s-000000'`).Scan(&after); err != nil || after != before {
		t.Fatal("projection rewritten", err)
	}
}
