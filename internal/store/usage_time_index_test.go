package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestUsageTimeIndexPreservesRhythmAndSeeksTimestamp(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS query_usage_scope_time;
 INSERT INTO query_usage SELECT 'earliest-probe',id,source_id,generation,projection_revision,'different-agent','different-model','token','2025-12-01T03:00:00.000000000Z',1,0,0,1 FROM query_sessions WHERE id='s-000000';
 INSERT INTO query_usage SELECT 'wrong-scope-probe',id,'wrong-source',generation,projection_revision,'different-agent','different-model','token','2020-01-01T00:00:00.000000000Z',1,0,0,1 FROM query_sessions WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "time-index-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	q := RhythmQuery{Timezone: "UTC"}
	before, err := s.Rhythm(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ensureUsageTimeIndex(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.SetupAnalytics(ctx); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Rhythm(ctx, q)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("index changed rhythm", err, before, after)
	}
	var archived int
	if err := s.db.QueryRow(`SELECT archived FROM query_sessions WHERE id='s-000001'`).Scan(&archived); err != nil || archived != 1 {
		t.Fatal("organization changed", archived, err)
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN " + rhythmSQL("1"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "USING COVERING INDEX query_usage_scope_time") && strings.Contains(detail, "timestamp>?") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("earliest usage does not use a scoped timestamp seek")
	}
}

func TestUsageTimeIndexFailureRollsBackAndRetries(t *testing.T) {
	for _, stage := range []string{"before-build", "before-commit", "cancel-before-commit", "probe-error"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t, Options{})
			seedAnalyticsCatalog(t, s, 3)
			ctx := context.Background()
			yes := true
			if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "failure-archive", Archived: &yes}); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetSession(ctx, "s-000001")
			if err != nil {
				t.Fatal(err)
			}
			q := RhythmQuery{Timezone: "UTC"}
			rhythmBefore, err := s.Rhythm(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DROP INDEX query_usage_scope_time`); err != nil {
				t.Fatal(err)
			}
			buildCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			checks := 0
			probeError := errors.New("storage probe unavailable")
			s.options.ReserveBytes = 1
			s.options.AvailableBytes = func(string) (int64, error) {
				checks++
				if stage == "probe-error" {
					return 0, probeError
				}
				if (stage == "before-build" && checks == 1) || (stage == "before-commit" && checks == 2) {
					return 1, nil
				}
				if stage == "cancel-before-commit" && checks == 2 {
					cancel()
				}
				return 1 << 30, nil
			}
			err = s.ensureUsageTimeIndex(buildCtx)
			want := ErrCapacity
			if stage == "probe-error" {
				want = probeError
			}
			if stage == "cancel-before-commit" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='query_usage_scope_time'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed build published index", count, err)
			}
			s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
			after, err := s.GetSession(ctx, "s-000001")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("session or organization changed", err)
			}
			if err := s.ensureUsageTimeIndex(ctx); err != nil {
				t.Fatal("retry failed", err)
			}
			rhythmAfter, err := s.Rhythm(ctx, q)
			if err != nil || !reflect.DeepEqual(rhythmBefore, rhythmAfter) {
				t.Fatal("failure/retry changed rhythm", err)
			}
			s.options.AvailableBytes = func(string) (int64, error) { return 1, nil }
			if err := s.ensureUsageTimeIndex(ctx); err != nil {
				t.Fatal("existing index requires build space", err)
			}
		})
	}
}
