package accounting

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// DuplicateUsageMatch identifies repeated observations across copies of one
// native Codex thread. It is not a session merge: differing messages remain
// independently accessible. Publication must still check source revisions and
// select exactly one contribution owner (including fork-baseline exclusions).
type DuplicateUsageMatch struct {
	EvidenceHash       string `json:"evidenceHash"`
	LeftObservationID  string `json:"leftObservationId"`
	RightObservationID string `json:"rightObservationId"`
	LeftSessionID      string `json:"leftSessionId"`
	RightSessionID     string `json:"rightSessionId"`
	LeftRevision       string `json:"leftRevision"`
	RightRevision      string `json:"rightRevision"`
	LeftGeneration     string `json:"leftGeneration"`
	RightGeneration    string `json:"rightGeneration"`
	LeftOffset         int64  `json:"leftOffset"`
	RightOffset        int64  `json:"rightOffset"`
}

// MatchDuplicateCodexUsage requires the same native identity, timestamp, full
// counter transition, attribution, and all token buckets. Equal session totals
// or a matching ending counter alone are insufficient. Only copy-specific IDs
// and byte offsets are omitted from the semantic comparison.
func MatchDuplicateCodexUsage(left, right store.Session, a, b store.UsageObservation) (DuplicateUsageMatch, bool) {
	var result DuplicateUsageMatch
	if left.ID == "" || right.ID == "" || left.ID == right.ID || left.SourceID == "" || right.SourceID == "" || left.SourceID == right.SourceID ||
		left.Generation == "" || right.Generation == "" || left.MachineID == "" || left.MachineID != right.MachineID ||
		left.Provider != "codex" || right.Provider != "codex" || left.NativeID == "" || left.NativeID != right.NativeID {
		return result, false
	}
	x, offsetA, ok := duplicateUsageEvidence(left, a)
	if !ok {
		return result, false
	}
	y, offsetB, ok := duplicateUsageEvidence(right, b)
	if !ok || !bytes.Equal(x, y) || a.ID == b.ID {
		return result, false
	}
	hash := sha256.Sum256(x)
	return DuplicateUsageMatch{
		EvidenceHash: hex.EncodeToString(hash[:]), LeftObservationID: a.ID, RightObservationID: b.ID,
		LeftSessionID: left.ID, RightSessionID: right.ID, LeftRevision: left.ProjectionRevision, RightRevision: right.ProjectionRevision,
		LeftGeneration: left.Generation, RightGeneration: right.Generation,
		LeftOffset: offsetA, RightOffset: offsetB,
	}, true
}

func duplicateUsageEvidence(session store.Session, u store.UsageObservation) ([]byte, int64, bool) {
	if u.ID == "" || u.SessionID != session.ID || u.AgentID != "main" || u.CounterScope != "thread:"+session.NativeID || u.Timestamp.IsZero() || len(u.Present) != 0 {
		return nil, 0, false
	}
	switch u.Kind {
	case "incomplete-attribution":
		if u.Model != "" {
			return nil, 0, false
		}
	case "cumulative-delta", "reported-last-call":
		if u.Model == "" {
			return nil, 0, false
		}
	default:
		return nil, 0, false
	}
	var total int64
	for _, n := range []int64{u.TokensIn, u.TokensCache, u.TokensCacheWrite, u.TokensOut} {
		if n < 0 || n > math.MaxInt64-total {
			return nil, 0, false
		}
		total += n
	}
	if total == 0 {
		return nil, 0, false
	}
	var evidence counterEvidence
	if json.Unmarshal(u.Evidence, &evidence) != nil || evidence.Offset == nil || *evidence.Offset < 0 || evidence.Previous == nil || !validEvidenceCounter(evidence.Counter) {
		return nil, 0, false
	}
	// Preserve every evidence field, including unknown future fields. Do not
	// round JSON numbers through float64 or drop model/reset/last-call evidence.
	var fields map[string]json.RawMessage
	if json.Unmarshal(u.Evidence, &fields) != nil {
		return nil, 0, false
	}
	delete(fields, "offset")
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, 0, false
	}
	u.ID, u.SessionID = "", ""
	u.Timestamp = u.Timestamp.UTC()
	u.Evidence = raw
	key, err := json.Marshal(u)
	return key, *evidence.Offset, err == nil
}
