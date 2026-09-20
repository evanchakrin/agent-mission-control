package store

import "fmt"

var usageFields = []string{"tokensIn", "tokensCache", "tokensCacheWrite", "tokensOut", "model"}

// mergeUsage executes inside CommitIndex's transaction. Message streams can
// update output_tokens without repeating input/cache/model. Keeping this merge
// in the ledger avoids a history-sized parser checkpoint or dropping old fields
// when an old message is revised after a restart.
func mergeUsage(old, next UsageObservation) (UsageObservation, error) {
	if next.Present == nil {
		return next, nil
	}
	if next.Kind != "message-partial" && next.Kind != "message-final" && next.Kind != "message-without-id" {
		return next, fmt.Errorf("%w: field presence requires provider-message usage", ErrInvalid)
	}
	present, known := map[string]bool{}, map[string]bool{}
	for _, field := range next.Present {
		valid := false
		for _, f := range usageFields {
			valid = valid || field == f
		}
		if !valid || present[field] {
			return next, fmt.Errorf("%w: usage field presence", ErrInvalid)
		}
		present[field], known[field] = true, true
	}
	for _, field := range old.Present {
		known[field] = true
	}
	// A pre-mask complete observation already has all buckets. A historically
	// partial observation is never upgraded by assuming missing fields were zero.
	if old.ID != "" && old.Present == nil && old.Kind == "message-final" {
		for _, field := range usageFields {
			known[field] = true
		}
	}
	if !present["tokensIn"] {
		next.TokensIn = old.TokensIn
	}
	if !present["tokensCache"] {
		next.TokensCache = old.TokensCache
	}
	if !present["tokensCacheWrite"] {
		next.TokensCacheWrite = old.TokensCacheWrite
	}
	if !present["tokensOut"] {
		next.TokensOut = old.TokensOut
	}
	if !present["model"] {
		next.Model = old.Model
	}
	if next.Timestamp.IsZero() {
		next.Timestamp = old.Timestamp
	}
	next.Present = make([]string, 0, len(usageFields))
	for _, field := range usageFields {
		if known[field] {
			next.Present = append(next.Present, field)
		}
	}
	if next.Kind != "message-without-id" {
		next.Kind = "message-partial"
		if len(next.Present) == len(usageFields) && next.Model != "" {
			next.Kind = "message-final"
		}
	}
	return next, nil
}
