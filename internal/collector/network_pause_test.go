package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type failingNetwork struct{}

func (failingNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.DNSError{Err: "fixture unavailable", Name: "fixture.invalid", IsTemporary: true}
}

func TestNetworkOutageBackoffSharedAcrossSourcesAndRestart(t *testing.T) {
	for _, code := range []int{0, 408, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var recovered atomic.Bool
			var heartbeats atomic.Int32
			c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/ingest/heartbeat" {
					heartbeats.Add(1)
					w.WriteHeader(204)
					return
				}
				if !recovered.Load() {
					w.WriteHeader(code)
					return
				}
				ch := decodeChunk(t, r)
				_ = json.NewEncoder(w).Encode(protocol.Receipt{SourceID: ch.Source.SourceID, Generation: ch.Source.Generation, DurableOffset: ch.Offset + ch.Length, ReceiptID: "probe"})
			})
			for i := 0; i < 8; i++ {
				writeSource(t, root, fmt.Sprintf("file-%d.jsonl", i), []byte("{}\n"))
			}
			reconcile(t, c)
			capture(t, c, 8)
			transport := c.client.Transport
			if code == 0 {
				c.client.Transport = failingNetwork{}
			}
			ctx := context.Background()
			for attempt := 1; attempt <= 3; attempt++ {
				if worked, err := c.UploadOnce(ctx); !worked || err == nil {
					t.Fatalf("expected failed probe: %v %v", worked, err)
				}
				pause, err := readUploadPause(ctx, c.db)
				if err != nil || pause.State != "retrying" || pause.Failures != attempt || pause.Until <= time.Now().UnixNano() {
					t.Fatalf("pause %+v: %v", pause, err)
				}
				for i := 0; i < 8; i++ {
					if worked, err := c.UploadOnce(ctx); worked || err != nil {
						t.Fatalf("other source bypassed pause: %v %v", worked, err)
					}
				}
				if attempt < 3 {
					if _, err := c.db.Exec(`UPDATE settings SET value=json_set(value,'$.until',0) WHERE key='upload_pause'`); err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg := c.cfg
			c.Close()
			reopened, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if worked, err := reopened.UploadOnce(ctx); worked || err != nil {
				t.Fatalf("restart bypassed pause: %v %v", worked, err)
			}
			if err := reopened.sendHeartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			if heartbeats.Load() != 1 {
				t.Fatal("pause delayed heartbeat")
			}
			if _, err := reopened.db.Exec(`UPDATE settings SET value=json_set(value,'$.until',0) WHERE key='upload_pause'`); err != nil {
				t.Fatal(err)
			}
			recovered.Store(true)
			reopened.client.Transport = transport
			if worked, err := reopened.UploadOnce(ctx); !worked || err != nil {
				t.Fatalf("successful probe: %v %v", worked, err)
			}
			pause, err := readUploadPause(ctx, reopened.db)
			if err != nil || pause.Failures != 0 {
				t.Fatalf("successful probe retained streak: %+v %v", pause, err)
			}
		})
	}
}
