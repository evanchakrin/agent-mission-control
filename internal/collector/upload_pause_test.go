package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestUploadPauseAcrossSourcesAndRestart(t *testing.T) {
	for _, code := range []int{401, 403, 507, 413} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var requests, heartbeats atomic.Int32
			var recovered atomic.Bool
			c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/ingest/heartbeat" {
					var hb protocol.Heartbeat
					if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
						t.Error(err)
					}
					if hb.State != "blocked_auth" && hb.State != "blocked_storage" {
						t.Errorf("heartbeat hid pause: %+v", hb)
					}
					if hb.CollectionState != "ready" || hb.ConnectionState != hb.State || hb.UploadRetryAt == nil || hb.ConnectionError == "" {
						t.Errorf("missing independent heartbeat diagnostics: %+v", hb)
					}
					heartbeats.Add(1)
					w.WriteHeader(204)
					return
				}
				requests.Add(1)
				if recovered.Load() {
					ch := decodeChunk(t, r)
					_ = json.NewEncoder(w).Encode(protocol.Receipt{SourceID: ch.Source.SourceID, Generation: ch.Source.Generation, DurableOffset: ch.Offset + ch.Length, ReceiptID: "recovered-receipt"})
					return
				}
				w.WriteHeader(code)
			})
			for i := range 8 {
				writeSource(t, root, fmt.Sprintf("source-%d.jsonl", i), []byte("{}\n"))
			}
			reconcile(t, c)
			capture(t, c, 8)
			ctx := context.Background()
			if worked, err := c.UploadOnce(ctx); !worked || err == nil {
				t.Fatalf("first failure: %v %v", worked, err)
			}
			cfg := c.cfg
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for range 16 {
				if worked, err := reopened.UploadOnce(ctx); worked || err != nil {
					t.Fatalf("pause bypassed: %v %v", worked, err)
				}
			}
			reopened.setCollection("ready", nil)
			if err := reopened.sendHeartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			s := stats(t, reopened)
			if requests.Load() != 1 || heartbeats.Load() != 1 || s.UploadRetryAt == nil || s.BacklogBytes != 24 || s.UploadedBytes != 0 {
				t.Fatalf("pause invariants: requests=%d heartbeat=%d status=%+v", requests.Load(), heartbeats.Load(), s)
			}
			// Expire only the shared pause; another source is immediately eligible
			// for the slow probe, without resetting the original chunk's retry.
			if _, err := reopened.db.Exec(`UPDATE settings SET value='{"until":1,"state":"blocked_auth"}' WHERE key='upload_pause'`); err != nil {
				t.Fatal(err)
			}
			if worked, err := reopened.UploadOnce(ctx); !worked || err == nil || requests.Load() != 2 {
				t.Fatalf("probe missing: %v %v %d", worked, err, requests.Load())
			}
			recovered.Store(true)
			if _, err := reopened.db.Exec(`UPDATE settings SET value='{"until":1,"state":"blocked_auth"}' WHERE key='upload_pause'`); err != nil {
				t.Fatal(err)
			}
			if worked, err := reopened.UploadOnce(ctx); !worked || err != nil {
				t.Fatalf("recovery failed: %v %v", worked, err)
			}
			s = stats(t, reopened)
			if s.UploadRetryAt != nil || s.ConnectionState != "connected" || s.ConnectionError != "" || s.UploadedBytes != 3 || s.BacklogBytes != 21 {
				t.Fatalf("invalid recovered state: %+v", s)
			}
		})
	}
}

func TestCollectionAndConnectionErrorsIndependent(t *testing.T) {
	c, _ := testCollector(t, nil)
	c.setConnection("retrying", errors.New("network unavailable"))
	c.setCollection("blocked_source", errors.New("source unavailable"))
	s := stats(t, c)
	if !strings.Contains(s.Error, "network unavailable") || !strings.Contains(s.Error, "source unavailable") {
		t.Fatalf("lost diagnostic: %+v", s)
	}
	c.setCollection("ready", nil)
	s = stats(t, c)
	if s.Error != "network unavailable" || s.CollectionError != "" {
		t.Fatalf("collection success hid connection failure: %+v", s)
	}
	c.setConnection("connected", nil)
	if s = stats(t, c); s.Error != "" {
		t.Fatalf("stale diagnostic: %+v", s)
	}
}

func TestInFlightSuccessCannotEraseConcurrentUploadPause(t *testing.T) {
	entered := make(chan int32, 2)
	releaseFailure := make(chan struct{})
	releaseSuccess := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var requests atomic.Int32
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		entered <- n
		gate := releaseFailure
		if n == 2 {
			gate = releaseSuccess
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
		if n == 1 {
			w.WriteHeader(401)
			return
		}
		ch := decodeChunk(t, r)
		_ = json.NewEncoder(w).Encode(protocol.Receipt{SourceID: ch.Source.SourceID, Generation: ch.Source.Generation, DurableOffset: ch.Offset + ch.Length, ReceiptID: "in-flight-success"})
	})
	for i := range 4 {
		writeSource(t, root, fmt.Sprintf("concurrent-%d.jsonl", i), []byte("{}\n"))
	}
	reconcile(t, c)
	capture(t, c, 4)
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := c.UploadOnce(ctx); results <- err }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("workers did not both enter transport")
		}
	}
	close(releaseFailure)
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("expected auth failure")
		}
	case <-ctx.Done():
		t.Fatal("failure did not commit")
	}
	close(releaseSuccess)
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("success did not commit")
	}
	s := stats(t, c)
	if s.ConnectionState != "blocked_auth" || s.UploadRetryAt == nil || s.UploadedBytes != 3 || s.BacklogBytes != 9 {
		t.Fatalf("in-flight success erased pause or history: %+v", s)
	}
	if worked, err := c.UploadOnce(ctx); worked || err != nil || requests.Load() != 2 {
		t.Fatalf("new upload bypassed pause: %v %v requests=%d", worked, err, requests.Load())
	}
}
