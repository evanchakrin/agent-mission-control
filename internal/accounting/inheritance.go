package accounting

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// ForkBaselineMatch records evidence for a shared starting counter, not an
// instruction to delete observations or merge sessions. Applying a correction
// still requires current-revision checks and one canonical parent contribution.
type ForkBaselineMatch struct {
	ChildSessionID      string   `json:"childSessionId"`
	ParentSessionID     string   `json:"parentSessionId"`
	ChildGeneration     string   `json:"childGeneration"`
	ParentGeneration    string   `json:"parentGeneration"`
	ChildRevision       string   `json:"childRevision"`
	ParentRevision      string   `json:"parentRevision"`
	ChildObservationID  string   `json:"childObservationId"`
	ChildObservationIDs []string `json:"childObservationIds,omitempty"`
	ParentObservationID string   `json:"parentObservationId"`
	ChildOffset         int64    `json:"childOffset"`
	ParentOffset        int64    `json:"parentOffset"`
	TokensIn            int64    `json:"tokensIn"`
	TokensCache         int64    `json:"tokensCache"`
	TokensCacheWrite    int64    `json:"tokensCacheWrite"`
	TokensOut           int64    `json:"tokensOut"`
}

// MatchForkBaselineGroup reunites the at-most-two observations emitted from a
// first counter: unattributed prior history and the reported last call. Neither
// component alone establishes the full inherited baseline. Raw observations and
// their original model attribution are retained unchanged.
func MatchForkBaselineGroup(child, parent store.Session, first []store.UsageObservation, parentUsage store.UsageObservation) (ForkBaselineMatch, bool) {
	var empty ForkBaselineMatch
	if len(first) < 1 || len(first) > 2 {
		return empty, false
	}
	combined := first[0]
	combined.TokensIn, combined.TokensCache, combined.TokensCacheWrite, combined.TokensOut = 0, 0, 0, 0
	combined.Kind, combined.Model = "incomplete-attribution", ""
	var initial counterEvidence
	if json.Unmarshal(first[0].Evidence, &initial) != nil {
		return empty, false
	}
	ids := make([]string, 0, len(first))
	kinds := map[string]bool{}
	for _, u := range first {
		if u.ID == "" || u.SessionID != combined.SessionID || u.AgentID != combined.AgentID || u.CounterScope != combined.CounterScope || kinds[u.Kind] {
			return empty, false
		}
		if u.Kind == "incomplete-attribution" {
			if u.Model != "" {
				return empty, false
			}
		} else if u.Kind == "reported-last-call" {
			if u.Model == "" {
				return empty, false
			}
		} else {
			return empty, false
		}
		for _, id := range ids {
			if id == u.ID {
				return empty, false
			}
		}
		var evidence counterEvidence
		if json.Unmarshal(u.Evidence, &evidence) != nil || !reflect.DeepEqual(initial, evidence) {
			return empty, false
		}
		for _, bucket := range []struct {
			total *int64
			value int64
		}{{&combined.TokensIn, u.TokensIn}, {&combined.TokensCache, u.TokensCache}, {&combined.TokensCacheWrite, u.TokensCacheWrite}, {&combined.TokensOut, u.TokensOut}} {
			if bucket.value < 0 || bucket.value > math.MaxInt64-*bucket.total {
				return empty, false
			}
			*bucket.total += bucket.value
		}
		ids = append(ids, u.ID)
		kinds[u.Kind] = true
	}
	match, ok := MatchForkBaseline(child, parent, combined, parentUsage)
	if !ok {
		return empty, false
	}
	sort.Strings(ids)
	match.ChildObservationIDs = ids
	if len(ids) > 1 {
		match.ChildObservationID = ""
	}
	return match, true
}

type counterEvidence struct {
	Offset   *int64          `json:"offset"`
	Counter  parser.Counter  `json:"counter"`
	Previous *parser.Counter `json:"previousCounter"`
}

func validEvidenceCounter(c parser.Counter) bool {
	return c.Set && c.Epoch >= 0 && c.Input >= 0 && c.Cache >= 0 && c.CacheWrite >= 0 && c.Output >= 0 &&
		c.Cache <= c.Input && c.CacheWrite <= c.Input-c.Cache && c.Output <= math.MaxInt64-c.Input
}

// MatchForkBaseline compares two already-selected published observations using
// fixed memory. A spawn relationship alone, similar total, model alias, or
// timestamp is insufficient. Independent child deltas are never candidates.
// Absence of a match means unverified, not proof of independent usage. Split
// first-counter attribution is intentionally left for reconciliation as a group.
func MatchForkBaseline(child, parent store.Session, first, parentUsage store.UsageObservation) (ForkBaselineMatch, bool) {
	var match ForkBaselineMatch
	if child.ID == "" || parent.ID == "" || child.ID == parent.ID || child.SourceID == "" || parent.SourceID == "" || child.SourceID == parent.SourceID ||
		child.Generation == "" || parent.Generation == "" || child.MachineID == "" || child.MachineID != parent.MachineID ||
		child.Provider != "codex" || parent.Provider != "codex" || child.NativeID == "" || parent.NativeID == "" || child.NativeID == parent.NativeID || child.ForkedFromID != parent.NativeID {
		return match, false
	}
	if first.ID == "" || parentUsage.ID == "" || first.SessionID != child.ID || parentUsage.SessionID != parent.ID ||
		first.AgentID != "main" || parentUsage.AgentID != "main" || first.CounterScope != "thread:"+child.NativeID || parentUsage.CounterScope != "thread:"+parent.NativeID ||
		first.Kind != "incomplete-attribution" || first.Model != "" {
		return match, false
	}
	switch parentUsage.Kind {
	case "incomplete-attribution", "cumulative-delta", "reported-last-call":
	default:
		return match, false
	}
	var a, b counterEvidence
	if json.Unmarshal(first.Evidence, &a) != nil || json.Unmarshal(parentUsage.Evidence, &b) != nil ||
		a.Offset == nil || b.Offset == nil || *a.Offset < 0 || *b.Offset < 0 || !validEvidenceCounter(a.Counter) || !validEvidenceCounter(b.Counter) ||
		a.Previous == nil || *a.Previous != (parser.Counter{}) {
		return match, false
	}
	x, y := a.Counter, b.Counter
	if x.Input != y.Input || x.Cache != y.Cache || x.CacheWrite != y.CacheWrite || x.Output != y.Output ||
		first.TokensIn != x.Input-x.Cache-x.CacheWrite || first.TokensCache != x.Cache || first.TokensCacheWrite != x.CacheWrite || first.TokensOut != x.Output ||
		x.Input+x.Output == 0 {
		return match, false
	}
	return ForkBaselineMatch{
		ChildSessionID: child.ID, ParentSessionID: parent.ID, ChildGeneration: child.Generation, ParentGeneration: parent.Generation,
		ChildRevision: child.ProjectionRevision, ParentRevision: parent.ProjectionRevision,
		ChildObservationID: first.ID, ParentObservationID: parentUsage.ID, ChildOffset: *a.Offset, ParentOffset: *b.Offset,
		TokensIn: first.TokensIn, TokensCache: first.TokensCache, TokensCacheWrite: first.TokensCacheWrite, TokensOut: first.TokensOut,
	}, true
}
