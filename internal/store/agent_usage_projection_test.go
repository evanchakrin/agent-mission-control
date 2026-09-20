package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAgentUsageSummaryMatchesRawRevisionsAndBackfill(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	check := func() {
		t.Helper()
		var differences int
		err := s.db.QueryRow(`WITH raw AS (
 SELECT session_id,source_id,generation,projection_revision,agent_id,count(*) observations,SUM(tokens_in) tokens_in,SUM(tokens_cache) tokens_cache,SUM(tokens_write) tokens_write,SUM(tokens_out) tokens_out,
 SUM(CASE WHEN model='' OR kind IN ('incomplete-attribution','message-without-id','message-partial') THEN tokens_in+tokens_cache+tokens_write+tokens_out ELSE 0 END) unknown_tokens
 FROM query_usage GROUP BY session_id,source_id,generation,projection_revision,agent_id),
 missing AS (SELECT * FROM raw EXCEPT SELECT * FROM query_agent_usage), extra AS (SELECT * FROM query_agent_usage EXCEPT SELECT * FROM raw)
 SELECT (SELECT count(*) FROM missing)+(SELECT count(*) FROM extra)`).Scan(&differences)
		if err != nil || differences != 0 {
			t.Fatal("agent summary differs from observations", differences, err)
		}
	}
	check()
	for _, stmt := range []string{
		`INSERT INTO query_usage SELECT 'agent-partial',session_id,source_id,generation,projection_revision,'helper','known-model','message-partial',timestamp,7,2,3,4 FROM query_usage WHERE id='usage-s-000000'`,
		`INSERT INTO query_usage SELECT 'agent-missing',session_id,source_id,generation,projection_revision,'helper','known-model','message-without-id',timestamp,1,0,0,0 FROM query_usage WHERE id='usage-s-000000'`,
		`INSERT INTO query_usage SELECT 'agent-zero',session_id,source_id,generation,projection_revision,'','known-model','token',timestamp,0,0,0,0 FROM query_usage WHERE id='usage-s-000000'`,
		`UPDATE query_usage SET agent_id='different-helper',generation='old',tokens_in=5,kind='token' WHERE id='agent-partial'`,
		`UPDATE query_usage SET model='',tokens_out=9 WHERE id='agent-partial'`,
		`DELETE FROM query_usage WHERE id='agent-missing'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
		check()
	}
	// Missing trigger simulates an incomplete derived-schema installation.
	if _, err := s.db.Exec(`DROP TRIGGER agent_usage_update; DELETE FROM query_agent_usage`); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureAgentUsageProjection(ctx); err != nil {
		t.Fatal(err)
	}
	check()
	if err := s.ensureAgentUsageProjection(ctx); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestAgentUsageSummarySurvivesReopenAndRoleQueryUsesSummary(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE query_usage SET agent_id='helper'; INSERT INTO agent_names VALUES('s-000000','helper','Reviewer')`); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{OperationID: "summary-reopen", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	before, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if before.TotalAppearances != 3 {
		t.Fatal("fixture lacks helper roles", before)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := reopened.BehaviorRoles(ctx, SessionQuery{Limit: 20})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("reopen changed roles", before, after, err)
	}
	session, err := reopened.GetSession(ctx, "s-000001")
	if err != nil || !session.Metadata.Archived {
		t.Fatal("archive changed", session.Metadata, err)
	}
	rows, err := reopened.db.Query("EXPLAIN QUERY PLAN " + fmt.Sprintf(behaviorRolesSQL, "1"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// A full-fleet query may scan the compact summary (reported only as "u")
	// instead of seeking its index. Do not require an index for three tiny rows.
	if !strings.Contains(behaviorRolesSQL, "FROM query_agent_usage u") || strings.Contains(behaviorRolesSQL, "FROM query_usage u") || strings.Contains(plan, "query_usage_session_agent") {
		t.Fatal("role query scans observation identities", plan)
	}
}

func TestAgentUsageSummaryFailedBackfillCanRetry(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	if _, err := s.db.Exec(`DROP TRIGGER agent_usage_update`); err != nil {
		t.Fatal(err)
	}
	checks := 0
	s.options.ReserveBytes = 1
	s.options.AvailableBytes = func(string) (int64, error) {
		checks++
		if checks == 2 {
			return 1, nil
		}
		return 1 << 30, nil
	}
	if err := s.ensureAgentUsageProjection(context.Background()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM query_agent_usage`).Scan(&count); err != nil || count != 3 {
		t.Fatal("failed backfill lost prior rows", count, err)
	}
	s.options.AvailableBytes = func(string) (int64, error) { return 1 << 30, nil }
	if err := s.ensureAgentUsageProjection(context.Background()); err != nil {
		t.Fatal(err)
	}
}
