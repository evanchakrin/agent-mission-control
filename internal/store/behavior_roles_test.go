package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestBehaviorRoleCountsUnknownsAndNames(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	start := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		src := testSource()
		src.SourceID = fmt.Sprint("role-source-", i)
		if i == 5 {
			src.MachineID = "other-machine"
		}
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprint("role-session-", i)
		b.Events = []Event{{ID: "root", Kind: "tool-call", AgentID: "main", SourceLength: 2, Timestamp: start, Data: json.RawMessage(`{"tool":"Read"}`)}, {ID: "helper-call", Kind: "tool-call", AgentID: "helper", SourceLength: 2, Timestamp: start, Data: json.RawMessage(`{"tool":"Read"}`)}, {ID: "helper-result", Kind: "tool-result", AgentID: "helper", SourceLength: 2, Timestamp: start.Add(2 * time.Second), Data: json.RawMessage(`{"error":false}`)}}
		if i == 1 {
			b.Events[2].Data = json.RawMessage(`{"error":true}`)
		}
		if i == 2 {
			b.Events[2].Data = json.RawMessage(`{}`)
		}
		for n := range b.Events {
			b.Events[n].ID = fmt.Sprintf("role-%d-%s", i, b.Events[n].ID)
		}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		if i < 6 {
			if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: fmt.Sprint("name-", i), AgentName: &AgentNamePatch{ID: "helper", Name: fmt.Sprintf("Reviewer #%d", i)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 1})
	if err != nil || page.TotalRoles != 2 || page.TotalAppearances != 7 || len(page.Roles) != 1 {
		t.Fatal(page, err)
	}
	r := page.Roles[0]
	if r.Name != "Reviewer" || r.Appearances != 6 || r.Sessions != 6 || r.Machines != 2 || r.WithErrors != 1 || r.WithoutReportedErrors != 4 || r.Unknown != 1 || r.DurationObserved != 6 || r.MeanObservedMS == nil || math.Abs(*r.MeanObservedMS-2000) > 1 || r.KnownCost != nil || r.WithEstimate != 0 {
		t.Fatal(r)
	}
	next, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 1, Cursor: page.NextCursor})
	if err != nil || next.Roles[0].Name != "Unnamed helper identity" || next.NextCursor != "" {
		t.Fatal(next, err)
	}
	if _, err := s.PatchMetadata(ctx, "role-session-0", MetadataPatch{Revision: 1, OperationID: "renamed", AgentName: &AgentNamePatch{ID: "helper", Name: "Different role"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("renaming retained stale role cursor", err)
	}
}

func TestBehaviorRoleUsesOnlyPublishedPartialAttribution(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Session.TokensIn = 100
	b.Usage = []UsageObservation{{ID: "usage", AgentID: "helper", Model: "model", Kind: "delta", TokensIn: 100}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeAccounting(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUsage(ctx, b.Session.ID, "", 10)
	if err != nil || len(u) != 1 {
		t.Fatal(u, err)
	}
	price := ObservationPrice{ObservationID: u[0].ID, AgentID: "helper", Model: "model", Cost: 0.6, RecordedTokens: 100, PricedTokens: 60, UnpricedTokens: 40}
	if err := s.StageObservationPrices(ctx, "role-price", []ObservationPrice{price}); err != nil {
		t.Fatal(err)
	}
	before, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
	if err != nil || len(before.Roles) != 1 || before.Roles[0].KnownCost != nil || before.Roles[0].RecordedTokens != 100 || before.Roles[0].UnpricedTokens != 100 || before.Roles[0].Unknown != 1 {
		t.Fatal(before, err)
	}
	session, err := s.GetSession(ctx, b.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"id": "role-price", "sessionId": session.ID, "generation": session.Generation, "projectionRevision": session.ProjectionRevision, "indexedOffset": 2, "catalogId": "fixture", "context": "fixture", "attributionVersion": 1, "observations": 1, "estimate": map[string]any{"cost": 0.6, "recordedTokens": 100, "pricedTokens": 60, "unpricedTokens": 40, "unattributedTokens": 0}})
	if err := s.SaveProjectionEstimate(ctx, "role-price", session.ID, session.Generation, session.ProjectionRevision, 2, payload); err != nil {
		t.Fatal(err)
	}
	after, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	r := after.Roles[0]
	if r.KnownCost == nil || math.Abs(*r.KnownCost-0.6) > 1e-9 || r.RecordedTokens != 100 || r.PricedTokens != 60 || r.UnpricedTokens != 40 || r.WithEstimate != 1 || r.MeanObservedMS != nil {
		t.Fatal(r)
	}
}

func TestBehaviorRolesMissingAttributionBeforePricing(t *testing.T) {
	for _, fixture := range []struct{ model, kind string }{{"", "delta"}, {"model", "incomplete-attribution"}, {"model", "message-without-id"}, {"model", "message-partial"}} {
		t.Run(fixture.model+"/"+fixture.kind, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			b.Session.TokensIn = 100
			b.Usage = []UsageObservation{{ID: "missing-attribution", AgentID: "helper", Model: fixture.model, Kind: fixture.kind, TokensIn: 100}}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			page, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
			if err != nil || len(page.Roles) != 1 {
				t.Fatal(page, err)
			}
			r := page.Roles[0]
			if r.RecordedTokens != 100 || r.UnpricedTokens != 100 || r.UnattributedTokens != 100 || r.PricedTokens != 0 || r.KnownCost != nil {
				t.Fatal(r)
			}
		})
	}
}

func TestBehaviorRoleEarlyIdentityFilterKeepsChildUsage(t *testing.T) {
	for _, fixture := range []struct {
		name, provider, agent, parent, nativeAgent string
		want                                       bool
	}{
		{"root", "codex", "main", "", "", false},
		{"unknown", "claude", "", "parent", "", false},
		{"helper", "claude", "helper", "", "", true},
		{"child-thread", "codex", "main", "parent", "", true},
		{"child-source", "claude", "main", "", "main", true},
		{"other-native-agent", "claude", "main", "", "other", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			s := openTestStore(t, Options{})
			ctx := context.Background()
			src := testSource()
			src.Provider = fixture.provider
			ingest(t, s, src, 0, "x\n")
			b := batch(src, 0, 2)
			b.Session.ParentThreadID = fixture.parent
			b.Session.NativeAgentID = fixture.nativeAgent
			b.Usage = []UsageObservation{{ID: "usage", AgentID: fixture.agent, Model: "model", Kind: "delta", TokensIn: 100}}
			if err := s.CommitIndex(ctx, b); err != nil {
				t.Fatal(err)
			}
			page, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if !fixture.want {
				if page.TotalAppearances != 0 || len(page.Roles) != 0 {
					t.Fatal("excluded identity retained", page)
				}
				return
			}
			if page.TotalAppearances != 1 || len(page.Roles) != 1 || page.Roles[0].RecordedTokens != 100 || page.Roles[0].Unknown != 1 {
				t.Fatal("child or helper usage lost", page)
			}
		})
	}
}

func TestBehaviorRolesRetainChildMainIdentity(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		src := testSource()
		src.SourceID = fmt.Sprint("child-role-", i)
		src.Provider = "codex"
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprint("child-role-session-", i)
		if i == 1 {
			b.Session.ParentThreadID = "parent-thread"
		}
		b.Events = []Event{{ID: fmt.Sprint("child-role-event-", i), Kind: "message", AgentID: "main", SourceLength: 2}}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.BehaviorRoles(ctx, SessionQuery{Limit: 20})
	if err != nil || page.TotalAppearances != 1 || len(page.Roles) != 1 || page.Roles[0].ExampleSessionID != "child-role-session-1" {
		t.Fatal(page, err)
	}
}
