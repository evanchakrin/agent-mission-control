package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestProjectDeletionDeadlineRollsBackAndSameOperationCanRetry(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 1)
	ctx := context.Background()
	project, err := s.MutateProject(ctx, ProjectMutation{ID: "deadline-project", Name: "Keep", Color: "#123456", OperationID: "deadline-create"})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	before, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{Project: &project.ID, Archived: &yes, OperationID: "deadline-assign"})
	if err != nil {
		t.Fatal(err)
	}
	// Force cancellable SQL work after the registry update, in this fixture only.
	if _, err = s.db.Exec(`CREATE TRIGGER slow_project_delete AFTER UPDATE ON projects WHEN NEW.deleted=1 BEGIN
	 WITH RECURSIVE work(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM work WHERE n<100000000) SELECT sum(n) FROM work; END;`); err != nil {
		t.Fatal(err)
	}
	remove := ProjectMutation{ID: project.ID, Revision: 1, Delete: true, OperationID: "deadline-delete"}
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = s.MutateProject(deadline, remove)
	cancel()
	if err == nil || deadline.Err() != context.DeadlineExceeded {
		t.Fatal("deadline was not exercised", err, deadline.Err())
	}
	registry, err := s.Projects(ctx, "", 100)
	if err != nil || len(registry.Items) != 1 || registry.Items[0] != project {
		t.Fatal("cancelled deletion changed registry", registry, err)
	}
	after, err := s.GetMetadata(ctx, "s-000000")
	if err != nil || after.Revision != before.Revision || after.Project != project.ID || !after.Archived {
		t.Fatal("cancelled deletion changed membership", after, err)
	}
	var receipts, audits int
	if err = s.db.QueryRow(`SELECT count(*) FROM project_operations WHERE operation_id='deadline-delete'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("cancelled deletion acknowledged", receipts, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE session_id='s-000000'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("cancelled deletion appended audit", audits, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER slow_project_delete`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.MutateProject(ctx, remove); err != nil || !result.Deleted || result.Revision != 2 {
		t.Fatal("same operation could not retry after rollback", result, err)
	}
}

