package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestHookCheckpointDamageBlocksReadinessAndPublication(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, damage := range []string{"missing", "offset", "errors", "diagnostics", "edit"} {
			t.Run(fmt.Sprintf("ready=%t/%s", ready, damage), func(t *testing.T) {
				s := openTestStore(t, Options{})
				ctx, src := context.Background(), testSource()
				ingest(t, s, src, 0, "x\n")
				if err := s.CommitIndex(ctx, batch(src, 0, 2)); err != nil {
					t.Fatal(err)
				}
				yes := true
				if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "archive", Archived: &yes}); err != nil {
					t.Fatal(err)
				}
				before, err := s.GetSession(ctx, "session-1")
				if err != nil {
					t.Fatal(err)
				}
				r, err := s.BeginRebuild(ctx, "session-1", "damaged-hooks", "3")
				if err != nil {
					t.Fatal(err)
				}
				b := batch(src, 0, 2)
				b.ProjectionRevision = r.Revision
				b.ParserState = json.RawMessage(`{"version":"3"}`)
				b.Events = []Event{{ID: "edit", Kind: "tool-call", SearchText: "app.js", Data: json.RawMessage(`{"tool":"Edit"}`), SourceLength: 2}, {ID: "error", Kind: "tool-result", SearchText: "app.js SyntaxError", SourceLength: 2}}
				if err := s.CommitIndex(ctx, b); err != nil {
					t.Fatal(err)
				}
				if ready {
					if err := s.ReadyRebuild(ctx, r.Revision); err != nil {
						t.Fatal(err)
					}
				}
				statements := map[string]string{"missing": "DELETE FROM hook_javascript_evidence WHERE revision=?", "offset": "UPDATE hook_javascript_evidence SET indexed_offset=1 WHERE revision=?", "errors": "UPDATE hook_javascript_evidence SET errors=99 WHERE revision=?", "diagnostics": "UPDATE hook_javascript_evidence SET diagnostics=99 WHERE revision=?", "edit": "UPDATE hook_javascript_evidence SET edited=0 WHERE revision=?"}
				if _, err := s.db.ExecContext(ctx, statements[damage], r.Revision); err != nil {
					t.Fatal(err)
				}
				if ready {
					err = s.PublishRebuild(ctx, r.Revision)
				} else {
					err = s.ReadyRebuild(ctx, r.Revision)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("damaged evidence passed", err)
				}
				after, err := s.GetSession(ctx, "session-1")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("failed guard changed visible history", err)
				}
			})
		}
	}
}

func TestHookEvidenceDoesNotTreatPreviewOnlyAsComplete(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx, src := context.Background(), testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{{ID: "preview", Kind: "tool-result", Text: "truncated output", SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.JavaScriptHookEvidence(ctx, "session-1", "")
	if err != nil || evidence.State != "incomplete-indexing" || evidence.Diagnostics != 1 {
		t.Fatal(evidence, err)
	}
}

func TestHookEvidenceStagedRebuildIsInvisibleUntilPublication(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx, src := context.Background(), testSource()
	ingest(t, s, src, 0, "x\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 2)); err != nil {
		t.Fatal(err)
	}
	before, err := s.JavaScriptHookEvidence(ctx, "session-1", "")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "archive-hooks", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	r, err := s.BeginRebuild(ctx, "session-1", "hook-evidence", "3")
	if err != nil {
		t.Fatal(err)
	}
	b := batch(src, 0, 2)
	b.ProjectionRevision = r.Revision
	b.ParserState = json.RawMessage(`{"version":"3"}`)
	b.Events = []Event{{ID: "edit", Kind: "tool-call", SearchText: "app.js", Data: json.RawMessage(`{"tool":"Edit"}`), SourceLength: 2}, {ID: "error", Kind: "tool-result", SearchText: "app.js SyntaxError", SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	staged, err := s.JavaScriptHookEvidence(ctx, "session-1", before.Snapshot)
	if err != nil || staged != before {
		t.Fatal("unpublished evidence leaked", staged, err)
	}
	if err := s.ReadyRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRebuild(ctx, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JavaScriptHookEvidence(ctx, "session-1", before.Snapshot); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	current, err := s.JavaScriptHookEvidence(ctx, "session-1", "")
	if err != nil || !current.Edited || current.Errors != 1 || current.State != "indexed-history" {
		t.Fatal(current, err)
	}
	owner, err := s.GetSession(ctx, "session-1")
	if err != nil || !owner.Metadata.Archived {
		t.Fatal("publication changed archive", err)
	}
}

func TestHookEvidenceAtomicDeduplicatedAndExplicitlyIncomplete(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx, src := context.Background(), testSource()
	ingest(t, s, src, 0, "x\nx\nx\nx\n")
	b := batch(src, 0, 2)
	b.Events = []Event{{ID: "edit", Kind: "tool-call", Text: "preview", SearchText: `{"file_path":"app.js"}`, Data: json.RawMessage(`{"tool":"Edit"}`), SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, err := s.JavaScriptHookEvidence(ctx, "session-1", "")
	if err != nil || first.State != "indexed-history" || !first.Edited || first.Errors != 0 || first.IndexedOffset != 2 {
		t.Fatal(first, err)
	}
	b = batch(src, 2, 4)
	b.Events = []Event{{ID: "failure", Kind: "tool-result", Text: "preview", SearchText: strings.Repeat("output ", 2000) + " app.js SyntaxError", SourceOffset: 2, SourceLength: 2}}
	// An invalid later observation must roll back events and evidence together.
	b.Usage = []UsageObservation{{ID: "invalid", TokensIn: -1}}
	if err := s.CommitIndex(ctx, b); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	unchanged, err := s.JavaScriptHookEvidence(ctx, "session-1", first.Snapshot)
	if err != nil || unchanged != first {
		t.Fatal(unchanged, err)
	}
	b.Usage = nil
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	b = batch(src, 4, 6)
	b.Events = []Event{{ID: "failure", Kind: "tool-result", SearchText: "app.js SyntaxError", SourceOffset: 2, SourceLength: 2}, {ID: "diagnostic", Kind: "indexing-error", Text: "unsupported record", SourceOffset: 4, SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.JavaScriptHookEvidence(ctx, "session-1", first.Snapshot)
	if err != nil || got.Errors != 1 || got.Diagnostics != 1 || got.State != "incomplete-indexing" || got.IndexedOffset != 6 {
		t.Fatal(got, err)
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
	restored, err := reopened.JavaScriptHookEvidence(ctx, "session-1", first.Snapshot)
	if err != nil || restored != got {
		t.Fatal(restored, err)
	}
	// Simulate an older projection that predates evidence, in this fixture only.
	if _, err := reopened.db.Exec("DELETE FROM hook_javascript_evidence"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CommitIndex(ctx, batch(src, 6, 8)); err != nil {
		t.Fatal(err)
	}
	missing, err := reopened.JavaScriptHookEvidence(ctx, "session-1", first.Snapshot)
	if err != nil || missing.State != "needs-rebuild" {
		t.Fatal(missing, err)
	}
}
