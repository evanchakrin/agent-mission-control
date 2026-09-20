package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/evanchakrin/agent-mission-control/internal/desktop"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestCompleteBackupRestoreAndCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "owner-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	sid := ""
	if runtime.GOOS == "windows" {
		var err error
		sid, err = platform.CurrentOwnerSID()
		if err != nil {
			t.Fatal(err)
		}
	}
	hubConfig := platform.Config{Role: platform.Hub, DataDir: filepath.Join(root, "hub"), OwnerSID: sid, PipeName: "backup-test", ListenAddress: "127.0.0.1:4183"}
	desktopConfig := platform.Config{Role: platform.Desktop, DataDir: filepath.Join(root, "desktop"), OwnerHomeDir: home, OwnerSID: sid, PipeName: "backup-test", ListenAddress: "127.0.0.1:4184"}
	s, err := store.Open(hubConfig.DataDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manager, err := desktop.New(desktop.Options{StateDir: desktopConfig.DataDir, HomeDir: home, CSRFToken: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	raw := []byte("raw evidence\n")
	hash := sha256.Sum256(raw)
	_, err = s.IngestChunk(ctx, protocol.Chunk{Source: protocol.Source{MachineID: "machine", SourceID: "source", Generation: "one", Provider: "claude", Size: int64(len(raw))}, Length: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	hubID, err := s.HubID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CommitIndex(ctx, store.IndexBatch{SourceID: "source", Generation: "one", ToOffset: int64(len(raw)), Session: store.Session{ID: "kept-session", ParentThreadID: "parent"}, Events: []store.Event{{ID: "kept-event", AgentID: "main", Text: "raw evidence", SourceLength: int64(len(raw))}}, Usage: []store.UsageObservation{{ID: "kept-usage", AgentID: "main", Model: "fixture-model", Kind: "message-partial", Timestamp: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), TokensIn: 7, TokensCache: 2, TokensOut: 3}}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	name, note, project := "Preserved session", "Preserved note", "preserved-project"
	projectCreate := store.ProjectMutation{ID: project, Name: "Preserved project", Color: "#60a5fa", OperationID: "project-create", RecoveryEpoch: s.RecoveryEpoch()}
	if _, err = s.MutateProject(ctx, projectCreate); err != nil {
		t.Fatal(err)
	}
	projectRename := store.ProjectMutation{ID: project, Name: "Renamed project", Color: "#123456", Revision: 1, OperationID: "project-rename", RecoveryEpoch: s.RecoveryEpoch()}
	wantProject, err := s.MutateProject(ctx, projectRename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateProject(ctx, store.ProjectMutation{ID: "deleted-project", Name: "Deleted", Color: "#654321", OperationID: "deleted-create"}); err != nil {
		t.Fatal(err)
	}
	projectDelete := store.ProjectMutation{ID: "deleted-project", Revision: 1, Delete: true, OperationID: "project-delete", RecoveryEpoch: s.RecoveryEpoch()}
	wantDeleted, err := s.MutateProject(ctx, projectDelete)
	if err != nil {
		t.Fatal(err)
	}
	wantProjectHistory, err := s.ProjectHistory(ctx, project, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantDeletedHistory, err := s.ProjectHistory(ctx, "deleted-project", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	tags := []string{"history", "kept"}
	patch := store.MetadataPatch{OperationID: "preserved-operation", Archived: &yes, Pinned: &yes, Name: &name, Note: &note, Project: &project, Tags: &tags, AgentName: &store.AgentNamePatch{ID: "main", Name: "Preserved agent"}}
	wantMetadata, err := s.PatchMetadata(ctx, "kept-session", patch)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutLegacyAlias(ctx, "legacy/session-key", "kept-session"); err != nil {
		t.Fatal(err)
	}
	wantAudit, err := s.OrganizationHistory(ctx, "kept-session", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantStats, err := s.SessionStats(ctx, "kept-session")
	if err != nil {
		t.Fatal(err)
	}
	wantRoles, err := s.BehaviorRoles(ctx, store.SessionQuery{Limit: 20})
	if err != nil || len(wantRoles.Roles) != 1 || wantRoles.Roles[0].RecordedTokens != 12 || wantRoles.Roles[0].UnattributedTokens != 12 {
		t.Fatal("fixture did not exercise derived agent usage", wantRoles, err)
	}
	dest := filepath.Join(root, "backup")
	m, err := Create(ctx, s, hubConfig, desktopConfig, dest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Hub.RawBytes != int64(len(raw)) {
		t.Fatal("raw history not captured", m)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Verify(cancelled, dest); !errors.Is(err, context.Canceled) {
		t.Fatal("complete backup verification ignored cancellation", err)
	}
	if _, err := Restore(cancelled, dest, filepath.Join(root, "cancelled-restore")); !errors.Is(err, context.Canceled) {
		t.Fatal("restore ignored cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cancelled-restore")); !os.IsNotExist(err) {
		t.Fatal("cancelled restore created a destination", err)
	}
	// A post-backup operation must not be silently applied to restored history.
	laterProject := store.ProjectMutation{ID: "after-backup", Name: "Later", Color: "#112233", OperationID: "after-backup-create", RecoveryEpoch: s.RecoveryEpoch()}
	if _, err = s.MutateProject(ctx, laterProject); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored")
	if _, err = Restore(ctx, dest, restored); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open(filepath.Join(restored, "hub"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.SetupAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	gotStats, err := r.SessionStats(ctx, "kept-session")
	if err != nil || !reflect.DeepEqual(wantStats, gotStats) {
		t.Fatal("restored session statistics changed", wantStats, gotStats, err)
	}
	gotRoles, err := r.BehaviorRoles(ctx, store.SessionQuery{Limit: 20})
	if err != nil || !reflect.DeepEqual(wantRoles.Roles, gotRoles.Roles) || wantRoles.TotalAppearances != gotRoles.TotalAppearances || wantRoles.TotalRoles != gotRoles.TotalRoles {
		t.Fatal("restored role summaries changed", wantRoles, gotRoles, err)
	}
	restoredID, _ := r.HubID(ctx)
	if restoredID != hubID || r.RecoveryEpoch() == s.RecoveryEpoch() {
		t.Fatal("hub identity or restore epoch incorrect")
	}
	registry, err := r.Projects(ctx, "", 100)
	if err != nil || len(registry.Items) != 1 || registry.Items[0] != wantProject {
		t.Fatal("project registry changed on restore", registry, err)
	}
	if replay, e := r.MutateProject(ctx, projectRename); e != nil || replay != wantProject {
		t.Fatal("project rename receipt lost", replay, e)
	}
	if replay, e := r.MutateProject(ctx, projectDelete); e != nil || replay != wantDeleted {
		t.Fatal("project tombstone receipt lost", replay, e)
	}
	if _, e := r.MutateProject(ctx, laterProject); !errors.Is(e, store.ErrHistoryChanged) {
		t.Fatal("absent post-backup operation was not guarded", e)
	}
	if _, e := r.MutateProject(ctx, store.ProjectMutation{ID: "deleted-project", Name: "Resurrected", Color: "#112233", Revision: 2, OperationID: "resurrect-project", RecoveryEpoch: r.RecoveryEpoch()}); !errors.Is(e, store.ErrConflict) {
		t.Fatal("restored tombstone allowed resurrection", e)
	}
	for _, expected := range []struct {
		id      string
		history store.ProjectAuditPage
	}{{project, wantProjectHistory}, {"deleted-project", wantDeletedHistory}} {
		history, e := r.ProjectHistory(ctx, expected.id, 0, 100)
		if e != nil || !reflect.DeepEqual(history, expected.history) {
			t.Fatal("project audit changed or replay duplicated it", expected.id, history, e)
		}
	}
	session, err := r.GetSession(ctx, "kept-session")
	if err != nil || !reflect.DeepEqual(session.Metadata, wantMetadata) {
		t.Fatal("organization state changed on restore", err)
	}
	alias, err := r.ResolveAlias(ctx, "legacy/session-key")
	if err != nil || alias != "kept-session" {
		t.Fatal("legacy alias lost", err)
	}
	agents, err := r.SessionAgents(ctx, "kept-session", "", 100)
	if err != nil || len(agents.Agents) != 1 || agents.Agents[0].Name != "Preserved agent" {
		t.Fatal("agent name lost", err)
	}
	ack, err := r.PatchMetadata(ctx, "kept-session", patch)
	if err != nil || !reflect.DeepEqual(ack, wantMetadata) {
		t.Fatal("idempotent receipt lost", err)
	}
	gotAudit, err := r.OrganizationHistory(ctx, "kept-session", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(wantAudit)
	gotJSON, _ := json.Marshal(gotAudit)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatal("audit changed or receipt replay duplicated mutation")
	}
	if _, err = Restore(ctx, dest, restored); err == nil {
		t.Fatal("restore overwrote existing destination")
	}
	manifestPath := filepath.Join(dest, "installation-manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifestPath, append(manifestBytes, []byte("\n{\"unexpected\":true}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, dest); err == nil {
		t.Fatal("backup accepted trailing manifest corruption")
	}
	if err = os.WriteFile(manifestPath, append(manifestBytes, bytes.Repeat([]byte(" "), 1<<20)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, dest); err == nil {
		t.Fatal("backup accepted oversized manifest with valid JSON prefix")
	}
	if err = os.WriteFile(filepath.Join(dest, "installation-manifest.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, dest); err == nil {
		t.Fatal("corrupt backup accepted")
	}
}
