package legacy

import (
	"context"
	"net/url"
	"testing"
)

func TestParsedSummaryCannotReplaceRawHistory(t *testing.T) {
	for _, transport := range []string{"append", "archive"} {
		t.Run(transport, func(t *testing.T) {
			h, s := fixture(t)
			path := "claude/project/main.jsonl"
			raw := []byte("{\"type\":\"user\"}\n")
			if transport == "append" {
				w := request(h, "POST", "/v1/relay/append", raw, appendHeaders(path, 0, int64(len(raw))))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			} else {
				w := request(h, "POST", "/v1/archive/raw", raw, map[string]string{"x-archive-machine": "erp", "x-archive-path": url.PathEscape(path)})
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			before, err := s.ListSources(context.Background(), "", 10)
			if err != nil || len(before) != 1 {
				t.Fatal(before, err)
			}
			body := []byte(`{"machine":"erp","file":"project/main.jsonl","meta":{"session":"main"},"result":{"agents":[],"events":[]}}`)
			w := request(h, "POST", "/v1/relay", body, nil)
			if w.Code != 409 {
				t.Fatal("summary replaced raw evidence", w.Code, w.Body.String())
			}
			after, err := s.ListSources(context.Background(), "", 10)
			if err != nil || len(after) != 1 || before[0].Source.Generation != after[0].Source.Generation || after[0].DurableOffset != int64(len(raw)) {
				t.Fatal(after, err)
			}
		})
	}
}
