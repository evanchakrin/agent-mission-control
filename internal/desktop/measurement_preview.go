package desktop

// Persist the measured evidence shown to the owner. Apply reads this record,
// never a client-supplied replacement body or a newly sampled measurement.
type measurementPreview struct {
	DirectiveID string      `json:"directiveId"`
	StateHash   string      `json:"stateHash"`
	Measurement Measurement `json:"measurement"`
}
