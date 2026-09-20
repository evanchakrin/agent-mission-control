package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type inheritanceFixture struct {
	sessions        map[string]store.Session
	usage           map[string][]store.UsageObservation
	change          bool
	repeat          bool
	maxPage         int
	receiptOverride func(string, protocol.Receipt) protocol.Receipt
}

func (f *inheritanceFixture) GetSession(_ context.Context, id string) (store.Session, error) {
	return f.sessions[id], nil
}
func (f *inheritanceFixture) SourceOffset(_ context.Context, source, generation string) (protocol.Receipt, error) {
	r := protocol.Receipt{SourceID: source, Generation: generation, IndexedOffset: 1000, RecoveryEpoch: "epoch"}
	if f.receiptOverride != nil {
		r = f.receiptOverride(source, r)
	}
	return r, nil
}

func TestForkBaselineInspectionRequiresPublishedCounterOffsets(t *testing.T) {
	for _, side := range []string{"child", "parent"} {
		for _, issue := range []string{"at-boundary", "after-boundary", "negative-index", "wrong-source", "wrong-generation", "changed-generation"} {
			t.Run(side+"/"+issue, func(t *testing.T) {
				f := inheritanceTestFixture(t)
				calls := 0
				f.receiptOverride = func(source string, r protocol.Receipt) protocol.Receipt {
					if source != side {
						return r
					}
					calls++
					switch issue {
					case "at-boundary":
						r.IndexedOffset = 123
					case "after-boundary":
						r.IndexedOffset = 122
					case "negative-index":
						r.IndexedOffset = -1
					case "wrong-source":
						r.SourceID = "other"
					case "wrong-generation":
						r.Generation = "other"
					case "changed-generation":
						if calls > 1 {
							r.Generation = "other"
						}
					}
					return r
				}
				result, err := InspectForkBaseline(context.Background(), f, "child", "parent", 10)
				if !errors.Is(err, store.ErrConflict) || result.Match != nil || result.State != "unverified" {
					t.Fatal("accepted unpublished evidence", result, err)
				}
			})
		}
	}
	f := inheritanceTestFixture(t)
	f.receiptOverride = func(_ string, r protocol.Receipt) protocol.Receipt { r.IndexedOffset = 124; return r }
	result, err := InspectForkBaseline(context.Background(), f, "child", "parent", 10)
	if err != nil || result.State != "matched-baseline" {
		t.Fatal("rejected counter inside published prefix", result, err)
	}
}
func (f *inheritanceFixture) GetUsage(_ context.Context, id, after string, limit int) ([]store.UsageObservation, error) {
	if limit > f.maxPage {
		f.maxPage = limit
	}
	if f.change && id == "parent" {
		p := f.sessions[id]
		p.ProjectionRevision = "changed"
		f.sessions[id] = p
	}
	if f.repeat {
		after = ""
	}
	rows := []store.UsageObservation{}
	for _, u := range f.usage[id] {
		if u.ID > after {
			rows = append(rows, u)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}
func inheritanceTestFixture(t *testing.T) *inheritanceFixture {
	t.Helper()
	f := &inheritanceFixture{sessions: map[string]store.Session{}, usage: map[string][]store.UsageObservation{}}
	for _, id := range []string{"child", "parent"} {
		f.sessions[id] = store.Session{ID: id, SourceID: id, Generation: "g", MachineID: "machine", Provider: "codex", NativeID: id}
		for i := 0; i < 501; i++ {
			f.usage[id] = append(f.usage[id], store.UsageObservation{ID: fmt.Sprintf("%04d", i), SessionID: id, Kind: "padding"})
		}
	}
	c := f.sessions["child"]
	c.ForkedFromID = "parent"
	f.sessions["child"] = c
	offset := int64(123)
	evidence, _ := json.Marshal(counterEvidence{Offset: &offset, Counter: parser.Counter{Set: true, Input: 100, Cache: 50, Output: 5}, Previous: &parser.Counter{}})
	for _, id := range []string{"child", "parent"} {
		i := 0
		if id == "parent" {
			i = 500
		}
		f.usage[id][i] = store.UsageObservation{ID: fmt.Sprintf("%04d", i), SessionID: id, AgentID: "main", CounterScope: "thread:" + id, Kind: "incomplete-attribution", TokensIn: 50, TokensCache: 50, TokensOut: 5, Evidence: evidence}
	}
	return f
}

func TestForkBaselineInspectionPagesAndRejectsChangedEvidence(t *testing.T) {
	for _, name := range []string{"match", "split-across-pages", "budget", "changed", "repeated-page", "canceled", "no-fork"} {
		t.Run(name, func(t *testing.T) {
			f := inheritanceTestFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pages := 10
			switch name {
			case "split-across-pages":
				prior := f.usage["child"][0]
				last := prior
				last.ID = "0500"
				last.Kind = "reported-last-call"
				last.Model = "known-model"
				last.TokensIn, last.TokensCache, last.TokensOut = 10, 10, 1
				prior.TokensIn, prior.TokensCache, prior.TokensOut = 40, 40, 4
				f.usage["child"][0], f.usage["child"][500] = prior, last
			case "budget":
				pages = 1
			case "changed":
				f.change = true
			case "repeated-page":
				f.repeat = true
			case "canceled":
				cancel()
			case "no-fork":
				c := f.sessions["child"]
				c.ForkedFromID = ""
				f.sessions["child"] = c
			}
			result, err := InspectForkBaseline(ctx, f, "child", "parent", pages)
			switch name {
			case "split-across-pages":
				if err != nil || result.State != "matched-baseline" || result.Pages != 4 || result.Match == nil || len(result.Match.ChildObservationIDs) != 2 || result.Match.ChildObservationID != "" || result.Match.TokensIn != 50 || result.Match.TokensCache != 50 || result.Match.TokensOut != 5 {
					t.Fatal(result, err)
				}
			case "match":
				if err != nil || result.State != "matched-baseline" || result.Pages != 4 || result.Match == nil || result.Match.ParentObservationID != "0500" {
					t.Fatal(result, err)
				}
			case "budget":
				if !errors.Is(err, ErrInheritanceScanIncomplete) {
					t.Fatal(result, err)
				}
			case "changed", "repeated-page":
				if !errors.Is(err, store.ErrConflict) || result.Match != nil {
					t.Fatal(result, err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(result, err)
				}
			case "no-fork":
				if err != nil || result.State != "unverified" || result.Pages != 0 || result.Match != nil {
					t.Fatal(result, err)
				}
			}
			if f.maxPage > 500 {
				t.Fatal("unbounded page", f.maxPage)
			}
		})
	}
}
