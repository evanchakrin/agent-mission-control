package collector

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestUploadSourceQueueTracksAcknowledgementsRollbackAndRecovery(t *testing.T) {
	c, root := testCollector(t, nil)
	writeSource(t, root, "fixture.jsonl", bytes.Repeat([]byte("x"), protocol.MaxChunkBytes+1))
	reconcile(t, c)
	capture(t, c, 2)
	count := func(want int) {
		t.Helper()
		var got int
		if err := c.db.QueryRow("SELECT count(*) FROM upload_sources").Scan(&got); err != nil || got != want {
			t.Fatalf("queue count %d want %d: %v", got, want, err)
		}
	}
	count(1)
	if _, err := c.db.Exec("UPDATE sources SET last_upload_attempt=42"); err != nil {
		t.Fatal(err)
	}
	var attempt int
	if err := c.db.QueryRow("SELECT last_attempt FROM upload_sources").Scan(&attempt); err != nil || attempt != 42 {
		t.Fatal(attempt, err)
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE chunks SET ack_at=1"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	count(1)
	if _, err = c.db.Exec("UPDATE chunks SET ack_at=1 WHERE offset=0"); err != nil {
		t.Fatal(err)
	}
	count(1)
	if _, err = c.db.Exec("UPDATE chunks SET ack_at=1"); err != nil {
		t.Fatal(err)
	}
	count(0)
	// Restore reconciliation makes a formerly acknowledged chunk pending again.
	if _, err = c.db.Exec("UPDATE chunks SET ack_at=NULL WHERE offset=0"); err != nil {
		t.Fatal(err)
	}
	count(1)
	if _, err = c.db.Exec("DELETE FROM chunks WHERE ack_at IS NULL"); err != nil {
		t.Fatal(err)
	}
	count(0)
	if _, err = c.db.Exec("UPDATE chunks SET ack_at=NULL"); err != nil {
		t.Fatal(err)
	}
	count(1)
	// Simulate an old installation before the derived queue was introduced.
	if _, err = c.db.Exec(`DROP TRIGGER upload_source_added;DROP TRIGGER upload_source_changed;DROP TRIGGER upload_source_removed;DROP TRIGGER upload_source_attempt;DROP TABLE upload_sources`); err != nil {
		t.Fatal(err)
	}
	if err = setupUploadQueue(c.db); err != nil {
		t.Fatal(err)
	}
	count(1)
	if err = setupUploadQueue(c.db); err != nil {
		t.Fatal(err)
	}
	count(1)
}

func TestUploadLeasePlanStartsFromPendingSourceOrder(t *testing.T) {
	c, _ := testCollector(t, nil)
	now := time.Now().UnixNano()
	rows, err := c.db.Query("EXPLAIN QUERY PLAN "+leaseSelectQuery, now, now)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "COVERING INDEX upload_sources_order") || strings.Contains(plan.String(), "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("lease lost bounded source ordering:\n%s", plan.String())
	}
}
