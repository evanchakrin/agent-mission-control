package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type fixtureAddress string

func (a fixtureAddress) Network() string { return "fixture" }
func (a fixtureAddress) String() string  { return string(a) }

func TestNetworkReportBoundsAndUnknown(t *testing.T) {
	at := time.Now().UTC()
	input := []net.Addr{fixtureAddress("127.0.0.1/8"), fixtureAddress("0.0.0.0/0"), fixtureAddress("224.0.0.1/4"), fixtureAddress("bad"), fixtureAddress("192.168.1.2/24"), fixtureAddress("::ffff:192.168.1.2/128"), fixtureAddress("2001:db8::2/64")}
	r := makeNetworkReport(input, nil, at)
	if !r.Valid() || !reflect.DeepEqual(r.Addresses, []string{"192.168.1.2", "2001:db8::2"}) || r.Truncated {
		t.Fatal(r)
	}
	for i := 0; i < 100; i++ {
		input = append(input, fixtureAddress(fmt.Sprintf("10.0.0.%d/24", i+1)))
	}
	r = makeNetworkReport(input, nil, at)
	if !r.Valid() || len(r.Addresses) != 32 || !r.Truncated {
		t.Fatal(r)
	}
	r = makeNetworkReport(input, errors.New("private OS error"), at)
	if !r.Valid() || r.State != "unavailable" || len(r.Addresses) != 0 {
		t.Fatal(r)
	}
}

func TestHeartbeatUsesCachedNetworkObservation(t *testing.T) {
	var received protocol.Heartbeat
	c, _ := testCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	report := makeNetworkReport([]net.Addr{fixtureAddress("10.2.3.4/24")}, nil, time.Now().UTC())
	c.networkReport = &report
	if err := c.sendHeartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(received.Network, &report) {
		t.Fatal("cached network observation changed in transit", received.Network)
	}
}
