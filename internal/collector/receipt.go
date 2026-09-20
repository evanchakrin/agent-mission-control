package collector

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

// A valid JSON prefix is not a complete receipt. Read one byte beyond the
// bounded ceiling to detect oversize replies, then reject trailing content.
func decodeReceipt(r io.Reader, receipt *protocol.Receipt) error {
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("receipt exceeds 64 KiB")
	}
	return json.Unmarshal(raw, receipt)
}
