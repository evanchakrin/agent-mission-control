package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplacementIdentity(t *testing.T) {
	for _, tc := range []struct{ same, ambiguous bool }{{true, false}, {false, false}, {true, true}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			same := tc.same && !tc.ambiguous
			c, root := testCollector(t, nil)
			original := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"chat-a\"}}\n")
			path := writeSource(t, root, "chat.jsonl", original)
			reconcile(t, c)
			capture(t, c, 1)
			var id, gen string
			if err := c.db.QueryRow("SELECT id,current_generation FROM sources").Scan(&id, &gen); err != nil {
				t.Fatal(err)
			}
			native := "chat-a"
			if !tc.same {
				native = "chat-b"
			}
			if tc.ambiguous {
				if _, err := c.db.Exec(`INSERT INTO sources(id,identity,path,provider,native_id,current_generation,last_seen) SELECT 'ambiguous','other-identity',path,provider,native_id,current_generation,last_seen FROM sources WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			replacement := writeSource(t, root, "replacement.tmp", []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n{}\n", native)))
			// Keep the old inode alive outside discovery so the replacement identity is distinct.
			if err := os.Rename(path, filepath.Join(root, "old.tmp")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			reconcile(t, c)
			var count int
			var after string
			if err := c.db.QueryRow("SELECT count(*) FROM sources").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 2
			if same {
				want = 1
			}
			if tc.ambiguous {
				want = 3
			}
			if count != want {
				t.Fatalf("sources=%d want=%d", count, want)
			}
			if err := c.db.QueryRow("SELECT current_generation FROM sources WHERE id=?", id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if same && after == gen {
				t.Fatal("replacement did not create generation")
			}
			if !same && after != gen {
				t.Fatal("different chat changed old generation")
			}
			var chunks int
			if err := c.db.QueryRow("SELECT count(*) FROM chunks WHERE source_id=? AND generation=?", id, gen).Scan(&chunks); err != nil {
				t.Fatal(err)
			}
			if chunks != 1 {
				t.Fatal("prior captured evidence lost")
			}
			reconcile(t, c)
			if err := c.db.QueryRow("SELECT count(*) FROM sources").Scan(&count); err != nil || count != want {
				t.Fatal("repeat discovery changed identity", count, err)
			}
		})
	}
}
