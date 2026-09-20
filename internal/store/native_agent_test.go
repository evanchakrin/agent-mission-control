package store

import (
	"context"
	"testing"
)

func TestNativeAgentSchemaSevenUpgradePreservesUnknownsAndProjectionChanges(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_set(projection,'$.nativeAgentId','known-child','$.parentThreadId','parent') WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow("SELECT projection FROM sessions WHERE id='s-000000'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"query_session_insert", "query_session_update", "query_session_delete", "query_metadata_insert", "query_metadata_update", "query_metadata_delete"} {
		if _, err := s.db.Exec("DROP TRIGGER " + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE query_sessions DROP COLUMN native_agent_id; UPDATE properties SET value='7' WHERE key='analytics_schema';`); err != nil {
		t.Fatal(err)
	}
	// Prior triggers are recreated only to represent the names present in v7.
	for _, entry := range []struct{ name, table string }{{"session", "sessions"}, {"metadata", "session_metadata"}} {
		for _, event := range []string{"insert", "update", "delete"} {
			if _, err := s.db.Exec("CREATE TRIGGER query_" + entry.name + "_" + event + " AFTER " + event + " ON " + entry.table + " BEGIN SELECT 1; END;"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	var after, native, unknown string
	if err := s.db.QueryRow("SELECT projection FROM sessions WHERE id='s-000000'").Scan(&after); err != nil || after != before {
		t.Fatal("read-model migration changed source projection", err)
	}
	if err := s.db.QueryRow("SELECT native_agent_id FROM query_sessions WHERE id='s-000000'").Scan(&native); err != nil || native != "known-child" {
		t.Fatal("known identity not backfilled", native, err)
	}
	if err := s.db.QueryRow("SELECT native_agent_id FROM query_sessions WHERE id='s-000001'").Scan(&unknown); err != nil || unknown != "" {
		t.Fatal("old history identity invented", unknown, err)
	}
	if _, err := s.db.Exec(`INSERT INTO session_metadata VALUES('s-000000',1,'{"archived":true,"revision":1}')`); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT native_agent_id FROM query_sessions WHERE id='s-000000'").Scan(&native); err != nil || native != "known-child" {
		t.Fatal("organization erased identity", native, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET projection=json_remove(projection,'$.nativeAgentId') WHERE id='s-000000'`); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT native_agent_id FROM query_sessions WHERE id='s-000000'").Scan(&native); err != nil || native != "" {
		t.Fatal("replacement left stale identity", native, err)
	}
}
