package collector

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type failedReceiptReader struct{}

func (failedReceiptReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDecodeReceiptRequiresCompleteBoundedResponse(t *testing.T) {
	const valid = `{"sourceId":"source"}`
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", valid, true},
		{"whitespace", valid + " \r\n\t", true},
		{"exact-limit", valid + strings.Repeat(" ", (64<<10)-len(valid)), true},
		{"over-limit", valid + strings.Repeat(" ", (64<<10)-len(valid)+1), false},
		{"second-object", valid + `{}`, false},
		{"partial", `{"sourceId":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var receipt protocol.Receipt
			err := decodeReceipt(strings.NewReader(tc.body), &receipt)
			if (err == nil) != tc.ok {
				t.Fatalf("decode error = %v, want success %v", err, tc.ok)
			}
		})
	}
	var receipt protocol.Receipt
	if err := decodeReceipt(io.MultiReader(strings.NewReader(valid), failedReceiptReader{}), &receipt); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("accepted incomplete transport: %v", err)
	}
}
