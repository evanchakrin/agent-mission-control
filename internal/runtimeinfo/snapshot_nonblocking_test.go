package runtimeinfo

import (
	"testing"
	"time"
)

func TestSnapshotDoesNotWaitForProgressPersistence(t *testing.T) {
	j, err := Begin(t.TempDir(), "hub", "snapshot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Finish("shutdown")
	want := j.Snapshot()
	// The writer holds this lock through fsync/rename. Health must keep
	// serving committed state even if that storage operation cannot finish.
	j.mu.Lock()
	defer j.mu.Unlock()
	got := make(chan Snapshot, 1)
	go func() { got <- j.Snapshot() }()
	select {
	case value := <-got:
		if value != want {
			t.Fatal("snapshot changed before persistence", value)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("health snapshot waited behind persistence lock")
	}
}
