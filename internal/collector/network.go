package collector

import (
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func (c *Collector) refreshNetworkReport() {
	addresses, err := net.InterfaceAddrs() // Local interface enumeration only; no DNS or probes.
	report := makeNetworkReport(addresses, err, time.Now().UTC())
	c.mu.Lock()
	c.networkReport = &report
	c.mu.Unlock()
}

func makeNetworkReport(addresses []net.Addr, err error, at time.Time) protocol.NetworkReport {
	r := protocol.NetworkReport{State: "reported", ObservedAt: at, Addresses: []string{}}
	if err != nil {
		r.State = "unavailable"
		return r
	}
	seen := map[string]bool{}
	for _, item := range addresses {
		prefix, e := netip.ParsePrefix(item.String())
		if e != nil {
			continue
		}
		address := prefix.Addr().Unmap()
		if address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() {
			continue
		}
		value := address.String()
		if seen[value] {
			continue
		}
		// Bounded report state even on hosts with unusually many addresses.
		if len(r.Addresses) == protocol.MaxReportedAddresses {
			r.Truncated = true
			continue
		}
		seen[value] = true
		r.Addresses = append(r.Addresses, value)
	}
	sort.Strings(r.Addresses)
	return r
}
