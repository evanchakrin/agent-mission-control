package protocol

import (
	"testing"
	"time"
)

func TestNetworkReportValidation(t *testing.T) {
	if !(*NetworkReport)(nil).Valid() {
		t.Fatal("old collectors rejected")
	}
	for _, values := range [][]string{{"text"}, {"127.0.0.1"}, {"::"}, {"224.0.0.1"}, {"::ffff:10.0.0.1"}, {"10.0.0.1", "10.0.0.1"}, make([]string, 33)} {
		if (&NetworkReport{State: "reported", ObservedAt: time.Now(), Addresses: values}).Valid() {
			t.Fatal("invalid report accepted", values)
		}
	}
	if (&NetworkReport{State: "reported"}).Valid() {
		t.Fatal("missing observation time accepted")
	}
	if (&NetworkReport{State: "unavailable", ObservedAt: time.Now(), Addresses: []string{"10.0.0.1"}}).Valid() {
		t.Fatal("failed observation retained addresses")
	}
}
