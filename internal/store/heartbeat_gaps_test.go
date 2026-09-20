package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestHeartbeatHistoryGapsPreserveUnknownZeroAndRecordedCounts(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	h := protocol.Heartbeat{MachineID: "machine"}
	for _, value := range []*int64{nil, new(int64), func() *int64 { v := int64(3); return &v }()} {
		h.HistoryGaps = value
		if err := s.RecordHeartbeat(ctx, h); err != nil {
			t.Fatal(err)
		}
		machines, err := s.ListMachines(ctx)
		if err != nil || len(machines) != 1 {
			t.Fatal(err)
		}
		got := machines[0].Heartbeat.HistoryGaps
		if (got == nil) != (value == nil) || (got != nil && *got != *value) {
			t.Fatal("gap count changed")
		}
		encoded, err := json.Marshal(machines[0].Heartbeat)
		if err != nil || strings.Contains(string(encoded), "historyGaps") != (value != nil) {
			t.Fatal("unknown gap count became reported zero", err)
		}
	}
	negative := int64(-1)
	h.HistoryGaps = &negative
	if err := s.RecordHeartbeat(ctx, h); !errors.Is(err, ErrInvalid) {
		t.Fatal("negative history gaps accepted", err)
	}
}
