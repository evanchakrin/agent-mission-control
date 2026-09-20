package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Filtering must still use the complete catalog even when the materialized
// behavior rows omit fields used only by the WHERE clause.
func TestBehaviorFlowsFilterBeforeNarrowProjection(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		src := testSource()
		src.SourceID = fmt.Sprint("scoped-flow-", i)
		src.MachineID = fmt.Sprint("machine-", i)
		src.Provider = []string{"claude", "codex"}[i]
		ingest(t, s, src, 0, "x\n")
		b := batch(src, 0, 2)
		b.Session.ID = fmt.Sprint("scoped-session-", i)
		b.Session.Project = fmt.Sprint("project-", i)
		b.Session.Title = []string{"distinctivealpha", "distinctivebeta"}[i]
		b.Events = []Event{{ID: fmt.Sprint("scoped-event-", i), Kind: "tool-call", AgentID: "helper", SourceLength: 2, Data: json.RawMessage(`{"tool":"Read"}`)}}
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			yes := true
			if _, err := s.PatchMetadata(ctx, b.Session.ID, MetadataPatch{OperationID: "scope-archive", Archived: &yes}); err != nil {
				t.Fatal(err)
			}
		}
	}
	yes := true
	for _, q := range []SessionQuery{{MachineID: "machine-0"}, {Provider: "claude"}, {Project: "project-0"}, {Text: "distinctivealpha"}, {Archived: &yes}} {
		q.Limit = 20
		patterns, err := s.BehaviorPatterns(ctx, q)
		if err != nil || patterns.TotalSessions != 1 || len(patterns.Patterns) != 1 || patterns.Patterns[0].ExampleSessionID != "scoped-session-0" {
			t.Fatal(q, patterns, err)
		}
		roles, err := s.BehaviorRoles(ctx, q)
		if err != nil || roles.TotalAppearances != 1 || len(roles.Roles) != 1 || roles.Roles[0].ExampleSessionID != "scoped-session-0" {
			t.Fatal(q, roles, err)
		}
	}
}
