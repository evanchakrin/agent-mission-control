package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestAgentNamesShareOrganizationRevisionAndSurviveImport(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	src := testSource()
	ingest(t, s, src, 0, "x\n")
	b := batch(src, 0, 2)
	b.Events = []Event{{ID: "event", AgentID: "main", Text: "hello", SourceLength: 2}}
	if err := s.CommitIndex(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	patch := MetadataPatch{OperationID: "name-1", AgentName: &AgentNamePatch{ID: "main", Name: "Research lead"}}
	for i := 0; i < 2; i++ {
		m, err := s.PatchMetadata(ctx, "session-1", patch)
		if err != nil || m.Revision != 1 {
			t.Fatal(m, err)
		}
	}
	page, err := s.SessionAgents(ctx, "session-1", "", 1)
	if err != nil || len(page.Agents) != 1 || page.Agents[0].Name != "Research lead" {
		t.Fatal(page, err)
	}
	stale := patch
	history, err := s.ListEvents(ctx, "session-1", 0, 100)
	if err != nil || len(history.Events) != 1 || history.Events[0].AgentDisplayName != "Research lead" || history.Events[0].AgentID != "main" {
		t.Fatal("history lost current display name or changed recorded identity", history, err)
	}
	stale.OperationID = "stale-name"
	if _, err = s.PatchMetadata(ctx, "session-1", stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale accepted", err)
	}
	yes := true
	cleared := MetadataPatch{Revision: 1, OperationID: "name-clear", Archived: &yes, AgentName: &AgentNamePatch{ID: "main", Name: ""}}
	if _, err = s.PatchMetadata(ctx, "session-1", cleared); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacyMetadata(ctx, map[string]string{"old": "session-1"}, map[string]json.RawMessage{"old": json.RawMessage(`{"agentNames":{"main":"Resurrected","child":"Imported child"}}`)}); err != nil {
		t.Fatal(err)
	}
	var name string
	history, err = s.ListEvents(ctx, "session-1", 0, 100)
	if err != nil || len(history.Events) != 1 || history.Events[0].AgentDisplayName != "" {
		t.Fatal("history resurrected cleared name", history, err)
	}
	if err = s.db.QueryRowContext(ctx, "SELECT name FROM agent_names WHERE session_id=? AND agent_id=?", "session-1", "main").Scan(&name); err != nil || name != "" {
		t.Fatal("cleared name resurrected", name, err)
	}
	if err = s.db.QueryRowContext(ctx, "SELECT name FROM agent_names WHERE session_id=? AND agent_id=?", "session-1", "child").Scan(&name); err != nil || name != "Imported child" {
		t.Fatal("legacy name lost", name, err)
	}
	audit, err := s.OrganizationHistory(ctx, "session-1", 0, 100)
	if err != nil || len(audit.Entries) != 2 {
		t.Fatal(audit, err)
	}
	if audit.Entries[1].Before.AgentName == nil || audit.Entries[1].Before.AgentName.Name != "Research lead" || audit.Entries[1].After.AgentName == nil || audit.Entries[1].After.AgentName.Name != "" || !audit.Entries[1].After.Archived {
		t.Fatal("incomplete agent audit", audit)
	}
	// Lost acknowledgement replay must not restore an older name or revision.
	if _, err = s.PatchMetadata(ctx, "session-1", patch); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMetadata(ctx, "session-1")
	if err != nil || m.Revision != 2 || !m.Archived {
		t.Fatal(m, err)
	}
}
