package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

var ErrInheritanceScanIncomplete = errors.New("inheritance inspection page budget exhausted; result unverified")

type inheritanceReader interface {
	GetSession(context.Context, string) (store.Session, error)
	GetUsage(context.Context, string, string, int) ([]store.UsageObservation, error)
	SourceOffset(context.Context, string, string) (protocol.Receipt, error)
}

type ForkBaselineInspection struct {
	State  string             `json:"state"`
	Reason string             `json:"reason,omitempty"`
	Pages  int                `json:"pages"`
	Match  *ForkBaselineMatch `json:"match,omitempty"`
}

// InspectForkBaseline is read-only. It retains at most one 500-observation page
// and at most two first-counter pieces. The caller selects the parent explicitly; a
// match does not resolve duplicate parent identities or apply an exclusion.
func InspectForkBaseline(ctx context.Context, s inheritanceReader, childID, parentID string, maxPages int) (ForkBaselineInspection, error) {
	result := ForkBaselineInspection{State: "unverified"}
	if childID == "" || parentID == "" || len(childID) > 512 || len(parentID) > 512 || childID == parentID || maxPages < 1 || maxPages > 200 {
		return result, store.ErrInvalid
	}
	child, err := s.GetSession(ctx, childID)
	if err != nil {
		return result, err
	}
	parent, err := s.GetSession(ctx, parentID)
	if err != nil {
		return result, err
	}
	if child.Provider != "codex" || parent.Provider != "codex" || child.MachineID != parent.MachineID || child.SourceID == parent.SourceID || parent.NativeID == "" || child.ForkedFromID != parent.NativeID {
		result.Reason = "explicit same-machine fork relationship unavailable"
		return result, nil
	}
	before := make([]protocol.Receipt, 2)
	for i, session := range []store.Session{child, parent} {
		before[i], err = s.SourceOffset(ctx, session.SourceID, session.Generation)
		if err != nil {
			return result, err
		}
		if before[i].SourceID != session.SourceID || before[i].Generation != session.Generation || before[i].IndexedOffset < 0 {
			return result, store.ErrConflict
		}
	}
	walk := func(session string, visit func(store.UsageObservation) (bool, error)) error {
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if result.Pages >= maxPages {
				return ErrInheritanceScanIncomplete
			}
			rows, err := s.GetUsage(ctx, session, after, 500)
			if err != nil {
				return err
			}
			result.Pages++
			if len(rows) > 500 {
				return store.ErrInvalid
			}
			for _, row := range rows {
				if row.ID <= after || row.SessionID != session {
					return store.ErrConflict
				}
				after = row.ID
				stop, err := visit(row)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
			if len(rows) < 500 {
				return nil
			}
		}
	}
	var first []store.UsageObservation
	err = walk(child.ID, func(u store.UsageObservation) (bool, error) {
		if u.AgentID != "main" || u.CounterScope != "thread:"+child.NativeID || (u.Kind != "incomplete-attribution" && u.Kind != "reported-last-call") {
			return false, nil
		}
		var e counterEvidence
		if json.Unmarshal(u.Evidence, &e) != nil || e.Previous == nil || *e.Previous != (parser.Counter{}) || !validEvidenceCounter(e.Counter) {
			return false, nil
		}
		if len(first) == 2 {
			return false, store.ErrConflict
		}
		first = append(first, u)
		return false, nil
	})
	if err != nil {
		return result, err
	}
	if len(first) == 0 {
		result.Reason = "no supported first counter; attribution requires further evidence"
	} else {
		err = walk(parent.ID, func(u store.UsageObservation) (bool, error) {
			match, ok := MatchForkBaselineGroup(child, parent, first, u)
			if ok {
				result.Match = &match
			}
			return ok, nil
		})
		if err != nil {
			return result, err
		}
		if result.Match == nil {
			result.Reason = "no matching counter in selected parent projection"
		}
	}
	for i, session := range []store.Session{child, parent} {
		after, err := s.GetSession(ctx, session.ID)
		if err != nil {
			return ForkBaselineInspection{State: "unverified"}, err
		}
		offset, err := s.SourceOffset(ctx, session.SourceID, session.Generation)
		if err != nil {
			return ForkBaselineInspection{State: "unverified"}, err
		}
		if !reflect.DeepEqual(session, after) || offset.SourceID != session.SourceID || offset.Generation != session.Generation || before[i].IndexedOffset != offset.IndexedOffset || before[i].RecoveryEpoch != offset.RecoveryEpoch {
			return ForkBaselineInspection{State: "unverified"}, store.ErrConflict
		}
	}
	if result.Match != nil {
		// A matched counter must belong to the published prefix, not merely
		// contain a plausible offset in its JSON evidence. Capture may be ahead
		// of indexing; uploaded bytes alone cannot establish this relationship.
		if result.Match.ChildOffset >= before[0].IndexedOffset || result.Match.ParentOffset >= before[1].IndexedOffset {
			return ForkBaselineInspection{State: "unverified", Reason: "counter evidence outside published history", Pages: result.Pages}, store.ErrConflict
		}
		result.State = "matched-baseline"
	}
	return result, nil
}
