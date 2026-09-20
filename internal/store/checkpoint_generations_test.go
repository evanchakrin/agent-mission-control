package store

import (
	"context"
	"testing"
)

func TestVersionAuditSkipsSupersededGenerationsWithoutRemovingEvidence(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err = s.db.Exec(`INSERT INTO source_identity VALUES('source','machine','codex','native','new')`); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"old", "new"} {
		if _, err = s.db.Exec(`INSERT INTO sources VALUES('source',?,'{}',10,10,'{"version":"3"}','now','now')`, generation); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.SourceCheckpointVersions(ctx, "", 1)
	if err != nil || len(page) != 1 || page[0].Generation != "new" {
		t.Fatal("non-actionable generation in audit", page, err)
	}
	end, err := s.SourceCheckpointVersions(ctx, CheckpointVersionCursor(page[0]), 1)
	if err != nil || len(end) != 0 {
		t.Fatal("superseded generation leaked into next page", end, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM sources`).Scan(&count); err != nil || count != 2 {
		t.Fatal("historical generation removed", count, err)
	}
}
