package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestBehaviorToolsBackfillDeltasAndRollback(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{
		{ID: "tool-a", Kind: "tool-call", AgentID: "main", SourceLength: 2, Data: json.RawMessage(`{"tool":"Read"}`)},
		{ID: "tool-b", Kind: "tool-call", AgentID: "helper", SourceLength: 2, Data: json.RawMessage(`{"tool":"mcp__fixture__Read"}`)},
		{ID: "not-tool", Kind: "message", AgentID: "main", SourceLength: 2},
	}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureBehaviorTools(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(want map[string]int) {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `SELECT tool,SUM(calls) FROM query_flow_tools_v1 GROUP BY tool`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]int{}
		for rows.Next() {
			var name string
			var n int
			if err := rows.Scan(&name, &n); err != nil {
				t.Fatal(err)
			}
			got[name] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal(got, want)
		}
	}
	check(map[string]int{"Read": 2})
	if err := s.ensureBehaviorTools(ctx); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"Read": 2})
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET data='{"tool":"Write"}' WHERE id='tool-a'`); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"Read": 1, "Write": 1})
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET kind='tool-call',data='{"tool":7}' WHERE id='not-tool'`); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"Read": 1, "Write": 1, "(unattributed tool)": 1})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"Read": 1, "Write": 1, "(unattributed tool)": 1})
	if _, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE id='tool-b'; UPDATE events SET kind='message' WHERE id='tool-a'`); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"(unattributed tool)": 1})
	// An interrupted/partial auxiliary schema is rebuilt atomically from evidence.
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER flow_tools_v1_update; UPDATE query_flow_tools_v1 SET calls=99`); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureBehaviorTools(ctx); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{"(unattributed tool)": 1})
	if _, err := s.db.ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	check(map[string]int{})
}

func TestBehaviorToolsFailedBackfillLeavesEvidenceAndRetries(t *testing.T) {
	for _, stage := range []string{"before-build", "before-commit", "cancel-before-commit", "probe-error"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			b.Events = []Event{{ID: "call", Kind: "tool-call", SourceLength: 2, Data: json.RawMessage(`{"tool":"Read"}`)}}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetSession(ctx, b.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS flow_tools_v1_insert; DROP TRIGGER IF EXISTS flow_tools_v1_update; DROP TRIGGER IF EXISTS flow_tools_v1_delete; DROP TABLE IF EXISTS query_flow_tools_v1;`); err != nil {
				t.Fatal(err)
			}
			buildCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			checks := 0
			probeErr := errors.New("fixture capacity probe unavailable")
			s.options.ReserveBytes = 1
			s.options.AvailableBytes = func(string) (int64, error) {
				checks++
				if stage == "probe-error" {
					return 0, probeErr
				}
				if stage == "before-build" && checks == 1 || stage == "before-commit" && checks == 2 {
					return 1, nil
				}
				if stage == "cancel-before-commit" && checks == 2 {
					cancel()
				}
				return 1 << 30, nil
			}
			err = s.ensureBehaviorTools(buildCtx)
			want := ErrCapacity
			if stage == "cancel-before-commit" {
				want = context.Canceled
			}
			if stage == "probe-error" {
				want = probeErr
			}
			if !errors.Is(err, want) {
				t.Fatal(err, want)
			}
			var objects int
			if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='query_flow_tools_v1' OR name LIKE 'flow_tools_v1_%'`).Scan(&objects); err != nil || objects != 0 {
				t.Fatal("failed backfill published objects", objects, err)
			}
			after, err := s.GetSession(ctx, b.Session.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("source projection changed", err)
			}
			s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
			if err = s.ensureBehaviorTools(ctx); err != nil {
				t.Fatal(err)
			}
			var calls int
			if err = s.db.QueryRowContext(ctx, `SELECT SUM(calls) FROM query_flow_tools_v1`).Scan(&calls); err != nil || calls != 1 {
				t.Fatal("retry lost or duplicated event", calls, err)
			}
		})
	}
}
