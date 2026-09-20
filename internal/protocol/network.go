package protocol

import (
	"net/netip"
	"time"
)

const MaxReportedAddresses = 32

// A collector observation, not an identity or proof of network reachability.
type NetworkReport struct {
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observedAt"`
	Addresses  []string  `json:"addresses"`
	Truncated  bool      `json:"truncated"`
}

func (r *NetworkReport) Valid() bool {
	if r == nil {
		return true
	}
	if r.ObservedAt.IsZero() || len(r.Addresses) > MaxReportedAddresses {
		return false
	}
	if r.State == "unavailable" {
		return len(r.Addresses) == 0 && !r.Truncated
	}
	if r.State != "reported" {
		return false
	}
	seen := map[string]bool{}
	for _, value := range r.Addresses {
		address, err := netip.ParseAddr(value)
		if err != nil || address.IsLoopback() || address.IsMulticast() || address.IsUnspecified() || address.Unmap().String() != value || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
