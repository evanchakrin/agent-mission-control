package store

import (
	"context"
	"testing"
)

func TestMetadataInsertUpgradePreservesExistingCatalog(t *testing.T) {
	for _, version := range []string{"8", "9"} {
		t.Run(version, func(t *testing.T) {
			s := openTestStore(t, Options{})
			seedAnalyticsCatalog(t, s, 1)
			ctx := context.Background()
			if _, err := s.db.Exec(`UPDATE properties SET value=? WHERE key='analytics_schema'`, version); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER query_metadata_insert;
			 CREATE TRIGGER query_metadata_insert AFTER INSERT ON session_metadata BEGIN SELECT RAISE(ABORT,'old insert trigger'); END;
			 CREATE TRIGGER guard_catalog_update BEFORE UPDATE ON query_sessions BEGIN SELECT RAISE(ABORT,'migration rewrote catalog'); END;
			 CREATE TRIGGER guard_catalog_delete BEFORE DELETE ON query_sessions BEGIN SELECT RAISE(ABORT,'migration deleted catalog'); END;`); err != nil {
				t.Fatal(err)
			}
			if err := s.SetupAnalytics(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER guard_catalog_update;
			 CREATE TRIGGER forbid_first_edit_accounting BEFORE UPDATE OF tokens_in,tokens_cache,tokens_write,tokens_out,cost_estimate ON query_sessions BEGIN SELECT RAISE(ABORT,'accounting rewrite'); END;`); err != nil {
				t.Fatal(err)
			}
			archived := true
			patch := MetadataPatch{OperationID: "first-archive", Archived: &archived}
			for i := 0; i < 2; i++ {
				m, err := s.PatchMetadata(ctx, "s-000000", patch)
				if err != nil || !m.Archived || m.Revision != 1 {
					t.Fatal("first archive or idempotent replay failed", m, err)
				}
			}
			var archivedCount, tokens int
			if err := s.db.QueryRow(`SELECT archived,tokens_in+tokens_cache+tokens_out FROM query_sessions WHERE id='s-000000'`).Scan(&archivedCount, &tokens); err != nil || archivedCount != 1 || tokens != 4 {
				t.Fatal("archive projection or accounting changed", archivedCount, tokens, err)
			}
			var actual string
			if err := s.db.QueryRow(`SELECT value FROM properties WHERE key='analytics_schema'`).Scan(&actual); err != nil || actual != "10" {
				t.Fatal("upgrade not recorded", actual, err)
			}
		})
	}
}

func TestMetadataCatalogMigrationPreservesAccounting(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE properties SET value='8' WHERE key='analytics_schema'; DROP TRIGGER query_metadata_update;
	 CREATE TRIGGER query_metadata_update AFTER UPDATE ON session_metadata BEGIN SELECT 1; END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	// A guard detects even a same-value accounting rewrite.
	if _, err := s.db.Exec(`CREATE TRIGGER forbid_metadata_accounting BEFORE UPDATE OF tokens_in,tokens_cache,tokens_out,cost_estimate ON query_sessions BEGIN SELECT RAISE(ABORT,'accounting rewrite'); END;`); err != nil {
		t.Fatal(err)
	}
	// The first owner edit must also leave accounting columns untouched.
	if _, err := s.db.Exec(`INSERT INTO session_metadata VALUES('s-000000',1,'{"archived":true}')`); err != nil {
		t.Fatal(err)
	}
	for i, value := range []string{`{"project":"owner","projectOverride":true,"name":"Renamed","archived":true,"pinned":true}`, `{"project":"","projectOverride":true,"name":"","archived":false,"pinned":false}`} {
		if _, err := s.db.Exec(`UPDATE session_metadata SET value=?,revision=revision+1 WHERE session_id='s-000000'`, value); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			var matches int
			if err := s.db.QueryRow(`SELECT count(*) FROM query_sessions WHERE id='s-000000' AND project='owner' AND title='Renamed' AND archived=1 AND pinned=1`).Scan(&matches); err != nil || matches != 1 {
				t.Fatal("organization fields not projected", matches, err)
			}
		}
	}
	var title, project string
	var archived, pinned, tokens int
	if err := s.db.QueryRow(`SELECT title,project,archived,pinned,tokens_in+tokens_cache+tokens_out FROM query_sessions WHERE id='s-000000'`).Scan(&title, &project, &archived, &pinned, &tokens); err != nil || title != "Transcript 000000" || project != "" || archived != 0 || pinned != 0 || tokens != 4 {
		t.Fatal(title, project, archived, pinned, tokens, err)
	}
}
