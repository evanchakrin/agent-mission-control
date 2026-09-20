package store

import (
	"context"
	"testing"
)

func TestFirstArchiveDoesNotWriteSearchText(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	// A guarded stand-in observes attempted SQL writes, even same-value ones.
	// FTS search behavior is covered separately against the real virtual table.
	if _, err := s.db.Exec(`DROP TABLE catalog_search;
	 CREATE TABLE catalog_search(text TEXT);
	 INSERT INTO catalog_search(rowid,text) SELECT rowid,'unchanged fixture' FROM query_sessions;
	 CREATE TRIGGER forbid_search_insert BEFORE INSERT ON catalog_search BEGIN SELECT RAISE(ABORT,'unnecessary search insert'); END;
	 CREATE TRIGGER forbid_search_update BEFORE UPDATE ON catalog_search BEGIN SELECT RAISE(ABORT,'unnecessary search update'); END;
	 CREATE TRIGGER forbid_search_delete BEFORE DELETE ON catalog_search BEGIN SELECT RAISE(ABORT,'unnecessary search delete'); END;`); err != nil {
		t.Fatal(err)
	}
	archived := true
	patch := MetadataPatch{OperationID: "first-archive", Archived: &archived}
	for i := 0; i < 2; i++ {
		m, err := s.PatchMetadata(context.Background(), "s-000000", patch)
		if err != nil || !m.Archived || m.Revision != 1 {
			t.Fatal("archive or retry rewrote search", m, err)
		}
	}
}

func TestFirstNoteSearchUpgradePreservesIndex(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	// A sentinel exists only in FTS: rebuilding from the catalog would lose it.
	if _, err := s.db.Exec(`INSERT INTO catalog_search(rowid,text) VALUES(999,'upgrade sentinel');
	 UPDATE properties SET value='3' WHERE key='catalog_search_schema';
	 DROP TRIGGER catalog_search_note_insert;
	 CREATE TRIGGER catalog_search_note_insert AFTER INSERT ON session_metadata BEGIN SELECT RAISE(ABORT,'old note trigger'); END;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ensureCatalogSearch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var sentinel string
	if err := s.db.QueryRow(`SELECT text FROM catalog_search WHERE rowid=999`).Scan(&sentinel); err != nil || sentinel != "upgrade sentinel" {
		t.Fatal("upgrade rebuilt existing search index", sentinel, err)
	}
	var version string
	if err := s.db.QueryRow(`SELECT value FROM properties WHERE key='catalog_search_schema'`).Scan(&version); err != nil || version != "4" {
		t.Fatal(version, err)
	}
	archived, note, name := true, "first searchable note", "first renamed session"
	for _, tc := range []struct {
		id, text string
		patch    MetadataPatch
	}{
		{"s-000000", "Transcript 000000", MetadataPatch{OperationID: "archive", Archived: &archived}},
		{"s-000001", note, MetadataPatch{OperationID: "note", Note: &note}},
		{"s-000002", name, MetadataPatch{OperationID: "name", Name: &name}},
	} {
		if _, err := s.PatchMetadata(ctx, tc.id, tc.patch); err != nil {
			t.Fatal(err)
		}
		page, err := s.ListSessions(ctx, SessionQuery{Text: tc.text})
		if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != tc.id {
			t.Fatal("first edit lost search text", tc.id, page, err)
		}
	}
}
