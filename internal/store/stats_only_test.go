package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatsOnlyRetainsCountsAndReceiptsWithoutTranscript(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "hub")
	seed, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.db.Exec(`INSERT INTO properties(key,value) VALUES('storage_mode','stats-only')`); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.StatsOnly() {
		t.Fatal("statistics mode not loaded")
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	src := testSource()
	receipt := ingest(t, s, src, 0, "private transcript\n")
	b := batch(src, 0, receipt.DurableOffset)
	b.ParserState = json.RawMessage(`{"version":"7","title":"private transcript","project":"test"}`)
	b.Events = []Event{{ID: "event-1", AgentID: "agent-1", Kind: "tool-call", Timestamp: time.Now(), SourceOffset: 0, SourceLength: receipt.DurableOffset, Text: "private transcript", SearchText: "private transcript", Data: json.RawMessage(`{"tool":"Read","input":"private transcript"}`)}}
	b.Usage = []UsageObservation{{ID: "usage-1", AgentID: "agent-1", Model: "model-1", Kind: "message-final", TokensIn: 4, Evidence: json.RawMessage(`{"body":"private transcript"}`)}}
	if err = s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	session, err := s.GetSession(ctx, b.Session.ID)
	if err != nil || session.Title != "" || session.TokensIn != 4 || session.EventCount != 1 {
		t.Fatalf("session %+v: %v", session, err)
	}
	for _, query := range []string{
		`SELECT text||CAST(data AS TEXT) FROM events LIMIT 1`,
		`SELECT CAST(observation AS TEXT) FROM usage_observations LIMIT 1`,
		`SELECT CAST(parser_state AS TEXT) FROM sources LIMIT 1`,
		`SELECT CAST(projection AS TEXT) FROM sessions LIMIT 1`,
	} {
		var data string
		if err = s.db.QueryRowContext(ctx, query).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(data, "private transcript") {
			t.Fatalf("transcript retained by %s", query)
		}
	}
	var searchCount int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_fts`).Scan(&searchCount); err != nil || searchCount != 0 {
		t.Fatalf("search index retained text: %d %v", searchCount, err)
	}
	chunk := makeChunk(src, 0, []byte("private transcript\n"))
	if removed, err := s.ReclaimIndexedBlobs(ctx, 10); err != nil || removed != 1 {
		t.Fatalf("reclaim %d: %v", removed, err)
	}
	if _, err = os.Stat(s.blobPath(chunk.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("raw blob still stored: %v", err)
	}
	retry := ingest(t, s, src, 0, "private transcript\n")
	if retry.ReceiptID != receipt.ReceiptID || retry.IndexedOffset != receipt.DurableOffset {
		t.Fatalf("retry %+v", retry)
	}
	if _, err = os.Stat(s.blobPath(chunk.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("retry recreated raw blob: %v", err)
	}
	var indexed int64
	if err = s.db.QueryRowContext(ctx, `SELECT indexed_offset FROM sources WHERE source_id=?`, src.SourceID).Scan(&indexed); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if indexed != receipt.DurableOffset {
		t.Fatal("indexed cursor lost")
	}
}
