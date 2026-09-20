package store

import (
	"context"
	"testing"
)

func TestRevisionMigrationPreservesV1RowsAndSearch(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "source bytes\n")
	b := batch(src, 0, 13)
	b.Events = []Event{{ID: "old", Text: "preview", SearchText: "full original searchable needle", SourceLength: 13}}
	b.Usage = []UsageObservation{{ID: "old-usage", TokensIn: 13}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PatchMetadata(ctx, "session-1", MetadataPatch{OperationID: "old-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-revision schema without altering its source/projection rows
	// or FTS rowids. The real upgrade must add identity without re-reading raw.
	dropLedgerTotalsForLegacyFixture(t, s)
	_, err := s.db.Exec(`DROP INDEX events_dedupe;DROP INDEX events_revision;DROP INDEX usage_revision;
 ALTER TABLE events DROP COLUMN projection_revision;ALTER TABLE usage_observations DROP COLUMN projection_revision;
 DROP TABLE active_projection;DROP TABLE projection_revisions;DROP TABLE baseline_projections;
 CREATE UNIQUE INDEX events_dedupe ON events(session_id,source_id,generation,dedupe_key) WHERE dedupe_key<>'';
 PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, err := s.GetSession(ctx, "session-1")
	if err != nil || row.TokensIn != 13 || !row.Metadata.Archived || row.ProjectionRevision != "" {
		t.Fatal(row, err)
	}
	found, err := s.Search(ctx, SearchQuery{Text: "searchable needle"})
	if err != nil || len(found.Events) != 1 || found.Events[0].ID != "old" {
		t.Fatal("full FTS evidence changed during migration", found, err)
	}
	usage, err := s.GetUsage(ctx, row.ID, "", 100)
	if err != nil || len(usage) != 1 || usage[0].ID != "old-usage" || usage[0].TokensIn != 13 {
		t.Fatal(usage, err)
	}
}
