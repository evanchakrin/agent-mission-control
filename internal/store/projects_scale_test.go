package store

import (
	"context"
	"runtime/pprof"
	"testing"
	"time"
)

func TestProjectDeletionTriggerDiagnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable trigger isolation diagnostic")
	}
	for _, removed := range []string{"delete_insert_search", "none", "catalog_search_update", "query_metadata_update"} {
		t.Run(removed, func(t *testing.T) {
			s := openTestStore(t, Options{})
			seedAnalyticsCatalog(t, s, 5000)
			if _, err := s.db.Exec(`INSERT INTO session_metadata SELECT id,1,json_object('project','large-project','projectOverride',json('true')) FROM sessions`); err != nil {
				t.Fatal(err)
			}
			// Deliberately break only this disposable projection to isolate costs.
			// These runs are diagnostics, never correctness acceptance evidence.
			if removed == "delete_insert_search" {
				if _, err := s.db.Exec(`DROP TRIGGER catalog_search_update;
CREATE TRIGGER catalog_search_update AFTER UPDATE ON query_sessions
WHEN OLD.title IS NOT NEW.title OR OLD.project IS NOT NEW.project OR OLD.machine_id IS NOT NEW.machine_id OR OLD.native_id IS NOT NEW.native_id
BEGIN DELETE FROM catalog_search WHERE rowid IN (SELECT q.rowid FROM query_sessions q WHERE q.id=NEW.id);
INSERT INTO catalog_search(rowid,text) SELECT q.rowid,` + catalogSearchText + ` FROM query_sessions q WHERE q.id=NEW.id; END;`); err != nil {
					t.Fatal(err)
				}
			} else if removed != "none" {
				if _, err := s.db.Exec("DROP TRIGGER " + removed); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			_, err := s.db.Exec(`UPDATE session_metadata SET revision=revision+1,value=json_set(value,'$.project','')`)
			t.Logf("update5000=%s", time.Since(start))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProjectDeletion100001MembershipsPreservesHistory(t *testing.T) {
	testLargeProjectDeletion(t, false, false, false)
}

func TestQueuedProjectDeletion100001MembershipsPreservesHistory(t *testing.T) {
	testLargeProjectDeletion(t, false, false, true)
}

func TestProjectDeletionWithoutBackgroundCheckpointDiagnostic(t *testing.T) {
	testLargeProjectDeletion(t, true, false, false)
}

func TestProjectDeletionBoundedCacheDiagnostic(t *testing.T) {
	testLargeProjectDeletion(t, false, true, false)
}

func testLargeProjectDeletion(t *testing.T, stopBackground, enlargedCache, queued bool) {
	if testing.Short() {
		t.Skip("100,001-membership project deletion fixture")
	}
	s := openTestStore(t, Options{})
	if enlargedCache {
		// Keep all four connections checked out while configuring so each distinct
		// pooled connection is covered. This is not a production configuration.
		func() {
			for i := 0; i < 4; i++ {
				conn, err := s.db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				var previous int
				if err = conn.QueryRowContext(context.Background(), `PRAGMA cache_size`).Scan(&previous); err != nil {
					t.Fatal(err)
				}
				if _, err = conn.ExecContext(context.Background(), `PRAGMA cache_size=-8192`); err != nil {
					t.Fatal(err)
				}
				t.Logf("diagnostic connection %d: cache_size %d -> -8192; FULL sync unchanged", i, previous)
			}
		}()
	}
	seedStart := time.Now()
	seedAnalyticsCatalog(t, s, 100002)
	t.Logf("seed100002=%s", time.Since(seedStart))
	ctx := context.Background()
	project, err := s.MutateProject(ctx, ProjectMutation{ID: "large-project", Name: "Large", Color: "#60a5fa", OperationID: "create-large"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed only disposable metadata. The last session is an unrelated sentinel.
	assignmentStart := time.Now()
	_, err = s.db.Exec(`INSERT INTO session_metadata(session_id,revision,value)
		SELECT id,1,json_object('sessionId',id,'revision',1,
		'project',CASE WHEN id='s-100001' THEN 'unrelated' ELSE 'large-project' END,
		'projectOverride',json('true'),'archived',json('true'),'pinned',json('true'),
		'note','preserved note','tags',json('["preserved"]')) FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("assign100002=%s", time.Since(assignmentStart))
	if stopBackground {
		// Isolate only the disposable store's background I/O after normal setup.
		// FULL commit sync remains enabled. This variant is not release evidence.
		s.checkpointStop()
		<-s.checkpointDone
		t.Log("diagnostic: checkpoint loop stopped; FULL durability unchanged")
	}
	remove := ProjectMutation{ID: project.ID, Revision: project.Revision, Delete: true, OperationID: "delete-large"}
	deleteContext, cancelDelete := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDelete()
	start := time.Now()
	var deleted Project
	if queued {
		var job ProjectDeletion
		job, err = s.QueueProjectDeletion(deleteContext, remove)
		if err != nil {
			t.Fatal(err)
		}
		accepted := time.Since(start)
		if accepted > 10*time.Second {
			t.Errorf("acceptance exceeded owner deadline: %s", accepted)
		}
		var longest time.Duration
		batches := 0
		for job.State != "complete" {
			batch, cancel := context.WithTimeout(ctx, 10*time.Second)
			before := time.Now()
			job, err = s.AdvanceProjectDeletion(batch, remove.OperationID, 100)
			elapsed := time.Since(before)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if elapsed > longest {
				longest = elapsed
			}
			if elapsed > 10*time.Second {
				t.Errorf("batch exceeded owner deadline: %s", elapsed)
			}
			batches++
		}
		if job.Processed != 100001 {
			t.Fatal(job)
		}
		deleted = job.Project
		t.Logf("queued acceptance=%s batches=%d longestBatch=%s", accepted, batches, longest)
	} else {
		pprof.Do(deleteContext, pprof.Labels("amc-phase", "project-delete"), func(measured context.Context) { deleted, err = s.MutateProject(measured, remove) })
	}
	deleteElapsed := time.Since(start)
	t.Logf("delete100001=%s", deleteElapsed)
	if err != nil || !deleted.Deleted {
		t.Fatal(deleted, err)
	}
	// Driver cancellation is cooperative and may not interrupt durable commit.
	// A nil error after the deadline is correctness evidence, not a latency pass.
	if !queued && deleteElapsed > 10*time.Second {
		t.Errorf("project deletion exceeded owner deadline: %s > 10s", deleteElapsed)
	}
	if replay, err := s.MutateProject(ctx, remove); err != nil || replay != deleted {
		t.Fatal(replay, err)
	}
	checks := []struct {
		query string
		want  int
	}{
		{`SELECT count(*) FROM sessions`, 100002},
		{`SELECT count(*) FROM usage_observations`, 100002},
		{`SELECT count(*) FROM session_metadata WHERE revision=2 AND json_extract(value,'$.revision')=2 AND json_extract(value,'$.project')='' AND json_extract(value,'$.projectOverride')=1 AND json_extract(value,'$.archived')=1 AND json_extract(value,'$.pinned')=1 AND json_extract(value,'$.note')='preserved note' AND json_extract(value,'$.tags[0]')='preserved'`, 100001},
		{`SELECT count(*) FROM session_metadata WHERE revision=1 AND json_extract(value,'$.project')='unrelated'`, 1},
		{`SELECT count(*) FROM organization_audit WHERE revision=2 AND json_extract(before_json,'$.project')='large-project' AND json_extract(after_json,'$.project')=''`, 100001},
		{`SELECT count(*) FROM changes WHERE kind='metadata'`, 100001},
		{`SELECT count(*) FROM query_sessions WHERE project='' AND archived=1 AND pinned=1`, 100001},
		{`SELECT count(*) FROM catalog_search WHERE instr(text,'large-project')>0`, 0},
		{`SELECT count(*) FROM catalog_search WHERE instr(text,'preserved note')>0`, 100002},
	}
	for _, check := range checks {
		var got int
		if err := s.db.QueryRowContext(ctx, check.query).Scan(&got); err != nil || got != check.want {
			t.Fatalf("%s: got %d want %d: %v", check.query, got, check.want, err)
		}
	}
}
