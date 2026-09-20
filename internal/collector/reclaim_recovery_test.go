package collector

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimPreservesChunkWhoseAcknowledgementWasRevoked(t *testing.T) {
	c, root := testCollector(t, nil)
	data := []byte("captured evidence\n")
	writeSource(t, root, "a.jsonl", data)
	reconcile(t, c)
	capture(t, c, 1)
	ctx := context.Background()
	var name string
	if err := c.db.QueryRowContext(ctx, `SELECT filename FROM chunks`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE chunks SET ack_at=1;
 CREATE TRIGGER revoke_ack_before_reclaim BEFORE UPDATE OF payload_present ON chunks
 WHEN NEW.payload_present=0 BEGIN
 UPDATE chunks SET ack_at=NULL WHERE source_id=OLD.source_id AND generation=OLD.generation AND offset=OLD.offset;
 SELECT RAISE(IGNORE);
 END;`); err != nil {
		t.Fatal(err)
	}
	// Force the same zero-row update produced when reconciliation revokes an
	// acknowledgement after candidate selection. No timing-sensitive goroutines.
	if err := c.reclaim(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(c.cfg.DataDir, "chunks", name))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("reclaimer deleted newly unacknowledged evidence", err)
	}
	var present, pending int
	if err := c.db.QueryRowContext(ctx, `SELECT payload_present,ack_at IS NULL FROM chunks`).Scan(&present, &pending); err != nil || present != 1 || pending != 1 {
		t.Fatal("recovery payload state changed", present, pending, err)
	}
}
