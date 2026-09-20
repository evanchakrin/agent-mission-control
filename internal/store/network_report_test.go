package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestHeartbeatNetworkReportValidationAndPersistence(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	r := &protocol.NetworkReport{State: "reported", ObservedAt: time.Now().UTC(), Addresses: []string{"not-an-address"}}
	h := protocol.Heartbeat{MachineID: "network-machine", Network: r}
	if err := s.RecordHeartbeat(ctx, h); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid address persisted", err)
	}
	r.Addresses = []string{"10.2.3.4", "2001:db8::1"}
	if err := s.RecordHeartbeat(ctx, h); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMachines(ctx)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Heartbeat.Network, r) {
		t.Fatal("network observation changed", rows, err)
	}
}
