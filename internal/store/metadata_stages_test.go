package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMetadataQueueOverflowRetainsOperationForRetry(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 1)
	s.writeMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, priorityWriterLimit)
	for i := 0; i < priorityWriterLimit; i++ {
		go func() { done <- s.writeMu.LockPriorityContext(ctx) }()
	}
	waitWriterQueue(t, &s.writeMu, 0, priorityWriterLimit)
	yes := true
	p := MetadataPatch{Archived: &yes, OperationID: "overflow-operation"}
	_, err := s.PatchMetadata(context.Background(), "s-000000", p)
	if !errors.Is(err, ErrWriterQueueFull) {
		t.Fatal("organization did not use bounded priority admission", err)
	}
	m, err := s.GetMetadata(context.Background(), "s-000000")
	if err != nil || m.Revision != 0 || m.Archived {
		t.Fatal("overflow changed metadata", m, err)
	}
	var audits int
	if err := s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id='overflow-operation'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
	cancel()
	for i := 0; i < priorityWriterLimit; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued writer did not cancel")
		}
	}
	s.writeMu.Unlock()
	for i := 0; i < 2; i++ {
		m, err = s.PatchMetadata(context.Background(), "s-000000", p)
		if err != nil || m.Revision != 1 || !m.Archived {
			t.Fatal("same intent could not retry", m, err)
		}
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id='overflow-operation'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("retry duplicated audit", audits, err)
	}
}

func TestMetadataStagesPreserveCommitAndReplay(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 1)
	var stages []string
	yes := true
	note := "private note must not appear in diagnostic stages"
	patch := MetadataPatch{Archived: &yes, Note: &note, OperationID: "stage-operation"}
	report := func(stage string) { stages = append(stages, stage) }
	m, err := s.PatchMetadataWithStage(context.Background(), "s-000000", patch, report)
	if err != nil || m.Revision != 1 || !m.Archived {
		t.Fatal(m, err)
	}
	want := []string{"organization-validate", "organization-writer-admission", "organization-begin-transaction", "organization-load-metadata", "organization-update-catalog", "organization-write-journal", "organization-commit"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatal(stages)
	}
	stages = nil
	replay, err := s.PatchMetadataWithStage(context.Background(), "s-000000", patch, report)
	if err != nil || !reflect.DeepEqual(m, replay) {
		t.Fatal(replay, err)
	}
	if !reflect.DeepEqual(stages, want[:4]) {
		t.Fatal("replay attempted another write", stages)
	}
	var audits int
	if err = s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id='stage-operation'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal(audits, err)
	}
}

func TestMetadataStageIdentifiesWriterAdmissionWithoutChangingCancellation(t *testing.T) {
	s := openTestStore(t, Options{ExternalCheckpointOwner: true})
	seedAnalyticsCatalog(t, s, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.writeMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.writeMu.Unlock) }
	reached := make(chan struct{}, 1)
	done := make(chan struct{})
	result := make(chan error, 1)
	yes := true
	go func() {
		defer close(done)
		_, err := s.PatchMetadataWithStage(ctx, "s-000000", MetadataPatch{Archived: &yes, OperationID: "waiting-operation"}, func(stage string) {
			if stage == "organization-writer-admission" {
				reached <- struct{}{}
			}
		})
		result <- err
	}()
	defer func() { unlock(); <-done }()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("writer-admission stage missing")
	}
	select {
	case <-done:
		t.Fatal("diagnostics bypassed held writer")
	default:
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled organization request waited for the active writer")
	}
	unlock()
	m, err := s.GetMetadata(context.Background(), "s-000000")
	if err != nil || m.Revision != 0 || m.Archived {
		t.Fatal("canceled request mutated organization", m, err)
	}
	var audits int
	if err := s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id='waiting-operation'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal("canceled admission wrote an audit", audits, err)
	}
	patch := MetadataPatch{Archived: &yes, OperationID: "waiting-operation"}
	for attempt := 0; attempt < 2; attempt++ {
		m, err := s.PatchMetadata(context.Background(), "s-000000", patch)
		if err != nil || m.Revision != 1 || !m.Archived {
			t.Fatal("same operation could not safely retry", m, err)
		}
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM organization_audit WHERE operation_id='waiting-operation'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("retry duplicated audit", audits, err)
	}
}
