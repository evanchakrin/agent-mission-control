package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestHookSummaryIncludesArchiveAndSeparatesCoverage(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	empty, err := s.JavaScriptHookSummary(ctx)
	if err != nil || empty.Sessions != 0 || empty.Proposed || len(empty.Evidence.Examples) != 0 {
		t.Fatal(empty, err)
	}
	for i := 0; i < 10; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("source-%d", i)
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprintf("session-%d", i)
		b.Events = []Event{{ID: fmt.Sprintf("edit-%d", i), Kind: "tool-call", SearchText: "app.js", Data: json.RawMessage(`{"tool":"Edit"}`), SourceLength: 2}, {ID: fmt.Sprintf("result-%d", i), Kind: "tool-result", SearchText: "app.js SyntaxError", SourceLength: 2}}
		if i == 8 {
			b.Events = append(b.Events, Event{ID: "diagnostic", Kind: "indexing-error", Text: "unsupported", SourceLength: 2})
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		yes := true
		if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: fmt.Sprintf("archive-%d", i), Archived: &yes}); err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			ingest(t, s, src, 2, "pending\n")
		}
	}
	// Fixture-only old and staged state must not inflate current coverage.
	if _, err := s.db.Exec(`DELETE FROM hook_javascript_evidence WHERE source_id='source-9';
 INSERT INTO hook_javascript_evidence VALUES('source-0','generation-1','unpublished',2,1,999,0)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.JavaScriptHookSummary(ctx)
	if err != nil || got.Sessions != 10 || got.NeedsRebuild != 1 || got.Incomplete != 2 || !got.Proposed || got.Evidence.SessionsRead != 7 || got.Evidence.SessionsEdited != 7 || got.Evidence.SessionsWithErrors != 7 || got.Evidence.Errors != 7 || len(got.Evidence.Examples) != 6 {
		t.Fatal(got, err)
	}
	for i, e := range got.Evidence.Examples {
		if e.SessionID != fmt.Sprintf("session-%d", i) || e.Errors != 1 {
			t.Fatal(got.Evidence.Examples)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.JavaScriptHookSummary(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
