package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsOffsetsUseCoveringIndex(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*),COALESCE(SUM(durable_offset),0),COALESCE(SUM(indexed_offset),0) FROM sources`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	covered := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		covered = covered || strings.Contains(detail, "COVERING INDEX sources_offsets")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("health offsets still read full source metadata rows")
	}
	rows.Close()
	src := testSource()
	ingest(t, s, src, 0, "fixture\n")
	if err := s.CommitIndex(context.Background(), batch(src, 0, 8)); err != nil {
		t.Fatal(err)
	}
	src.Generation = "replacement"
	ingest(t, s, src, 0, "replacement\n")
	d, err := s.Diagnostics(context.Background())
	if d.IndexingBacklogScope != "all-retained-generations" {
		t.Fatal("retained bytes advertised without their generation scope", d)
	}
	if err != nil || d.Sources != 1 || d.Generations != 2 || d.DurableBytes != 20 || d.IndexedBytes != 8 || d.IndexingBacklogBytes != 12 {
		t.Fatal("offset index changed history totals", d, err)
	}
}

func TestDiagnosticsOffsets100001Sources(t *testing.T) {
	if testing.Short() {
		t.Skip("100001 padded source metadata rows")
	}
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	// Query-only fixture: offsets are synthetic, not evidence of captured bytes.
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<100000) INSERT INTO source_identity SELECT printf('s%06d',x),'fixture','codex','','g1' FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sources(source_id,generation,meta_json,durable_offset,indexed_offset,created_at,updated_at) SELECT source_id,'g1',json_object('fixturePadding',?),128,64,'','' FROM source_identity`, strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	read := func(label, suffix string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		var count, durable, indexed int64
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(durable_offset),0),COALESCE(SUM(indexed_offset),0) FROM sources`+suffix).Scan(&count, &durable, &indexed)
		if err != nil || count != 100001 || durable != 128*100001 || indexed != 64*100001 {
			t.Fatal(label, count, durable, indexed, err)
		}
		t.Logf("%s=%s all %d sources preserved", label, time.Since(started), count)
	}
	read("full-source-rows", " NOT INDEXED")
	for i := 0; i < 3; i++ {
		read("covering-offset-index", "")
	}
}
