package store

import (
	"context"
	"testing"
	"time"
)

// Directly seeds a query workload, not captured raw evidence. This cannot
// certify source durability, parser correctness, 20 GiB storage, or fleet soak.
func TestTroubleFiles100001SessionQueryWorkload(t *testing.T) {
	runTroubleFilesScaleFixture(t, false)
}

func TestPatternGrouping100001SessionExperiment(t *testing.T) {
	runTroubleFilesScaleFixture(t, true)
}

func runTroubleFilesScaleFixture(t *testing.T, patternExperiment bool) {
	if testing.Short() {
		t.Skip("100001-session Trouble Files query workload")
	}
	s := openTestStore(t, Options{})
	seedStart := time.Now()
	seedAnalyticsCatalog(t, s, 100001)
	t.Logf("catalog seed=%s", time.Since(seedStart))
	ctx := context.Background()
	statements := []string{
		`INSERT INTO source_identity SELECT source_id,machine_id,provider,id,generation FROM sessions`,
		`INSERT INTO sources(source_id,generation,meta_json,created_at,updated_at) SELECT source_id,generation,json_object('machineId',machine_id,'sourceId',source_id,'generation',generation,'provider',provider,'modifiedAt','2026-01-01T00:00:00Z'),'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z' FROM sessions`,
		`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'call-'||id,id,source_id,generation,'main','tool-call','2026-01-01T00:00:00Z',0,0,'synthetic edit','{"tool":"Edit","toolUseId":"edit"}','','' FROM sessions`,
		`INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'result-'||id,id,source_id,generation,'main','tool-result','2026-01-01T00:00:01Z',0,0,'synthetic result',json_object('toolUseId','edit','error',json(CASE WHEN CAST(substr(id,3) AS INTEGER)%10=0 THEN 'true' ELSE 'false' END)),'','' FROM sessions`,
		`INSERT INTO file_edit_events SELECT seq,source_id,generation,projection_revision,'shared.go','Edit' FROM events WHERE kind='tool-call'`,
		`INSERT INTO file_edit_checkpoints SELECT source_id,generation,'',0,1,0 FROM sessions`,
	}
	evidenceStart := time.Now()
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("synthetic 200002 events and 100001 file touches=%s", time.Since(evidenceStart))
	if patternExperiment {
		testPatternGroupingExperiment(t, s)
		return
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	request, cancel := context.WithTimeout(ctx, 10*time.Second)
	start := time.Now()
	page, err := s.TroubleFiles(request, "", 20, now)
	elapsed := time.Since(start)
	cancel()
	t.Logf("first file page=%s", elapsed)
	if err != nil {
		t.Fatal("first file page failed owner deadline", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("first file page exceeded owner deadline: %s", elapsed)
	}
	if page.TotalSessions != 100001 || page.IndexedSessions != 100001 || len(page.Files) != 20 || page.NextCursor == "" {
		t.Fatal(page)
	}
	for _, f := range page.Files {
		if f.Sessions < 5 || f.UnknownSessions != 0 || f.Rate == nil {
			t.Fatal("incorrect large-catalog evidence", f)
		}
	}
	request, cancel = context.WithTimeout(ctx, 10*time.Second)
	start = time.Now()
	next, err := s.TroubleFiles(request, page.NextCursor, 20, now)
	elapsed = time.Since(start)
	cancel()
	t.Logf("second file page=%s", elapsed)
	if err != nil || len(next.Files) != 20 || next.Snapshot != page.Snapshot {
		t.Fatal(next, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("second file page exceeded owner deadline: %s", elapsed)
	}
	f := page.Files[0]
	request, cancel = context.WithTimeout(ctx, 10*time.Second)
	start = time.Now()
	refs, err := s.TroubleFileSessions(request, f.MachineID, f.Project, f.Path, "", 25, now)
	elapsed = time.Since(start)
	cancel()
	t.Logf("file session drilldown=%s", elapsed)
	if err != nil || refs.Total != f.Sessions || len(refs.Sessions) != 25 {
		t.Fatal(refs, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("drilldown exceeded owner deadline: %s", elapsed)
	}
	request, cancel = context.WithTimeout(ctx, 10*time.Second)
	start = time.Now()
	candidates, err := s.UnsavedCandidates(request, "m0", "", 20)
	elapsed = time.Since(start)
	cancel()
	t.Logf("local machine unsaved candidate page=%s", elapsed)
	if err != nil || candidates.TotalSessions != 4001 || candidates.IndexedSessions != 4001 || candidates.TotalFiles != 71 || len(candidates.Files) != 20 || candidates.NextCursor == "" {
		t.Fatal(candidates, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("candidate page exceeded owner deadline: %s", elapsed)
	}
	request, cancel = context.WithTimeout(ctx, 10*time.Second)
	continuation, err := s.UnsavedCandidates(request, "m0", candidates.NextCursor, 20)
	cancel()
	if err != nil || len(continuation.Files) != 20 || continuation.Snapshot != candidates.Snapshot {
		t.Fatal(continuation, err)
	}
	seen := map[string]bool{}
	for _, candidate := range append(candidates.Files, continuation.Files...) {
		if candidate.MachineID != "m0" || candidate.Sessions < 56 || seen[candidate.Project] {
			t.Fatal("lost machine scope, per-file count or continuation", candidate)
		}
		seen[candidate.Project] = true
	}
	t.Run("flows", func(t *testing.T) { testBehaviorFlowsWorkload(t, s) })
	// Keep diagnostics after the owner checks so they cannot warm those queries.
	testTroubleGroupingComparison(t, s, now)
}
