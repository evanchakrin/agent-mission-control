package desktop

import (
	"encoding/json"
	"strings"
)

type plantOperation struct {
	key, requestHash, id, blockHash string
}

func (m *Manager) loadPlantOperation(b input) (*plantOperation, map[string]any, error) {
	if len(b.OperationID) > 128 || strings.ContainsAny(b.OperationID, " \t\r\n/\\") {
		return nil, nil, fail(400, "invalid planting operation identity")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, nil, err
	}
	o := &plantOperation{key: "directive-plant-operation:" + b.OperationID, requestHash: hash(raw)}
	for _, key := range []string{o.key, o.key + ":intent"} {
		var receipt localOperationReceipt
		if err := m.load(key, &receipt); err != nil {
			return nil, nil, err
		}
		if receipt.RequestHash == "" {
			continue
		}
		if receipt.RequestHash != o.requestHash {
			return nil, nil, fail(409, "operation identity was already used for different planting content")
		}
		if key == o.key {
			return o, receipt.Result, nil
		}
		var ok bool
		o.id, ok = receipt.Result["id"].(string)
		if !ok || o.id == "" {
			return nil, nil, fail(409, "planting intent is damaged; inspect before recovery")
		}
		o.blockHash, ok = receipt.Result["blockHash"].(string)
		if !ok || len(o.blockHash) != 64 {
			return nil, nil, fail(409, "planting intent checksum is damaged")
		}
	}
	return o, nil, nil
}
