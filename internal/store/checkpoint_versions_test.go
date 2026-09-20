package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckpointVersionAuditBoundedAndSkipsExternalNormalizers(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	checkpoint, _ := json.Marshal(map[string]any{"version": strings.Repeat("v", 1000), "counters": strings.Repeat("x", 3<<20)})
	for _, id := range []string{"normal", "external"} {
		if _, err := s.db.Exec(`INSERT INTO source_identity VALUES(?,?,?,?,?)`, id, "machine", "claude", "", "g"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO sources VALUES(?,?,?,?,?,?,?,?)`, id, "g", `{}`, 10, 10, checkpoint, "now", "now"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetExternalIndex(ctx, "external", true); err != nil {
		t.Fatal(err)
	}
	page, err := s.SourceCheckpointVersions(ctx, "", 100)
	if err != nil || len(page) != 1 || page[0].SourceID != "normal" || len(page[0].Version) != 256 || !page[0].Valid {
		t.Fatal("unbounded checkpoint or wrong normalizer contract", page, err)
	}
	if _, err := s.db.Exec(`UPDATE sources SET parser_state='null' WHERE source_id='normal'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.SourceCheckpointVersions(ctx, "", 100)
	if err != nil || len(page) != 1 || page[0].Valid {
		t.Fatal("null checkpoint called valid", page, err)
	}
	if _, err := s.db.Exec(`UPDATE sources SET parser_state='broken' WHERE source_id='normal'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.SourceCheckpointVersions(ctx, "", 100)
	if err != nil || len(page) != 1 || page[0].Valid {
		t.Fatal("invalid JSON invoked unguarded json_extract", page, err)
	}
}
