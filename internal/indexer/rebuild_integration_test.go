package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func rebuildFixture(t *testing.T, count int, options store.Options) (*store.Store, protocol.Source, []byte) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.SetupIndexing(ctx); err != nil {
		t.Fatal(err)
	}
	src := protocol.Source{SourceID: "source", MachineID: "machine", Generation: "g1", GenerationSequence: 1, Provider: "claude", NativeID: "native"}
	var raw bytes.Buffer
	for i := 0; i < count; i++ {
		fmt.Fprintf(&raw, "{\"type\":\"assistant\",\"timestamp\":\"2026-09-04T01:00:00Z\",\"uuid\":\"content-%d\",\"message\":{\"id\":\"msg-%d\",\"model\":\"new-model\",\"content\":[{\"type\":\"text\",\"text\":\"newneedle\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n", i, i)
	}
	data := raw.Bytes()
	src.Size = int64(len(data))
	h := sha256.Sum256(data)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Length: src.Size, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	old := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, ToOffset: src.Size, ParserState: json.RawMessage(`{"version":"1"}`), Session: store.Session{ID: "preserved-session-key", Title: "old title", Project: "source project"}, Events: []store.Event{{ID: "old-event", AgentID: "old-agent", Kind: "assistant", SourceLength: src.Size, Text: "oldneedle", Data: json.RawMessage(`{}`), Timestamp: time.Now()}}, Usage: []store.UsageObservation{{ID: "old-usage", AgentID: "old-agent", Model: "old-model", TokensIn: 999, Kind: "message-final"}}}
	if err = s.CommitIndex(ctx, old); err != nil {
		t.Fatal(err)
	}
	return s, src, append([]byte(nil), data...)
}

