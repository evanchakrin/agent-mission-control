package store

import (
	"context"
	"testing"
)

func TestOrganizationAuditCommitsExactlyOnceWithMutation(t *testing.T) {
	s := openTestStore(t, Options{})
	src := testSource()
	ctx := context.Background()
	ingest(t, s, src, 0, "x\n")
	if err := s.CommitIndex(ctx, batch(src, 0, 2)); err != nil {
		t.Fatal(err)
	}
	yes := true
	p := MetadataPatch{Revision: 0, OperationID: "archive-audit", Archived: &yes}
	for i := 0; i < 2; i++ {
		if _, err := s.PatchMetadata(ctx, "session-1", p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.OrganizationHistory(ctx, "session-1", 0, 100)
	if err != nil || len(got.Entries) != 1 {
		t.Fatal(got, err)
	}
	e := got.Entries[0]
	if e.Before.Archived || !e.After.Archived || e.Revision != 1 || e.Patch.OperationID != p.OperationID {
		t.Fatal(e)
	}
	p.OperationID = "stale-audit"
	if _, err = s.PatchMetadata(ctx, "session-1", p); err == nil {
		t.Fatal("stale accepted")
	}
	got, err = s.OrganizationHistory(ctx, "session-1", 0, 100)
	if err != nil || len(got.Entries) != 1 {
		t.Fatal("rejected mutation produced audit", got, err)
	}
}
