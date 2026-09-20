package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

// This measures archive latency overlapping publication, not collector throughput
// or complete resource certification. Source bytes are a tiny synthetic fixture.
func TestAttributionPublication100001ObservationsLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("100001-observation publication measurement")
	}
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Usage = nil
	b.Events = nil
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE n(i) AS(SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<100001)
 INSERT INTO usage_observations(id,session_id,source_id,generation,observation,projection_revision)
 SELECT printf('u-%06d',i),'session-1',?,?,json_object('id',printf('u-%06d',i),'sessionId','session-1','agentId','main','model','known','tokensIn',1,'tokensCache',0,'tokensCacheWrite',0,'tokensOut',0),'' FROM n`, src.SourceID, src.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO observation_prices SELECT 'scale',id,'main','known',json_object('observationId',id,'agentId','main','model','known','cost',0.000001,'recordedTokens',1,'pricedTokens',1,'unpricedTokens',0,'unattributedTokens',0,'rateIds',json('[]')) FROM usage_observations;
 UPDATE sessions SET projection=json_set(projection,'$.tokensIn',100001,'$.tokensOut',0,'$.tokensCache',0,'$.tokensCacheWrite',0) WHERE id='session-1';`)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("seed100001 observation prices=%s", time.Since(start))
	payload, _ := json.Marshal(map[string]any{"id": "scale", "sessionId": "session-1", "generation": src.Generation, "projectionRevision": "", "indexedOffset": 13, "catalogId": "catalog", "context": "test", "attributionVersion": 1, "observations": 100001, "estimate": map[string]any{"cost": 0.100001, "recordedTokens": 100001, "pricedTokens": 100001, "unpricedTokens": 0, "unattributedTokens": 0}})
	start = time.Now()
	done := make(chan error, 1)
	go func() { done <- s.SaveProjectionEstimate(ctx, "scale", "session-1", src.Generation, "", 13, payload) }()
	var timings []time.Duration
	var publishErr error
loop:
	for i := int64(0); ; i++ {
		select {
		case publishErr = <-done:
			break loop
		default:
		}
		archived := i%2 == 0
		at := time.Now()
		if _, err = s.PatchMetadata(ctx, "session-1", MetadataPatch{Archived: &archived, Revision: i, OperationID: fmt.Sprintf("archive-during-pricing-%d", i)}); err != nil {
			t.Fatal(err)
		}
		timings = append(timings, time.Since(at))
		time.Sleep(time.Millisecond)
	}
	if publishErr != nil {
		t.Fatal(publishErr)
	}
	if len(timings) < 5 {
		t.Fatal("insufficient overlapping archive measurements", len(timings))
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	p95 := timings[(len(timings)*95-1)/100]
	maximum := timings[len(timings)-1]
	t.Logf("100001-observation publication wall=%s; concurrent archive count=%d p95=%s max=%s", time.Since(start), len(timings), p95, maximum)
	if p95 >= 250*time.Millisecond || maximum >= 250*time.Millisecond {
		t.Fatalf("publication blocked archive writes: p95=%s max=%s", p95, maximum)
	}
}
