//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/migration"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestNativeMigrationAdoptionAndCollectorUpload(t *testing.T) {
	binary := os.Getenv("AMC_ADOPTION_BINARY")
	if binary == "" {
		t.Skip("native candidate binary not selected")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	invoke := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmd.WaitDelay = 3 * time.Second
		return cmd.CombinedOutput()
	}
	write := func(path string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "native-history.jsonl")
	prefix := bytes.Repeat([]byte("x"), protocol.MaxChunkBytes+7)
	if err := os.WriteFile(path, prefix, 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := platform.CurrentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	cfg := platform.Config{Role: platform.Collector, OwnerSID: owner, MachineID: "native-adoption-machine", DataDir: t.TempDir(), Token: "isolated-native-adoption-test-token", HubURL: "http://127.0.0.1:1", Sources: []platform.SourceRoot{{Path: root, Provider: "codex"}}}
	configPath := filepath.Join(t.TempDir(), "collector.json")
	write(configPath, cfg)
	dest := filepath.Join(t.TempDir(), "migrated-hub")
	output, err := invoke("migrate", "--config", configPath, "--source", t.TempDir(), "--destination", dest, "--json")
	if err != nil {
		t.Fatalf("native migration failed: %v %s", err, output)
	}
	var result migration.Result
	if err = json.Unmarshal(output, &result); err != nil || len(result.Sources) != 1 {
		t.Fatal("invalid native migration result", err)
	}
	source := result.Sources[0]
	s, err := store.Open(dest, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	yes := true
	id := parser.SessionID(source)
	if _, err = s.PatchMetadata(ctx, id, store.MetadataPatch{OperationID: "native-archive", Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(hub.New(s, cfg.Token, "fixture").IngestionHandler())
	defer server.Close()
	cfg.HubURL = server.URL
	write(configPath, cfg)
	all := append(append([]byte{}, prefix...), []byte("native append\n")...)
	if err = os.WriteFile(path, all, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"collector-adopt", "--config", configPath, "--manifest", filepath.Join(dest, "migration-manifest.json"), "--source-id", source.SourceID, "--source-path", path, "--json"}
	for range 2 {
		output, err = invoke(args...)
		if err != nil {
			t.Fatalf("native adoption failed: %v %s", err, output)
		}
		var ack struct {
			Adopted bool `json:"adopted"`
		}
		if err = json.Unmarshal(output, &ack); err != nil || !ack.Adopted {
			t.Fatal("invalid native adoption acknowledgement", err)
		}
	}
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(processCtx, binary, "collector", "--config", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.WaitDelay = 3 * time.Second
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { stop(); <-done }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, e := s.CurrentSource(ctx, source.SourceID)
		if e == nil && state.DurableOffset == int64(len(all)) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("native collector did not finish adopted upload", ctx.Err())
		case <-ticker.C:
		}
	}
	output, err = invoke(args...)
	if err == nil {
		t.Fatal("native adoption accepted a running collector")
	}
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if e := json.Unmarshal(output, &failure); e != nil || failure.OK || failure.Error == "" {
		t.Fatal("native error was not structured JSON", e)
	}
	sources, err := s.ListSources(ctx, "", 10)
	if err != nil || len(sources) != 1 {
		t.Fatal("native collector duplicated imported source", err)
	}
	raw, err := s.OpenSource(ctx, source.SourceID, source.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(raw)
	raw.Close()
	if err != nil || !bytes.Equal(got, all) {
		t.Fatal("native migrated bytes differ", err)
	}
	session, err := s.GetSession(ctx, id)
	if err != nil || !session.Metadata.Archived {
		t.Fatal("native adoption lost organization", err)
	}
	t.Logf("native migration, adoption replay, collector upload, instance exclusion and archive preservation passed: sources=1 bytes=%d", len(all))
}
