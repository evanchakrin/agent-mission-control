package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingProjectDeletionSurvivesVerifiedBackupRestore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, Options{})
	for i := 0; i < 3; i++ {
		src := testSource()
		src.SourceID = fmt.Sprintf("restore-source-%d", i)
		ingest(t, s, src, 0, fmt.Sprintf("backup %02d\n", i))
		b := batch(src, 0, 10)
		b.Session.ID = fmt.Sprintf("restore-session-%d", i)
		if err := s.CommitIndex(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	project, err := s.MutateProject(ctx, ProjectMutation{ID: "restore-project", Name: "Restore", Color: "#123456", OperationID: "restore-create"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = s.PatchMetadata(ctx, fmt.Sprintf("restore-session-%d", i), MetadataPatch{Project: &project.ID, OperationID: fmt.Sprintf("restore-assign-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	request := ProjectMutation{ID: project.ID, Revision: 1, Delete: true, OperationID: "restore-delete", RecoveryEpoch: s.RecoveryEpoch()}
	if _, err = s.QueueProjectDeletion(ctx, request); err != nil {
		t.Fatal(err)
	}
	if job, err := s.AdvanceProjectDeletion(ctx, request.OperationID, 1); err != nil || job.Processed != 1 || job.State != "pending" {
		t.Fatal(job, err)
	}
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	manifest, err := s.Backup(ctx, backup)
	if err != nil || manifest.RawBytes != 30 {
		t.Fatal(manifest, err)
	}
	if _, err = VerifyBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	// Complete the original after the snapshot. Restoring must use the backed-up
	// pending ledger, not borrow this later receipt from the original directory.
	if job, err := s.AdvanceProjectDeletion(ctx, request.OperationID, 100); err != nil || job.State != "complete" {
		t.Fatal(job, err)
	}
	restored, err := RestoreBackup(ctx, backup, filepath.Join(root, "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.RecoveryEpoch() == s.RecoveryEpoch() {
		t.Fatal("restore did not rotate epoch")
	}
	job, err := restored.QueueProjectDeletion(ctx, request)
	if err != nil || job.Processed != 1 || job.State != "pending" {
		t.Fatal("surviving acceptance failed to resume", job, err)
	}
	absent := request
	absent.OperationID = "absent-after-backup"
	if _, err = restored.QueueProjectDeletion(ctx, absent); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("old absent operation accepted", err)
	}
	yes := true
	note := "owner edit after restore"
	if _, err = restored.PatchMetadata(ctx, "restore-session-2", MetadataPatch{Revision: 1, Archived: &yes, Note: &note, OperationID: "post-restore-edit"}); err != nil {
		t.Fatal(err)
	}
	if job, err = restored.AdvanceProjectDeletion(ctx, request.OperationID, 100); err != nil || job.State != "complete" || job.Processed != 3 {
		t.Fatal(job, err)
	}
	if receipt, err := restored.MutateProject(ctx, request); err != nil || receipt != job.Project {
		t.Fatal("restored completion receipt", receipt, err)
	}
	meta, err := restored.GetMetadata(ctx, "restore-session-2")
	if err != nil || !meta.Archived || meta.Note != note || meta.Project != "" || !meta.ProjectOverride || meta.Revision != 3 {
		t.Fatal("post-restore edit lost", meta, err)
	}
	var audits int
	if err = restored.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id LIKE 'project-delete-batch:%'`).Scan(&audits); err != nil || audits != 3 {
		t.Fatal("repeated or missing audits", audits, err)
	}
	if original, err := s.GetMetadata(ctx, "restore-session-2"); err != nil || original.Note != "" || original.Archived {
		t.Fatal("restored writes touched original", original, err)
	}
	// Verify a second backup containing the completed restored ledger and the
	// same complete raw history, rather than only inspecting in-memory state.
	completed := filepath.Join(root, "completed-backup")
	if _, err = restored.Backup(ctx, completed); err != nil {
		t.Fatal(err)
	}
	if verified, err := VerifyBackup(ctx, completed); err != nil || verified.RawBytes != 30 {
		t.Fatal(verified, err)
	}
}

func TestProjectDeletionWorkerCompletesAndStops(t *testing.T) {
	s := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := s.MutateProject(ctx, ProjectMutation{ID: "worker", Name: "Worker", Color: "#123456", OperationID: "create-worker"}); err != nil {
		t.Fatal(err)
	}
	request := ProjectMutation{ID: "worker", Revision: 1, Delete: true, OperationID: "delete-worker", RecoveryEpoch: "old-absent-epoch"}
	if _, err := s.QueueProjectDeletion(ctx, request); !errors.Is(err, ErrHistoryChanged) {
		t.Fatal("restore fence", err)
	}
	request.RecoveryEpoch = s.RecoveryEpoch()
	if _, err := s.QueueProjectDeletion(ctx, request); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.RunProjectDeletions(lifetime, nil) }()
	deadline := time.After(5 * time.Second)
	for {
		job, err := s.ProjectDeletion(ctx, request.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == "complete" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestQueuedProjectDeletionBatchesRestartAndConcurrentEdits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedAnalyticsCatalog(t, s, 4)
	p, err := s.MutateProject(ctx, ProjectMutation{ID: "batch-project", Name: "Batch", Color: "#123456", OperationID: "batch-create"})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	note := "retain this"
	for i := 0; i < 4; i++ {
		_, err = s.PatchMetadata(ctx, fmt.Sprintf("s-%06d", i), MetadataPatch{Project: &p.ID, Archived: &yes, Pinned: &yes, Note: &note, OperationID: fmt.Sprintf("assign-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	request := ProjectMutation{ID: p.ID, Revision: 1, Delete: true, OperationID: "batch-delete", RecoveryEpoch: s.RecoveryEpoch()}
	job, err := s.QueueProjectDeletion(ctx, request)
	if err != nil || job.State != "pending" || job.Processed != 0 {
		t.Fatal(job, err)
	}
	if _, err = s.MutateProject(ctx, request); !errors.Is(err, ErrConflict) {
		t.Fatal("pending job falsely acknowledged", err)
	}
	if _, err = s.PatchMetadata(ctx, "s-000000", MetadataPatch{Project: &p.ID, Revision: 1, OperationID: "reassign-deleted"}); !errors.Is(err, ErrConflict) {
		t.Fatal("tombstone accepted assignment", err)
	}
	if job, err = s.AdvanceProjectDeletion(ctx, request.OperationID, 1); err != nil || job.Processed != 1 || job.State != "pending" {
		t.Fatal(job, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if job, err = s.QueueProjectDeletion(ctx, request); err != nil || job.Processed != 1 {
		t.Fatal("retry lost durable progress", job, err)
	}
	changed := request
	changed.Revision++
	if _, err = s.QueueProjectDeletion(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("operation collision", err)
	}
	other := "another-project"
	if _, err = s.PatchMetadata(ctx, "s-000003", MetadataPatch{Project: &other, Revision: 1, OperationID: "move-away"}); err != nil {
		t.Fatal(err)
	}
	note = "edited while pending"
	if _, err = s.PatchMetadata(ctx, "s-000002", MetadataPatch{Note: &note, Revision: 1, OperationID: "pending-note"}); err != nil {
		t.Fatal(err)
	}
	if job, err = s.AdvanceProjectDeletion(ctx, request.OperationID, 100); err != nil || job.State != "complete" || job.Processed != 3 {
		t.Fatal(job, err)
	}
	if again, err := s.AdvanceProjectDeletion(ctx, request.OperationID, 100); err != nil || again != job {
		t.Fatal("completion not idempotent", again, err)
	}
	if result, err := s.MutateProject(ctx, request); err != nil || result != job.Project {
		t.Fatal("missing final receipt", result, err)
	}
	m, err := s.GetMetadata(ctx, "s-000002")
	if err != nil || m.Project != "" || !m.ProjectOverride || !m.Archived || !m.Pinned || m.Note != note || m.Revision != 3 {
		t.Fatal(m, err)
	}
	m, err = s.GetMetadata(ctx, "s-000003")
	if err != nil || m.Project != other || m.Revision != 2 {
		t.Fatal("concurrent reassignment lost", m, err)
	}
	var audits, sessions int
	if err = s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id LIKE 'project-delete-batch:%'`).Scan(&audits); err != nil || audits != 3 {
		t.Fatal(audits, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 4 {
		t.Fatal(sessions, err)
	}
	if next, err := s.NextProjectDeletion(ctx); err != nil || next != "" {
		t.Fatal(next, err)
	}
}

func TestQueuedProjectDeletionFailureRollsBackBatch(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, Options{})
	seedAnalyticsCatalog(t, s, 2)
	p, err := s.MutateProject(ctx, ProjectMutation{ID: "failure-project", Name: "Failure", Color: "#123456", OperationID: "failure-create"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.PatchMetadata(ctx, fmt.Sprintf("s-%06d", i), MetadataPatch{Project: &p.ID, OperationID: fmt.Sprintf("assign-fail-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	request := ProjectMutation{ID: p.ID, Revision: 1, Delete: true, OperationID: "failure-delete"}
	if _, err = s.QueueProjectDeletion(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_delete_batch BEFORE UPDATE ON session_metadata WHEN NEW.session_id='s-000001' BEGIN SELECT RAISE(ABORT,'fixture failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceProjectDeletion(ctx, request.OperationID, 100); err == nil {
		t.Fatal("injected failure ignored")
	}
	job, err := s.ProjectDeletion(ctx, request.OperationID)
	if err != nil || job.Processed != 0 || job.State != "pending" {
		t.Fatal(job, err)
	}
	m, err := s.GetMetadata(ctx, "s-000000")
	if err != nil || m.Project != p.ID || m.Revision != 1 {
		t.Fatal("partial batch committed", m, err)
	}
	var audits int
	if err = s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id LIKE 'project-delete-batch:%'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_delete_batch`); err != nil {
		t.Fatal(err)
	}
	if job, err = s.AdvanceProjectDeletion(ctx, request.OperationID, 100); err != nil || job.State != "complete" || job.Processed != 2 {
		t.Fatal(job, err)
	}
}
