package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestUploadEpochFailureDoesNotAcknowledgePayload(t *testing.T) {
	c, root := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		ch := decodeChunk(t, r)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(protocol.Receipt{SourceID: ch.Source.SourceID, Generation: ch.Source.Generation, DurableOffset: ch.Offset + ch.Length, RecoveryEpoch: "new-epoch", ReceiptID: "fixture"})
	})
	writeSource(t, root, "a.jsonl", []byte("history\n"))
	reconcile(t, c)
	capture(t, c, 1)
	if _, err := c.db.Exec(`CREATE TRIGGER fail_epoch BEFORE INSERT ON settings WHEN NEW.key='recovery_pending' BEGIN SELECT RAISE(ABORT,'injected recovery state failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadOnce(context.Background()); err == nil {
		t.Fatal("upload hid recovery state failure")
	}
	var pending, epoch int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE ack_at IS NULL AND payload_present=1`).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("failed recovery write acknowledged payload", pending, err)
	}
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key='recovery_epoch'`).Scan(&epoch); err != nil || epoch != 0 {
		t.Fatal("partial epoch transaction committed", epoch, err)
	}
	if _, err := c.db.Exec(`DROP TRIGGER fail_epoch; UPDATE chunks SET next_attempt=0,lease_until=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadOnce(context.Background()); err != nil {
		t.Fatal("retry after storage recovery failed", err)
	}
	var value string
	if err := c.db.QueryRow(`SELECT value FROM settings WHERE key='recovery_pending'`).Scan(&value); err != nil || value != "new-epoch" {
		t.Fatal("successful retry lost durable recovery intent", value, err)
	}
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE ack_at IS NOT NULL`).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("successful retry was not acknowledged", pending, err)
	}
}

func TestHeartbeatReportsEpochStorageFailure(t *testing.T) {
	c, _ := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-AMC-Recovery-Epoch", "new-epoch")
		w.WriteHeader(http.StatusNoContent)
	})
	if _, err := c.db.Exec(`CREATE TRIGGER fail_epoch BEFORE INSERT ON settings WHEN NEW.key='recovery_pending' BEGIN SELECT RAISE(ABORT,'injected recovery state failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.sendHeartbeat(context.Background()); err == nil {
		t.Fatal("heartbeat hid recovery state failure")
	}
	if stats(t, c).LastHeartbeatAt != nil {
		t.Fatal("failed recovery write reported a successful heartbeat")
	}
}
