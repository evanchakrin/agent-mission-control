package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

func catalogSearchPredicate(query string) (string, []any) {
	text := strings.ToLower(query)
	if utf8.RuneCountInString(text) >= 3 && !strings.ContainsRune(text, 0) {
		return `catalog_search MATCH ? AND instr(catalog_search.text,?)>0`, []any{`"` + strings.ReplaceAll(text, `"`, `""`) + `"`, text}
	}
	return `instr(catalog_search.text,?)>0`, []any{text}
}

const catalogSearchText = `amc_fold(q.title||' '||q.project||' '||q.machine_id||' '||
 COALESCE((SELECT json_extract(machine.heartbeat,'$.name') FROM machines machine WHERE machine.machine_id=q.machine_id),'')||' '||
 COALESCE((SELECT label.display_name FROM machine_labels label WHERE label.machine_id=q.machine_id),'')||' '||
 q.native_id||' '||q.id||' '||COALESCE((SELECT json_extract(metadata.value,'$.note') FROM session_metadata metadata WHERE metadata.session_id=q.id),''))`

// Existing catalog rows have stable rowids. Updating in place avoids the
// redundant row lookup and delete/insert statement pair on organization edits.
const catalogSearchUpdate = `CREATE TRIGGER catalog_search_update AFTER UPDATE ON query_sessions
 WHEN OLD.title IS NOT NEW.title OR OLD.project IS NOT NEW.project OR OLD.machine_id IS NOT NEW.machine_id OR OLD.native_id IS NOT NEW.native_id
 BEGIN UPDATE catalog_search SET text=(SELECT ` + catalogSearchText + ` FROM query_sessions q WHERE q.id=NEW.id) WHERE rowid=NEW.rowid; END;`

// Missing/empty notes add no search text. Other first-edit fields are handled
// by the catalog insert/update triggers, including metadata before a source.
const catalogSearchNoteInsert = `CREATE TRIGGER catalog_search_note_insert AFTER INSERT ON session_metadata
 WHEN COALESCE(json_extract(NEW.value,'$.note'),'') <> '' BEGIN
 DELETE FROM catalog_search WHERE rowid IN (SELECT q.rowid FROM query_sessions q WHERE q.id=NEW.session_id);
 INSERT INTO catalog_search(rowid,text) SELECT q.rowid,` + catalogSearchText + ` FROM query_sessions q WHERE q.id=NEW.session_id;
 END;`

const catalogSearchDrop = `
DROP TRIGGER IF EXISTS catalog_search_insert;
DROP TRIGGER IF EXISTS catalog_search_update;
DROP TRIGGER IF EXISTS catalog_search_delete;
DROP TRIGGER IF EXISTS catalog_search_note_insert;
DROP TRIGGER IF EXISTS catalog_search_note_update;
DROP TRIGGER IF EXISTS catalog_search_note_delete;
DROP TRIGGER IF EXISTS catalog_search_machine_insert;
DROP TRIGGER IF EXISTS catalog_search_machine_update;
DROP TRIGGER IF EXISTS catalog_search_machine_delete;
DROP TRIGGER IF EXISTS catalog_search_label_insert;
DROP TRIGGER IF EXISTS catalog_search_label_update;
DROP TRIGGER IF EXISTS catalog_search_label_delete;
DROP TABLE IF EXISTS catalog_search;
DELETE FROM properties WHERE key='catalog_search_schema';
`

// A disposable projection of organization and catalog text, never transcript
// bodies. Triggers update it in the same transaction as the authoritative edit.
// Ordinary token/event-count updates and unchanged heartbeats do not rewrite it.
func (s *Store) ensureCatalogSearch(ctx context.Context) error {
	var version string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='catalog_search_schema'").Scan(&version)
	if err == nil && version == "4" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && version != "1" && version != "2" && version != "3" {
		return ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT value FROM properties WHERE key='catalog_search_schema'").Scan(&version)
	if err == nil && version == "4" {
		return nil
	}
	if err == nil && version == "3" {
		// Only the trigger changed: preserve the complete existing FTS index.
		if _, err = tx.ExecContext(ctx, `DROP TRIGGER catalog_search_note_insert;`+catalogSearchNoteInsert+`UPDATE properties SET value='4' WHERE key='catalog_search_schema';`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err == nil && (version == "1" || version == "2") {
		// Rebuild only the disposable text index, within this transaction. Existing
		// session projections, raw evidence and organization rows remain intact.
		if _, err = tx.ExecContext(ctx, catalogSearchDrop); err != nil {
			return err
		}
		err = sql.ErrNoRows
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return err
		}
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, `CREATE VIRTUAL TABLE catalog_search USING fts5(text,tokenize='trigram case_sensitive 1');
 INSERT INTO catalog_search(rowid,text) SELECT q.rowid,`+catalogSearchText+` FROM query_sessions q;`); err != nil {
		return err
	}
	refresh := func(where string) string {
		return `DELETE FROM catalog_search WHERE rowid IN (SELECT q.rowid FROM query_sessions q WHERE ` + where + `); INSERT INTO catalog_search(rowid,text) SELECT q.rowid,` + catalogSearchText + ` FROM query_sessions q WHERE ` + where + `;`
	}
	statements := []string{
		`CREATE TRIGGER catalog_search_insert AFTER INSERT ON query_sessions BEGIN ` + refresh("q.id=NEW.id") + ` END;`,
		catalogSearchUpdate,
		`CREATE TRIGGER catalog_search_delete AFTER DELETE ON query_sessions BEGIN DELETE FROM catalog_search WHERE rowid=OLD.rowid; END;`,
		catalogSearchNoteInsert,
		`CREATE TRIGGER catalog_search_note_update AFTER UPDATE ON session_metadata WHEN json_extract(OLD.value,'$.note') IS NOT json_extract(NEW.value,'$.note') BEGIN ` + refresh("q.id=NEW.session_id") + ` END;`,
		`CREATE TRIGGER catalog_search_note_delete AFTER DELETE ON session_metadata BEGIN ` + refresh("q.id=OLD.session_id") + ` END;`,
		`CREATE TRIGGER catalog_search_machine_insert AFTER INSERT ON machines BEGIN ` + refresh("q.machine_id=NEW.machine_id") + ` END;`,
		`CREATE TRIGGER catalog_search_machine_update AFTER UPDATE ON machines WHEN json_extract(OLD.heartbeat,'$.name') IS NOT json_extract(NEW.heartbeat,'$.name') BEGIN ` + refresh("q.machine_id=NEW.machine_id") + ` END;`,
		`CREATE TRIGGER catalog_search_machine_delete AFTER DELETE ON machines BEGIN ` + refresh("q.machine_id=OLD.machine_id") + ` END;`,
		`CREATE TRIGGER catalog_search_label_insert AFTER INSERT ON machine_labels BEGIN ` + refresh("q.machine_id=NEW.machine_id") + ` END;`,
		`CREATE TRIGGER catalog_search_label_update AFTER UPDATE ON machine_labels WHEN OLD.display_name IS NOT NEW.display_name BEGIN ` + refresh("q.machine_id=NEW.machine_id") + ` END;`,
		`CREATE TRIGGER catalog_search_label_delete AFTER DELETE ON machine_labels BEGIN ` + refresh("q.machine_id=OLD.machine_id") + ` END;`,
		`INSERT INTO properties(key,value) VALUES('catalog_search_schema','4');`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
