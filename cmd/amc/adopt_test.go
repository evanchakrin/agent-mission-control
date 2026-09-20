package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/migration"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func TestCollectorAdoptCommandLocksAndResumes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.jsonl")
	data := []byte("imported prefix\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	f := migration.File{Path: path, Size: int64(len(data)), ModifiedAt: info.ModTime(), Raw: true, Provider: "codex", MachineID: "test-machine", SHA256: hex.EncodeToString(hash[:])}
	source := protocol.Source{MachineID: f.MachineID, Provider: f.Provider, Path: f.Path, Size: f.Size, ModifiedAt: f.ModifiedAt, SourceID: parser.ID("legacy-file", f.MachineID, f.Provider, f.Path), Generation: parser.ID("legacy-generation", f.ModifiedAt.Format(time.RFC3339Nano), strconv.FormatInt(f.Size, 10))}
	manifest := migration.Result{Version: 1, CompletedAt: time.Now(), Sources: []protocol.Source{source}, Files: []migration.File{f}}
	manifestPath := filepath.Join(t.TempDir(), "migration-manifest.json")
	writeJSON := func(path string, value any) {
		t.Helper()
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeJSON(manifestPath, manifest)
	owner := ""
	if runtime.GOOS == "windows" {
		owner, err = platform.CurrentOwnerSID()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := platform.Config{Role: platform.Collector, OwnerSID: owner, MachineID: f.MachineID, DataDir: t.TempDir(), HubURL: "http://127.0.0.1:1", Sources: []platform.SourceRoot{{Path: root, Provider: "codex"}}}
	configPath := filepath.Join(t.TempDir(), "collector.json")
	writeJSON(configPath, cfg)
	args := []string{"collector-adopt", "--config", configPath, "--manifest", manifestPath, "--source-id", source.SourceID, "--source-path", path, "--json"}
	lock, err := platform.AcquireInstance(platform.Collector, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	err = run(args)
	lock.Close()
	if err == nil {
		t.Fatal("adoption modified a locked collector")
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, "spool.sqlite")); !os.IsNotExist(err) {
		t.Fatal("locked adoption opened spool", err)
	}
	if err = run(args); err != nil {
		t.Fatal(err)
	}
	// A lost command acknowledgement must be recoverable even if the file has
	// subsequently moved away; the committed adoption is not performed twice.
	if err = os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err = run(args); err != nil {
		t.Fatal("receipt replay failed", err)
	}
	manifest.Sources = append(manifest.Sources, source)
	writeJSON(manifestPath, manifest)
	if _, _, err = adoptionSelection(manifestPath, source.SourceID); err == nil {
		t.Fatal("ambiguous manifest accepted")
	}
	manifest.Sources = manifest.Sources[:1]
	manifest.Files[0].Size++
	writeJSON(manifestPath, manifest)
	if _, _, err = adoptionSelection(manifestPath, source.SourceID); err == nil {
		t.Fatal("inconsistent manifest accepted")
	}
}