func TestRunClearsVersionWarningAfterPublication(t *testing.T) {
	s, _, _ := rebuildFixture(t, 1, store.Options{})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.BeginRebuild(ctx, "preserved-session-key", "status-rebuild", parser.Version); err != nil {
		t.Fatal(err)
	}
	x := Indexer{Store: s}
	done := make(chan error, 1)
	go func() { done <- x.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("worker shutdown: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := x.Status()
		if status.Records > 0 && status.State == "caught-up" && status.VersionIssues == 0 && status.VersionExample == nil && status.Rebuilds.Pending == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publication left stale status", x.Status())
}

func TestRebuildStoragePressurePausesWithoutReplacingHistory(t *testing.T) {
	free := int64(2 << 30)
	s, _, _ := rebuildFixture(t, 1, store.Options{ReserveBytes: 16 << 20, AvailableBytes: func(string) (int64, error) { return free, nil }})
	defer s.Close()
	ctx := context.Background()
	x := Indexer{Store: s}
	job, err := s.BeginRebuild(ctx, "preserved-session-key", "storage-op", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	free = (16 << 20) + 1024
	if _, err = x.RebuildOnce(ctx, job.Revision); !errors.Is(err, store.ErrCapacity) {
		t.Fatal("rebuild ignored disk reserve", err)
	}
	row, err := s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.TokensIn != 999 {
		t.Fatal("disk pressure lost prior history", row, err)
	}
	state, err := s.ProjectionRevision(ctx, job.Revision)
	if err != nil || state.IndexedOffset != 0 || state.State != "building" {
		t.Fatal(state, err)
	}
	free = 2 << 30
	if _, err = x.RebuildOnce(ctx, job.Revision); err != nil {
		t.Fatal(err)
	}
	row, err = s.GetSession(ctx, row.ID)
	if err != nil || row.ProjectionRevision != job.Revision || row.TokensIn != 1 {
		t.Fatal("rebuild did not resume", row, err)
	}
}

func TestRebuildPublicationCrashHelper(t *testing.T) {
	mode := os.Getenv("AMC_TEST_REBUILD_CRASH")
	if mode == "" {
		return
	}
	s, err := store.Open(os.Getenv("AMC_TEST_REBUILD_DIRECTORY"), store.Options{BeforeCommit: func() error {
		if mode == "before" {
			os.Exit(86)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PublishRebuild(context.Background(), os.Getenv("AMC_TEST_REBUILD_REVISION")); err != nil {
		t.Fatal(err)
	}
	os.Exit(87) // simulate committed publication whose response never reached owner
}

func TestRebuildPublicationSurvivesActualProcessExit(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			hold := false
			s, _, _ := rebuildFixture(t, 1, store.Options{BeforeCommit: func() error {
				if hold {
					return errors.New("hold ready")
				}
				return nil
			}})
			defer s.Close()
			x := Indexer{Store: s}
			job, err := s.BeginRebuild(ctx, "preserved-session-key", "process-op", parser.Version)
			if err != nil {
				t.Fatal(err)
			}
			hold = true
			if _, err = x.RebuildOnce(ctx, job.Revision); err == nil {
				t.Fatal("ready state was not held")
			}
			snapshot := t.TempDir() + "/backup"
			if _, err = s.Backup(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir() + "/process-ledger"
			restored, err := store.RestoreBackup(ctx, snapshot, dir, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			restored.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(executable, "-test.run=^TestRebuildPublicationCrashHelper$")
			child.Env = append(os.Environ(), "AMC_TEST_REBUILD_CRASH="+mode, "AMC_TEST_REBUILD_DIRECTORY="+dir, "AMC_TEST_REBUILD_REVISION="+job.Revision)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			want := 87
			if mode == "before" {
				want = 86
			}
			if !errors.As(err, &exit) || exit.ExitCode() != want {
				t.Fatalf("helper exit: %v %s", err, output)
			}
			reopened, err := store.Open(dir, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			row, err := reopened.GetSession(ctx, "preserved-session-key")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "before" {
				if row.TokensIn != 999 || row.ProjectionRevision != "" {
					t.Fatal("uncommitted projection exposed after crash", row)
				}
			} else {
				if row.TokensIn != 1 || row.ProjectionRevision != job.Revision {
					t.Fatal("committed projection lost after crash", row)
				}
			}
			if err = reopened.PublishRebuild(ctx, job.Revision); err != nil {
				t.Fatal("safe lost-response retry", err)
			}
			row, err = reopened.GetSession(ctx, row.ID)
			if err != nil || row.TokensIn != 1 || row.ProjectionRevision != job.Revision {
				t.Fatal(row, err)
			}
			found, err := reopened.Search(ctx, store.SearchQuery{Text: "oldneedle"})
			if err != nil || len(found.Events) != 0 {
				t.Fatal("retired FTS survived publication", found, err)
			}
		})
	}
}

func appendRebuildRaw(t *testing.T, s *store.Store, src *protocol.Source, data []byte) {
	t.Helper()
	offset := src.Size
	src.Size += int64(len(data))
	h := sha256.Sum256(data)
	if _, err := s.IngestChunk(context.Background(), protocol.Chunk{Source: *src, Offset: offset, Length: int64(len(data)), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildPublicationFailurePreservesOldReadsAndRepricesSameOffset(t *testing.T) {
	crash := false
	s, src, _ := rebuildFixture(t, 1, store.Options{BeforeCommit: func() error {
		if crash {
			return errors.New("injected before publication commit")
		}
		return nil
	}})
	defer s.Close()
	ctx := context.Background()
	x := Indexer{Store: s}
	card := accounting.Catalog{ID: "rate-card", Rates: []accounting.Rate{{ID: "old-rate", Model: "old-model", Context: "api", EffectiveFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Source: "fixture", Input: 1}, {ID: "new-rate", Model: "new-model", Context: "api", EffectiveFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Source: "fixture", Input: 1}}}
	if err := accounting.SaveCatalog(ctx, s, card); err != nil {
		t.Fatal(err)
	}
	before, err := accounting.Reprice(ctx, s, "preserved-session-key", card.ID, "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.BeginRebuild(ctx, "preserved-session-key", "crash-op", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	crash = true
	if _, err = x.RebuildOnce(ctx, job.Revision); err == nil {
		t.Fatal("fault injection did not stop publication")
	}
	row, err := s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.TokensIn != 999 || row.ProjectionRevision != "" {
		t.Fatal("partial publication", row, err)
	}
	state, err := s.ProjectionRevision(ctx, job.Revision)
	if err != nil || state.State != "ready" {
		t.Fatal("ready checkpoint lost", state, err)
	}
	crash = false
	if err = s.PublishRebuild(ctx, job.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := accounting.Reprice(ctx, s, row.ID, card.ID, "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID == after.ID || before.IndexedOffset != after.IndexedOffset || after.Generation != src.Generation || after.ProjectionRevision != job.Revision || after.Estimate.RecordedTokens != 1 {
		t.Fatal("repricing identity crossed revisions", before, after)
	}
	history, err := s.EstimateHistory(ctx, row.ID, "", 100)
	if err != nil || len(history) != 2 {
		t.Fatal("historical estimate lost", history, err)
	}
	if err = s.SaveProjectionEstimate(ctx, "stale", row.ID, src.Generation, "", src.Size, json.RawMessage(`{}`)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stale pricing CAS accepted", err)
	}
}

func TestRebuildKeepsPartialTailAndNormalIndexingUsesPublishedRevision(t *testing.T) {
	s, src, _ := rebuildFixture(t, 1, store.Options{})
	defer s.Close()
	ctx := context.Background()
	x := Indexer{Store: s}
	boundary := src.Size
	appendRebuildRaw(t, s, &src, []byte(`{"type":"assistant","uuid":"later","message":`))
	job, err := s.BeginRebuild(ctx, "preserved-session-key", "partial-op", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = x.RebuildOnce(ctx, job.Revision); err != nil {
			t.Fatal(err)
		}
	}
	row, err := s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.Completeness != "indexed-source-partial" {
		t.Fatal(row, err)
	}
	st, err := s.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil || st.IndexedOffset != boundary || st.ProjectionRevision != job.Revision {
		t.Fatal("tail checkpoint lost", st, err)
	}
	appendRebuildRaw(t, s, &src, []byte("{\"id\":\"later\",\"model\":\"new-model\",\"content\":\"tail completed\",\"usage\":{\"input_tokens\":7}}}\n"))
	if n, e := x.Once(ctx, src.SourceID, src.Generation); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	row, err = s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.TokensIn != 8 || row.EventCount != 2 || row.ProjectionRevision != job.Revision {
		t.Fatal("normal indexing changed identity or mixed revisions", row, err)
	}
	all, err := s.CatalogTotals(ctx, store.SessionQuery{})
	if err != nil || all.Sessions != 1 || all.TokensIn != 8 {
		t.Fatal("duplicate source session", all, err)
	}
}

func TestRebuildCannotPublishOverNewRawGeneration(t *testing.T) {
	crash := false
	s, src, raw := rebuildFixture(t, 1, store.Options{BeforeCommit: func() error {
		if crash {
			return errors.New("hold ready")
		}
		return nil
	}})
	defer s.Close()
	ctx := context.Background()
	x := Indexer{Store: s}
	job, err := s.BeginRebuild(ctx, "preserved-session-key", "obsolete-op", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	crash = true
	if _, err = x.RebuildOnce(ctx, job.Revision); err == nil {
		t.Fatal("expected held publication")
	}
	crash = false
	src.Generation = "g2"
	src.GenerationSequence = 2
	src.Size = 0
	appendRebuildRaw(t, s, &src, raw)
	if err = s.PublishRebuild(ctx, job.Revision); !errors.Is(err, store.ErrConflict) {
		t.Fatal("old raw generation replaced new", err)
	}
	row, err := s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.TokensIn != 999 {
		t.Fatal("old published projection became unavailable", row, err)
	}
	if _, err = x.Once(ctx, src.SourceID, src.Generation); err != nil {
		t.Fatal(err)
	}
	row, err = s.GetSession(ctx, "preserved-session-key")
	if err != nil || row.Generation != "g2" || row.TokensIn != 1 {
		t.Fatal("new generation did not publish to stable session", row, err)
	}
}

func TestStagedRebuildSurvivesRestartAndSwitchesEveryReadAtomically(t *testing.T) {
	s, src, raw := rebuildFixture(t, 600, store.Options{})
	defer func() { s.Close() }()
	ctx := context.Background()
	x := &Indexer{Store: s}
	archived := true
	meta, err := s.PatchMetadata(ctx, "preserved-session-key", store.MetadataPatch{OperationID: "archive", Archived: &archived})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.BeginRebuild(ctx, "preserved-session-key", "rebuild-op", parser.Version)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.BeginRebuild(ctx, "preserved-session-key", "rebuild-op", parser.Version)
	if err != nil || retry.Revision != job.Revision {
		t.Fatal("idempotent job", retry, err)
	}
	if _, err = s.BeginRebuild(ctx, "preserved-session-key", "competing-op", parser.Version); err == nil {
		t.Fatal("competing rebuild accepted")
	}
	if n, e := x.RebuildOnce(ctx, job.Revision); e != nil || n != 256 {
		t.Fatal(n, e)
	}
	assertVisible := func(tokens int64, model, needle, absent string) {
		t.Helper()
		row, e := s.GetSession(ctx, "preserved-session-key")
		if e != nil || row.TokensIn != tokens || !row.Metadata.Archived {
			t.Fatal("session projection", row, e)
		}
		all, e := s.CatalogTotals(ctx, store.SessionQuery{})
		if e != nil || all.TokensIn != tokens {
			t.Fatal("aggregate leaked staging", all, e)
		}
		models, e := s.SessionModelUsage(ctx, row.ID, "", 100)
		if e != nil || len(models.Models) != 1 || models.Models[0].Model != model {
			t.Fatal("model projection", models, e)
		}
		found, e := s.Search(ctx, store.SearchQuery{Text: needle})
		if e != nil || len(found.Events) == 0 {
			t.Fatal("visible search", found, e)
		}
		found, e = s.Search(ctx, store.SearchQuery{Text: absent})
		if e != nil || len(found.Events) != 0 {
			t.Fatal("staging/retired search leaked", found, e)
		}
	}
	assertVisible(999, "old-model", "oldneedle", "newneedle")
	name, project := "Owner title", ""
	if _, err = s.PatchMetadata(ctx, "preserved-session-key", store.MetadataPatch{OperationID: "owner-edit", Revision: meta.Revision, Name: &name, Project: &project}); err != nil {
		t.Fatal(err)
	}
	// Reopen the same durable directory between staging batches.
	backupDir := t.TempDir() + "/snapshot"
	if _, err = s.Backup(ctx, backupDir); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir() + "/restored"
	restoredStore, restoreErr := store.RestoreBackup(ctx, backupDir, restored, store.Options{})
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	restoredStore.Close()
	s.Close()
	s, err = store.Open(restored, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	x.Store = s
	assertVisible(999, "old-model", "oldneedle", "newneedle")
	for i := 0; i < 10; i++ {
		if _, err = x.RebuildOnce(ctx, job.Revision); err != nil {
			t.Fatal(err)
		}
		state, e := s.ProjectionRevision(ctx, job.Revision)
		if e != nil {
			t.Fatal(e)
		}
		if state.State == "active" {
			break
		}
		if i == 9 {
			t.Fatal("rebuild did not publish")
		}
	}
	assertVisible(600, "new-model", "newneedle", "oldneedle")
	row, _ := s.GetSession(ctx, "preserved-session-key")
	if row.ProjectionRevision != job.Revision || row.Metadata.Name != name || !row.Metadata.ProjectOverride || row.Metadata.Project != "" {
		t.Fatal("organization/revision changed", row)
	}
	events, err := s.ListEvents(ctx, row.ID, 0, 100)
	if err != nil || len(events.Events) != 100 || events.Events[0].ProjectionRevision != job.Revision {
		t.Fatal("event revision", events, err)
	}
	stats, err := s.SessionStats(ctx, row.ID)
	if err != nil || stats.Events != 600 || stats.AgentCount != 1 {
		t.Fatal("agent summary leaked old revision", stats, err)
	}
	stream, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || !bytes.Equal(actual, raw) {
		t.Fatal("raw evidence changed", err)
	}
	if err = s.PublishRebuild(ctx, job.Revision); err != nil {
		t.Fatal("lost publish ACK", err)
	}
	changes, err := s.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	publications := 0
	for _, c := range changes {
		if c.Kind == "projection" {
			publications++
		}
	}
	if publications != 1 {
		t.Fatal("duplicate publication", publications)
	}
}
