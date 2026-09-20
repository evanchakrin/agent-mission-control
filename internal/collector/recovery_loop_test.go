package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecoveryLoopReportsLedgerReadFailure(t *testing.T) {
	c, _ := testCollector(t, nil)
	if err := c.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.recoveryLoop(ctx) }()
	c.wakeRecovery()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "database is closed") {
			t.Fatal("ledger failure not propagated", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("recovery swallowed ledger failure and waited for another wakeup")
	}
}

func TestRecoveryLoopWithoutPendingIntentRemainsIdle(t *testing.T) {
	c, _ := testCollector(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.wakeRecovery()
	if err := c.recoveryLoop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("absent recovery marker was treated as a ledger failure", err)
	}
}
