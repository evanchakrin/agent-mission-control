package store

import (
	"context"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestMachineNameChangePublishesAtomicallyWithoutHeartbeatChurn(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	h := protocol.Heartbeat{MachineID: "machine", Name: "First"}
	if err := s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	head, err := s.ChangeHead(ctx)
	if err != nil || head != 1 {
		t.Fatal(head, err)
	}
	if err = s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	next, err := s.ChangeHead(ctx)
	if err != nil || next != head {
		t.Fatal("heartbeat churn", next, err)
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_machine_write BEFORE UPDATE ON machines BEGIN SELECT RAISE(ABORT,'fixture write failure'); END`); err != nil {
		t.Fatal(err)
	}
	h.Name = "Not committed"
	if err = s.RecordHeartbeat(ctx, h); err == nil {
		t.Fatal("fixture failure missing")
	}
	next, err = s.ChangeHead(ctx)
	if err != nil || next != head {
		t.Fatal("uncommitted rename was published", next, err)
	}
	machines, err := s.Machines(ctx)
	if err != nil || machines[0].Heartbeat.Name != "First" {
		t.Fatal(machines, err)
	}
}

func TestChangeHeadOnlyReflectsCommittedLedger(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	check := func(want int64) {
		t.Helper()
		got, err := s.ChangeHead(ctx)
		if err != nil || got != want {
			t.Fatalf("head = %d, %v; want %d", got, err, want)
		}
	}
	check(0)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO changes(seq,kind,session_id,at) VALUES(100001,'organization','s','2026-09-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// The WAL reader must not observe a writer's uncommitted sequence.
	check(0)
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check(0)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO changes(seq,kind,session_id,at) VALUES(100002,'organization','s','2026-09-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	check(100002)
}

func TestMachineLabelInvalidationOnlyAfterCommitAndOncePerOperation(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	head, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := MachineLabelMutation{MachineID: "m", DisplayName: "Owner", OperationID: "rename"}
	if _, err = s.MutateMachineLabel(ctx, p); err != nil {
		t.Fatal(err)
	}
	next, err := s.ChangeHead(ctx)
	if err != nil || next <= head {
		t.Fatal("rename not published", next, err)
	}
	if _, err = s.MutateMachineLabel(ctx, p); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.ChangeHead(ctx)
	if err != nil || replayed != next {
		t.Fatal("receipt invalidation churn", replayed, err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_label_commit BEFORE INSERT ON changes WHEN NEW.kind='machine-label' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	p.OperationID = "failed-rename"
	p.Revision = 1
	p.DisplayName = "Not committed"
	if _, err = s.MutateMachineLabel(ctx, p); err == nil {
		t.Fatal("failure hidden")
	}
	failed, err := s.ChangeHead(ctx)
	if err != nil || failed != next {
		t.Fatal("failed change published", failed, err)
	}
	label, err := s.MachineLabel(ctx, "m")
	if err != nil || label.DisplayName != "Owner" || label.Revision != 1 {
		t.Fatal(label, err)
	}
}
