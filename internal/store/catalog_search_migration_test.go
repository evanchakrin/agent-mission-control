package store

import (
	"context"
	"strings"
	"testing"
)

func TestCatalogSearchUpdateMigrationPreservesRowsAndSearch(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	seedAnalyticsCatalog(t, s, 3)
	// Mimic the previous deployed schema without rewriting its indexed contents.
	if _, err := s.db.Exec(`UPDATE properties SET value='1' WHERE key='catalog_search_schema'; DROP TRIGGER catalog_search_update;
CREATE TRIGGER catalog_search_update AFTER UPDATE ON query_sessions BEGIN SELECT 1; END;
CREATE TRIGGER forbid_catalog_rebuild BEFORE DELETE ON query_sessions BEGIN SELECT RAISE(ABORT,'catalog rebuilt'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureCatalogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureCatalogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	var schema, trigger string
	if err := s.db.QueryRow(`SELECT value FROM properties WHERE key='catalog_search_schema'`).Scan(&schema); err != nil || schema != "4" {
		t.Fatal(schema, err)
	}
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='catalog_search_update'`).Scan(&trigger); err != nil || !strings.Contains(trigger, "UPDATE catalog_search SET") {
		t.Fatal(trigger, err)
	}
	name, project, note := "Renamed fixture", "new-project", "retained note"
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{OperationID: "search-migration-edit", Name: &name, Project: &project, Note: &note}); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := s.db.QueryRow(`SELECT f.text FROM catalog_search f JOIN query_sessions q ON q.rowid=f.rowid WHERE q.id='s-000000'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"renamed fixture", "new-project", "retained note"} {
		if !strings.Contains(text, term) {
			t.Fatal("missing search text", term, text)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM catalog_search WHERE catalog_search MATCH '"new-project"'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM catalog_search`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
}
