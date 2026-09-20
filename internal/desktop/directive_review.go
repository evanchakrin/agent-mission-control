package desktop

import (
	"encoding/json"
	"strings"
)

// The review precondition covers the entire rule, including pending work and
// target evidence. Reviewing a stale body must not approve a newer body.
type directiveView struct {
	Directive
	StateHash string `json:"stateHash"`
}

func viewDirective(d Directive) directiveView {
	raw, _ := json.Marshal(d)
	return directiveView{Directive: d, StateHash: hash(raw)}
}

func (m *Manager) reviewDirective(b input) (any, error) {
	if len(b.OperationID) == 0 || len(b.OperationID) > 128 || strings.ContainsAny(b.OperationID, " \t\r\n/\\") || len(b.ExpectedHash) != 64 {
		return nil, fail(400, "review requires an operation identity and observed rule hash")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	requestHash, key := hash(raw), "directive-review-operation:"+b.OperationID
	var prior localOperationReceipt
	if err := m.load(key, &prior); err != nil {
		return nil, err
	}
	if prior.RequestHash != "" {
		if prior.RequestHash != requestHash {
			return nil, fail(409, "operation identity was already used for a different review")
		}
		return prior.Result, nil
	}
	items, err := m.directiveItems()
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID != b.ID {
			continue
		}
		if viewDirective(items[i]).StateHash != b.ExpectedHash {
			return nil, fail(409, "standing order changed; reload and review its current content")
		}
		if items[i].PendingMeasurement != nil {
			return nil, fail(409, "complete pending remeasurement before marking this rule reviewed")
		}
		items[i].LastReviewedAt = now()
		result := map[string]any{"ok": true, "id": b.ID, "operationId": b.OperationID, "item": viewDirective(items[i])}
		err = m.saveOperation("directives", items, "directive-reviewed", key, &localOperationReceipt{RequestHash: requestHash, Result: result})
		return result, err
	}
	return nil, fail(409, "standing order was removed; reload before reviewing it")
}
