package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestIngestNewSourceAvoidsRedundantUpdate(t *testing.T) {
	for _, firstBody := range []string{"first", ""} {
		t.Run("initial="+firstBody, func(t *testing.T) {
			failCommit := false
			injected := errors.New("injected commit failure")
			s := openTestStore(t, Options{ExternalCheckpointOwner: true, BeforeCommit: func() error {
				if failCommit {
					return injected
				}
				return nil
			}})
			// Count actual source UPDATE statements without depending on wall time.
			if _, err := s.db.Exec(`CREATE TABLE source_update_probe(n INTEGER NOT NULL);
INSERT INTO source_update_probe VALUES(0);
CREATE TRIGGER source_update_probe AFTER UPDATE ON sources BEGIN
 UPDATE source_update_probe SET n=n+1;
END;`); err != nil {
				t.Fatal(err)
			}
			check := func(want int) {
				t.Helper()
				var got int
				if err := s.db.QueryRow(`SELECT n FROM source_update_probe`).Scan(&got); err != nil || got != want {
					t.Fatalf("source updates=%d want=%d err=%v", got, want, err)
				}
				assertLedgerTotals(t, s)
			}
			src := testSource()
			first := ingest(t, s, src, 0, firstBody)
			check(0)
			next := ingest(t, s, src, int64(len(firstBody)), "next")
			check(1)
			retry := ingest(t, s, src, int64(len(firstBody)), "next")
			if retry.ReceiptID != next.ReceiptID || retry.DurableOffset != next.DurableOffset {
				t.Fatal("retry changed receipt")
			}
			check(1)
			if first.DurableOffset != int64(len(firstBody)) {
				t.Fatal("initial receipt", first)
			}
			failCommit = true
			rewritten := src
			rewritten.Generation = "new-generation"
			chunk := makeChunk(rewritten, 0, []byte("replacement"))
			if _, err := s.IngestChunk(context.Background(), chunk, bytes.NewBufferString("replacement")); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			var generations int
			if err := s.db.QueryRow(`SELECT count(*) FROM sources`).Scan(&generations); err != nil || generations != 1 {
				t.Fatal(generations, err)
			}
			check(1)
			failCommit = false
			ingest(t, s, rewritten, 0, "replacement")
			check(1)
		})
	}
}
