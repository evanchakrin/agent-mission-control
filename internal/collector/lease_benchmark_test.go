package collector

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Measures the real lease transaction with 100001 queued sources. No transcript
// bodies or network requests are involved; setup is excluded from timing.
func BenchmarkLeaseChunk100001Sources(b *testing.B) {
	root := b.TempDir()
	c, err := Open(Config{DataDir: filepath.Join(root, "spool"), Roots: []Root{{Provider: "claude", Path: root}}, HubURL: "http://127.0.0.1:1", MachineID: "fixture", Token: "fixture"})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	tx, err := c.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`WITH RECURSIVE ids(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM ids WHERE n<100000)
   INSERT INTO sources(id,identity,path,provider,native_id,current_generation,last_seen)
   SELECT printf('source-%06d',n),printf('identity-%06d',n),'fixture.jsonl','claude',printf('native-%06d',n),'g',0 FROM ids`,
		`INSERT INTO generations(source_id,generation,path,size,modified_ns,captured) SELECT id,'g','fixture.jsonl',1,0,1 FROM sources`,
		`INSERT INTO chunks(source_id,generation,offset,length,sha256,filename) SELECT id,'g',0,1,'0000000000000000000000000000000000000000000000000000000000000000','fixture.chunk' FROM sources`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixNano()
	rows, err := c.db.Query("EXPLAIN QUERY PLAN "+leaseSelectQuery, now, now)
	if err != nil {
		b.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			b.Fatal(err)
		}
		b.Log(detail)
	}
	if err = rows.Err(); err != nil {
		b.Fatal(err)
	}
	rows.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunk, err := c.leaseChunk(context.Background())
		if err != nil || chunk == nil {
			b.Fatalf("lease %d: %v", i, err)
		}
	}
}