func TestProjectRegistryRevisionsDeletionAndRestart(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	ctx := context.Background()
	create := ProjectMutation{ID: "project-one", Name: "ERP", Color: "#60a5fa", OperationID: "create-project"}
	one, err := s.MutateProject(ctx, create)
	if err != nil || one.Revision != 1 {
		t.Fatal(one, err)
	}
	retry, err := s.MutateProject(ctx, create)
	if err != nil || retry != one {
		t.Fatal(retry, err)
	}
	create.Name = "Other"
	if _, err = s.MutateProject(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	create.OperationID = "stale-create"
	if _, err = s.MutateProject(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	create.Revision = 1
	create.OperationID = "rename-project"
	updated, err := s.MutateProject(ctx, create)
	if err != nil || updated.Revision != 2 {
		t.Fatal(updated, err)
	}
	yes := true
	note := "keep me"
	tags := []string{"important"}
	assignment := MetadataPatch{OperationID: "assign-project", Project: &one.ID, Archived: &yes, Pinned: &yes, Note: &note, Tags: &tags}
	m, err := s.PatchMetadata(ctx, "s-000000", assignment)
	if err != nil {
		t.Fatal(err)
	}
	remove := ProjectMutation{ID: one.ID, Revision: 2, Delete: true, OperationID: "delete-project"}
	// Failure after membership SQL must roll back project, metadata and audit.
	if _, err = s.db.Exec(`CREATE TRIGGER fail_project_audit BEFORE INSERT ON project_audit BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateProject(ctx, remove); err == nil {
		t.Fatal("failed audit committed")
	}
	unchanged, err := s.GetMetadata(ctx, "s-000000")
	if err != nil || unchanged.Revision != m.Revision || unchanged.Project != one.ID {
		t.Fatal(unchanged, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_project_audit"); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.MutateProject(ctx, remove)
	if err != nil || !deleted.Deleted || deleted.Revision != 3 {
		t.Fatal(deleted, err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.MutateProject(ctx, remove); err != nil {
			t.Fatal(err)
		}
	}
	m, err = s.GetMetadata(ctx, "s-000000")
	if err != nil || m.Revision != 2 || m.Project != "" || !m.ProjectOverride || !m.Archived || !m.Pinned || m.Note != note || len(m.Tags) != 1 {
		t.Fatal(m, err)
	}
	audit, err := s.OrganizationHistory(ctx, "s-000000", 0, 100)
	if err != nil || len(audit.Entries) != 2 || audit.Entries[1].Patch.Project == nil || *audit.Entries[1].Patch.Project != "" {
		t.Fatal(audit, err)
	}
	row, err := s.GetSession(ctx, "s-000000")
	if err != nil || row.Metadata.Project != "" {
		t.Fatal(row, err)
	}
	page, err := s.CatalogProjects(ctx, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Items {
		if p.ID == one.ID {
			t.Fatal("deleted project still assigned", p)
		}
	}
	assignment.OperationID = "assign-deleted"
	assignment.Revision = 2
	if _, err = s.PatchMetadata(ctx, "s-000000", assignment); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	// An acknowledged old assignment retry remains a receipt, not a new write.
	assignment.OperationID = "assign-project"
	assignment.Revision = 0
	if _, err = s.PatchMetadata(ctx, "s-000000", assignment); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.ImportLegacyMetadata(ctx, map[string]string{"legacy-project-member": "s-000001"}, map[string]json.RawMessage{"legacy-project-member": json.RawMessage(`{"project":"project-one","archived":true}`)}); err != nil {
		t.Fatal(err)
	}
	imported, err := reopened.GetMetadata(ctx, "s-000001")
	if err != nil || imported.Project != "" || !imported.ProjectOverride || !imported.Archived {
		t.Fatal(imported, err)
	}
	if got, e := reopened.MutateProject(ctx, remove); e != nil || got != deleted {
		t.Fatal(got, e)
	}
	projects, e := reopened.Projects(ctx, "", 100)
	if e != nil || len(projects.Items) != 0 {
		t.Fatal(projects, e)
	}
	create.Revision = 3
	create.OperationID = "resurrect"
	if _, e = reopened.MutateProject(ctx, create); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestProjectRegistryPagesAndValidation(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	for _, id := range []string{"b", "a", "c"} {
		if _, err := s.MutateProject(ctx, ProjectMutation{ID: id, Name: id, Color: "#abcdef", OperationID: id}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Projects(ctx, "", 2)
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "a" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	last, err := s.Projects(ctx, page.NextCursor, 2)
	if err != nil || len(last.Items) != 1 || last.Items[0].ID != "c" || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	for _, color := range []string{"red", "#abc", "#zz0000", "#000000;"} {
		if _, err = s.MutateProject(ctx, ProjectMutation{ID: "bad", Name: "bad", Color: color, OperationID: "bad"}); !errors.Is(err, ErrInvalid) {
			t.Fatal(color, err)
		}
	}
}

func TestProjectRetryAfterRestoreRequiresSurvivingReceipt(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	op := ProjectMutation{ID: "p", Name: "Project", Color: "#123456", OperationID: "before-restore", RecoveryEpoch: s.epoch}
	old, err := s.MutateProject(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	s.epoch = "restored-epoch"
	got, err := s.MutateProject(ctx, op)
	if err != nil || got != old {
		t.Fatal(got, err)
	}
	op.ID = "absent"
	op.OperationID = "missing-receipt"
	if _, err = s.MutateProject(ctx, op); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal(err)
	}
	page, err := s.Projects(ctx, "", 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
}

func TestProjectAssignmentFilterDoesNotConfuseSourceRepositories(t *testing.T) {
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 3)
	ctx := context.Background()
	assigned, empty := "named-project", ""
	if _, err := s.PatchMetadata(ctx, "s-000000", MetadataPatch{Project: &assigned, OperationID: "assign"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchMetadata(ctx, "s-000001", MetadataPatch{Project: &empty, OperationID: "unassign"}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		value string
		want  int64
	}{{assigned, 1}, {empty, 2}, {"p002", 0}} {
		q := SessionQuery{ProjectAssignment: &check.value, Limit: 100}
		page, err := s.ListSessions(ctx, q)
		if err != nil || int64(len(page.Sessions)) != check.want {
			t.Fatal(check, page, err)
		}
		totals, err := s.SessionTotals(ctx, q)
		if err != nil || totals.Sessions != check.want {
			t.Fatal(check, totals, err)
		}
	}
	// Existing repository-path filtering remains unchanged.
	page, err := s.ListSessions(ctx, SessionQuery{Project: "p002"})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatal(page, err)
	}
}

func TestLegacyProjectImportPreservesOwnerEditsAndTombstones(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	legacy := []LegacyProject{{ID: "one", Name: "Original"}, {ID: "two", Name: "Other", Color: "#aabbcc"}}
	if err := s.ImportLegacyProjects(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyProjects(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	page, err := s.Projects(ctx, "", 100)
	if err != nil || len(page.Items) != 2 || page.Items[0].Revision != 1 || page.Items[0].Color != "#8a93a8" {
		t.Fatal(page, err)
	}
	_, err = s.MutateProject(ctx, ProjectMutation{ID: "one", Name: "Owner name", Color: "#123456", Revision: 1, OperationID: "owner-rename"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MutateProject(ctx, ProjectMutation{ID: "two", Revision: 1, Delete: true, OperationID: "owner-delete"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacyProjects(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	page, err = s.Projects(ctx, "", 100)
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "Owner name" || page.Items[0].Color != "#123456" || page.Items[0].Revision != 2 {
		t.Fatal(page, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM project_audit").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	audit, e := s.ProjectHistory(ctx, "one", 0, 1)
	if e != nil || len(audit.Items) != 1 || audit.Items[0].After.Name != "Original" || audit.Next != 1 {
		t.Fatal(audit, e)
	}
	audit, e = s.ProjectHistory(ctx, "one", audit.Next, 1)
	if e != nil || len(audit.Items) != 1 || audit.Items[0].Before.Name != "Original" || audit.Items[0].After.Name != "Owner name" || audit.Next != 0 {
		t.Fatal(audit, e)
	}
	audit, e = s.ProjectHistory(ctx, "two", 1, 100)
	if e != nil || len(audit.Items) != 1 || !audit.Items[0].After.Deleted {
		t.Fatal(audit, e)
	}
	if err = s.ImportLegacyProjects(ctx, []LegacyProject{{ID: "new", Name: "Valid"}, {ID: "bad", Name: "Bad", Color: "red"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	page, err = s.Projects(ctx, "", 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("partial failed import", page, err)
	}
}
