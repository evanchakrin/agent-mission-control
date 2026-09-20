package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestForkRelationshipReadsExistingCheckpointWithoutReindex(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	src.Provider = "codex"
	ingest(t, s, src, 0, "fixture\n")
	b := batch(src, 0, 8)
	b.ParserState = json.RawMessage(`{"version":"7","nativeId":"native-1","forkedFromId":"origin"}`)
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := s.db.QueryRow(`SELECT projection FROM sessions WHERE id='session-1'`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	head, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.GetSession(ctx, "session-1")
	if err != nil || session.ForkedFromID != "origin" {
		t.Fatal("checkpoint relationship missing", session, err)
	}
	page, err := s.QuerySessions(ctx, SessionQuery{Limit: 10})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ForkedFromID != "origin" {
		t.Fatal("catalog relationship missing", page, err)
	}
	var after string
	if err := s.db.QueryRow(`SELECT projection FROM sessions WHERE id='session-1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	afterHead, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if original != after || head != afterHead {
		t.Fatal("relationship read changed durable projection")
	}
	for _, raw := range []string{`{"nativeId":"wrong","forkedFromId":"origin"}`, `{"nativeId":"native-1","forkedFromId":123}`, `{broken`, `{"nativeId":"native-1"}`} {
		if _, err := s.db.Exec(`UPDATE sources SET parser_state=? WHERE source_id=? AND generation=?`, raw, src.SourceID, src.Generation); err != nil {
			t.Fatal(err)
		}
		item, err := s.GetSession(ctx, "session-1")
		if err != nil || item.ForkedFromID != "" {
			t.Fatal("unverified checkpoint relationship used", raw, item, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE sources SET parser_state='{"nativeId":"native-1","forkedFromId":"origin"}' WHERE source_id=? AND generation=?`, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO active_projection VALUES(?,?,'different-revision')`, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetSession(ctx, "session-1")
	if err != nil || item.ForkedFromID != "" {
		t.Fatal("unpublished checkpoint borrowed", item, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.forkedFromId','published-origin') WHERE id='session-1'`); err != nil {
		t.Fatal(err)
	}
	item, err = s.GetSession(ctx, "session-1")
	if err != nil || item.ForkedFromID != "published-origin" {
		t.Fatal("published relationship overwritten", item, err)
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT `+sessionProjectionJSON+` FROM sessions s WHERE s.id=?`, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH src USING INDEX") || !strings.Contains(plan, "SEARCH ap USING INDEX") || strings.Contains(plan, "SCAN src") {
		t.Fatal("checkpoint lookup is not indexed", plan)
	}
}
