package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestReconciliationRejectsMalformedReceiptWithoutAcknowledging(t *testing.T) {
	for _, suffix := range []string{` {"second":"object"}`, strings.Repeat(" ", 64<<10)} {
		t.Run(map[bool]string{true: "oversize", false: "trailing-json"}[len(suffix) > 100], func(t *testing.T) {
			c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
				var src protocol.Source
				if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
					t.Error(err)
					return
				}
				raw, _ := json.Marshal(protocol.Receipt{SourceID: src.SourceID, Generation: src.Generation, DurableOffset: src.Size})
				w.Write(raw)
				w.Write([]byte(suffix))
			})
			writeSource(t, root, "history.jsonl", []byte("history\n"))
			reconcile(t, c)
			capture(t, c, 1)
			before := stats(t, c)
			if err := c.ReconcileRemote(context.Background()); err == nil {
				t.Fatal("malformed receipt acknowledged queued history")
			}
			after := stats(t, c)
			if after.BacklogBytes != before.BacklogBytes || after.UploadedBytes != before.UploadedBytes || after.SpoolBytes != before.SpoolBytes {
				t.Fatal("malformed receipt changed durable state", before, after)
			}
		})
	}
}

func TestUploadRejectsMalformedReceiptWithoutAcknowledging(t *testing.T) {
	for _, suffix := range []string{` {"second":"object"}`, strings.Repeat(" ", 64<<10)} {
		t.Run(map[bool]string{true: "oversize", false: "trailing-json"}[len(suffix) > 100], func(t *testing.T) {
			c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
				ack(w, decodeChunk(t, r))
				w.Write([]byte(suffix))
			})
			writeSource(t, root, "history.jsonl", []byte("history\n"))
			reconcile(t, c)
			capture(t, c, 1)
			before := stats(t, c)
			if _, err := c.UploadOnce(context.Background()); err == nil {
				t.Fatal("malformed receipt acknowledged queued history")
			}
			after := stats(t, c)
			if after.BacklogBytes != before.BacklogBytes || after.UploadedBytes != before.UploadedBytes || after.SpoolBytes != before.SpoolBytes {
				t.Fatal("malformed receipt changed durable state", before, after)
			}
		})
	}
}
