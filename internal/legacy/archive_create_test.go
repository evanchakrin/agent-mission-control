package legacy

import (
	"context"
	"io"
	"net/url"
	"testing"
)

func TestArchiveCreateOnlyPreservesAcceptedSource(t *testing.T) {
	for _, initial := range []string{"", "retained source\n"} {
		t.Run(initial, func(t *testing.T) {
			h, s := fixture(t)
			path := "claude/project/recovered.jsonl"
			head := map[string]string{"x-archive-machine": "erp", "x-archive-path": url.PathEscape(path), "If-None-Match": "*"}
			w := request(h, "POST", "/v1/archive/raw", []byte(initial), head)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			before, err := s.ListSources(context.Background(), "", 10)
			if err != nil || len(before) != 1 {
				t.Fatal(before, err)
			}
			for range 2 {
				w = request(h, "POST", "/v1/archive/raw", []byte("must not replace\n"), head)
				if w.Code != 412 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			delete(head, "If-None-Match")
			w = request(h, "POST", "/v1/archive/create", []byte("also must not replace\n"), head)
			if w.Code != 412 {
				t.Fatal("dedicated recovery route lost create-only semantics", w.Code, w.Body.String())
			}
			after, err := s.ListSources(context.Background(), "", 10)
			if err != nil || len(after) != 1 || after[0].Source.Generation != before[0].Source.Generation || after[0].DurableOffset != int64(len(initial)) {
				t.Fatal(after, err)
			}
			r, err := s.OpenSource(context.Background(), after[0].Source.SourceID, after[0].Source.Generation, 0)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(b) != initial {
				t.Fatal("accepted source changed", err)
			}
		})
	}
}
