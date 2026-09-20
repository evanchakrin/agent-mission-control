package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRecentRunActivityTransitions(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	now := time.Now().UTC()
	src := testSource()
	src.ModifiedAt = now.Add(-time.Minute)
	src.Size = 2
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Session.LastActivity = now.Add(-3 * time.Minute)
	b.Events = []Event{{ID: "run-call", AgentID: "main", Kind: "tool-call", Timestamp: now.Add(-3 * time.Minute), SourceLength: 2, Data: json.RawMessage(`{"toolUseId":"call"}`)}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	page, err := s.RecentRunActivity(ctx, now)
	if err != nil || len(page.Runs) != 1 {
		t.Fatal(page, err)
	}
	r := page.Runs[0]
	if !r.Active || !r.Synced || r.Stalled == nil || !*r.Stalled {
		t.Fatal(r)
	}
	page, err = s.RecentRunActivity(ctx, now.Add(8*time.Minute))
	if err != nil || len(page.Runs) != 1 || page.Runs[0].Active || page.Runs[0].Stalled == nil || *page.Runs[0].Stalled {
		t.Fatal(page, err)
	}
	page, err = s.RecentRunActivity(ctx, now.Add(time.Hour))
	if err != nil || len(page.Runs) != 0 {
		t.Fatal("old history included", page, err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO events(id,session_id,source_id,generation,agent_id,kind,timestamp,source_offset,raw_length,text,data,dedupe_key,projection_revision) SELECT 'run-result',session_id,source_id,generation,agent_id,'tool-result',timestamp,0,2,'',data,'',projection_revision FROM events WHERE id='run-call'`); err != nil {
		t.Fatal(err)
	}
	page, err = s.RecentRunActivity(ctx, now)
	if err != nil || page.Runs[0].Stalled == nil || *page.Runs[0].Stalled {
		t.Fatal("result did not clear pending call", page, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE sources SET durable_offset=3`); err != nil {
		t.Fatal(err)
	}
	page, err = s.RecentRunActivity(ctx, now)
	if err != nil || page.Runs[0].Synced || page.Runs[0].Stalled != nil {
		t.Fatal("indexing lag considered reliable", page, err)
	}
}
