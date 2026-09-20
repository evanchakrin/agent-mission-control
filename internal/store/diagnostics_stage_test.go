package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDiagnosticsReportsStagesAndPoolWait(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	var stages []string
	stage := func(value string) { stages = append(stages, value) }
	if _, err := s.DiagnosticsWithStage(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	want := []string{"ledger-background-admission", "ledger-version", "ledger-journal-mode", "ledger-synchronous", "ledger-source-totals", "ledger-checkpoint-status"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatal("incorrect diagnostic stages", stages)
	}
	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	before := s.ConnectionStats()
	if before.InUse != 4 || before.Idle != 0 {
		t.Fatal("pool was not exhausted", before)
	}
	stages = nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.DiagnosticsWithStage(ctx, stage); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pool wait did not honor cancellation", err)
	}
	after := s.ConnectionStats()
	if !reflect.DeepEqual(stages, []string{"ledger-background-admission", "ledger-version"}) || after.WaitCount <= before.WaitCount || after.WaitDuration <= before.WaitDuration {
		t.Fatal("pool starvation was not observable", stages, before, after)
	}
}
