package store

import (
	"context"
	"testing"
	"time"
)

func TestPricingInitializationIsRetryableAndReadsAvoidWriter(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.InitializePricingQueue(cancelled); err == nil {
		t.Fatal("cancelled initialization succeeded")
	}
	if s.pricingReady.Load() || s.accountingReady.Load() {
		t.Fatal("failed initialization cached")
	}
	if err = s.InitializePricingQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err = s.db.QueryRow(`PRAGMA schema_version`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	s.writeMu.Lock()
	finished := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if e := s.InitializePricingQueue(context.Background()); e != nil {
				finished <- e
				return
			}
			if e := s.InitializeAccounting(context.Background()); e != nil {
				finished <- e
				return
			}
		}
		finished <- nil
	}()
	select {
	case err = <-finished:
		s.writeMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.writeMu.Unlock()
		<-finished
		t.Fatal("initialized read path waited for database writer")
	}
	if err = s.db.QueryRow(`PRAGMA schema_version`).Scan(&after); err != nil || before != after {
		t.Fatal("status initialization changed schema", before, after, err)
	}
}
