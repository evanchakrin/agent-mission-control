package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestModelHookSummaryCurrentAgentPairs(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	empty, err := s.ModelHookSummary(ctx)
	if err != nil || empty.Agents != 0 || empty.UnattributedContexts != 0 {
		t.Fatal(empty, err)
	}
	for i := 0; i < 2; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("model-source-%d", i)
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("model-session-%d", i)
		b.Events = []Event{{ID: fmt.Sprintf("event-%d", i), AgentID: "event-only", Kind: "message", SourceLength: 2}}
		b.Usage = []UsageObservation{
			{ID: fmt.Sprintf("known-%d", i), AgentID: "main", Model: "recorded-model", Kind: "delta"},
			{ID: fmt.Sprintf("repeat-%d", i), AgentID: "main", Model: "second-model", Kind: "delta"},
			{ID: fmt.Sprintf("missing-%d", i), AgentID: "missing", Kind: "delta", TokensIn: 10},
			{ID: fmt.Sprintf("partial-%d", i), AgentID: "partial", Model: "recorded-model", Kind: "incomplete-attribution"},
			{ID: fmt.Sprintf("unknown-%d", i), Model: "recorded-model", Kind: "delta"},
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		yes := true
		if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: fmt.Sprintf("archive-model-%d", i), Archived: &yes}); err != nil {
			t.Fatal(err)
		}
	}
	// Unpublished usage is retained but must not change the current result.
	if _, err := s.db.Exec(`INSERT INTO query_usage SELECT 'staged',id,source_id,generation,'unpublished','phantom','model','delta','',1,0,0,0 FROM query_sessions LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelHookSummary(ctx)
	if err != nil || got.Agents != 8 || got.WithRecordedModel != 2 || got.WithoutRecordedModel != 6 || got.UnattributedContexts != 2 {
		t.Fatal(got, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ModelHookSummary(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
