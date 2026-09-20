package migration

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func fixture(t *testing.T, root, relative, body string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func options(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	return Options{LegacyStateDir: legacy, Destination: filepath.Join(root, "candidate"), ReserveBytes: 1, AvailableBytes: func(string) (int64, error) { return 1 << 40, nil }}
}

func TestMachineLabelsSurviveShadowMigrationBackupAndRestore(t *testing.T) {
	o := options(t)
	ctx := context.Background()
	body := `{"machineId":"legacy-home","sessions":{},"machineNames":{"erp":"ERP workstation","missing":"Retained without transcript"}}`
	source := fixture(t, o.LegacyStateDir, "state.json", body)
	plan, err := Preflight(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Run(ctx, plan, store.Options{}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(o.Destination, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[string]string{"erp": "ERP workstation", "missing": "Retained without transcript"} {
		label, e := s.MachineLabel(ctx, id)
		if e != nil || label.DisplayName != want || label.Revision != 1 {
			t.Fatal(id, label, e)
		}
	}
	cleared, err := s.MutateMachineLabel(ctx, store.MachineLabelMutation{MachineID: "erp", Revision: 1, OperationID: "clear-after-import", RecoveryEpoch: s.RecoveryEpoch()})
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(o.Destination), "backup")
	if _, err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(filepath.Dir(o.Destination), "restored")
	restored, err := store.RestoreBackup(ctx, backup, restoredDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	label, err := restored.MachineLabel(ctx, "erp")
	if err != nil || label != cleared {
		t.Fatal("clear tombstone lost", label, err)
	}
	label, err = restored.MachineLabel(ctx, "missing")
	if err != nil || label.DisplayName != "Retained without transcript" || label.Revision != 1 {
		t.Fatal("unmatched label lost", label, err)
	}
	for _, path := range []string{source, filepath.Join(restoredDir, "legacy-assets", "state.json")} {
		got, e := os.ReadFile(path)
		if e != nil || string(got) != body {
			t.Fatal("legacy evidence changed", path, e)
		}
	}
}

func TestLegacyEconomicsSurvivesMigrationBackupAndRestoreWithoutRewriting(t *testing.T) {
	o := options(t)
	ctx := context.Background()
	fixture(t, o.LegacyStateDir, "state.json", `{"machineId":"legacy-home","sessions":{}}`)
	// Repeated records and malformed/partial evidence must not disappear merely
	// because the newer accounting implementation cannot certify their totals.
	body := "{\"at\":123,\"totalUsd\":42,\"topTierShare\":0.9}\r\n{\"at\":123,\"totalUsd\":42,\"topTierShare\":0.9}\r\nmalformed legacy record\n{\"partial\":"
	source := fixture(t, o.LegacyStateDir, "econ-history.jsonl", body)
	plan, err := Preflight(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, plan, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, file := range result.Files {
		if file.Relative == "econ-history.jsonl" {
			if file.Raw {
				t.Fatal("legacy economics misclassified as provider transcript")
			}
			digest = file.SHA256
		}
	}
	if digest == "" {
		t.Fatal("economics evidence missing from migration manifest")
	}
	s, err := store.Open(o.Destination, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup := filepath.Join(filepath.Dir(o.Destination), "backup")
	manifest, err := s.Backup(ctx, backup)
	if err != nil || manifest.Version != 2 || manifest.LegacyAssetsSHA256 == "" {
		t.Fatal(manifest, err)
	}
	restoredDir := filepath.Join(filepath.Dir(o.Destination), "restored")
	restored, err := store.RestoreBackup(ctx, backup, restoredDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, path := range []string{source, filepath.Join(o.Destination, "legacy-assets", "econ-history.jsonl"), filepath.Join(restoredDir, "legacy-assets", "econ-history.jsonl")} {
		actual, e := os.ReadFile(path)
		if e != nil || string(actual) != body {
			t.Fatalf("legacy evidence rewritten at %s: %v", path, e)
		}
	}
	oldManifest, e := os.ReadFile(filepath.Join(o.Destination, "migration-manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	newManifest, e := os.ReadFile(filepath.Join(restoredDir, "migration-manifest.json"))
	if e != nil || string(oldManifest) != string(newManifest) {
		t.Fatal("migration provenance changed on restore", e)
	}
	page, err := restored.EconomicsHistory(ctx, "", 100)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("legacy estimates silently promoted to verified measurements", page, err)
	}
}

func TestShadowMigrationPreservesRawMetadataAndLosslessAssets(t *testing.T) {
	o := options(t)
	ctx := context.Background()
	state := `{"machineId":"local-id","projects":[{"id":"p","name":"ERP","color":"#60a5fa"}],"tags":[{"id":"t"}],"sessions":{"R:erp:claude:main":{"archived":true,"name":"renamed","note":"keep","tags":["t"],"projectId":"p"},"R:erp:claude:missing":{"pinned":true},"L:local-id:claude:local":{"archived":true,"projectId":null}}}`
	fixture(t, o.LegacyStateDir, "state.json", state)
	raw := strings.Repeat("evidence", (1<<20)/8+100) + "\n"
	fixture(t, o.LegacyStateDir, "archive/erp/claude/project/main.jsonl", raw)
	fixture(t, o.LegacyStateDir, "relay/missing.json", `{"id":"relay:erp:project/missing.jsonl","machine":"erp","meta":{"session":"missing","title":"Missing raw"},"result":{"agents":[{"id":"main","model":"unknown","inTokens":100,"outTokens":17}]}}`)
	asset := `{"playbooks":[{"name":"sensitive configuration","body":"preserve exactly"}]}`
	fixture(t, o.LegacyStateDir, "playbooks.json", asset)
	fixture(t, o.LegacyStateDir, "audit.jsonl", "{\"operation\":\"archived\"}\n")
	local := filepath.Join(filepath.Dir(o.LegacyStateDir), "local")
	fixture(t, local, "project/local.jsonl", "{\"type\":\"user\",\"message\":{\"content\":\"hi\"}}\n")
	o.LocalRoots = []Root{{Path: local, Provider: "claude"}}
	p, err := Preflight(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if p.RequiredFreeBytes < 2*p.RawBytes+p.AssetBytes {
		t.Fatal("preflight underbudgets raw and indexes")
	}
	r, err := Run(ctx, p, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.RawSources != 2 || r.CacheOnlySessions != 1 || r.MetadataEntries != 3 {
		t.Fatalf("migration counts %+v", r)
	}
	s, err := store.Open(o.Destination, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	projects, err := s.Projects(ctx, "", 100)
	if err != nil || len(projects.Items) != 1 || projects.Items[0].ID != "p" || projects.Items[0].Name != "ERP" || projects.Items[0].Color != "#60a5fa" {
		t.Fatal("lost project registry", projects, err)
	}
	compat, err := s.LegacyManifest(ctx, "erp")
	if err != nil || compat["claude/project/main.jsonl"] != int64(len(raw)) {
		t.Fatalf("migrated raw missing from v1manifest %+v %v", compat, err)
	}
	m, err := s.GetMetadata(ctx, r.Aliases["R:erp:claude:main"])
	if err != nil || !m.Archived || m.Name != "renamed" || m.Note != "keep" || len(m.Tags) != 1 || m.Project != "p" || !m.ProjectOverride {
		t.Fatalf("lost metadata %+v %v", m, err)
	}
	unassigned, e := s.GetMetadata(ctx, r.Aliases["L:local-id:claude:local"])
	if e != nil || unassigned.Project != "" || !unassigned.ProjectOverride {
		t.Fatal("lost explicit unassignment", unassigned, e)
	}
	cache, err := s.GetSession(ctx, r.Aliases["R:erp:claude:missing"])
	if err != nil || cache.Completeness != "legacy-cache-only" || cache.TokensOut != 17 || cache.CostEstimate != nil {
		t.Fatalf("cache falsely complete %+v %v", cache, err)
	}
	for _, src := range r.Sources {
		if src.MachineID != "erp" {
			continue
		}
		stream, err := s.OpenSource(ctx, src.SourceID, src.Generation, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(stream)
		stream.Close()
		if err != nil || string(got) != raw {
			t.Fatal("multi-chunk raw changed", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(o.Destination, "legacy-assets", "playbooks.json"))
	if err != nil || string(got) != asset {
		t.Fatal("asset changed", err)
	}
	got, err = os.ReadFile(filepath.Join(o.LegacyStateDir, "state.json"))
	if err != nil || string(got) != state {
		t.Fatal("source state modified", err)
	}
	manifest, err := os.ReadFile(filepath.Join(o.Destination, "migration-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err = json.Unmarshal(manifest, &decoded); err != nil || len(decoded.Files) == 0 {
		t.Fatal("manifest missing", err)
	}
}

func TestPreflightRefusesInsufficientSpaceOverlapAndOverwrite(t *testing.T) {
	o := options(t)
	fixture(t, o.LegacyStateDir, "state.json", `{"sessions":{}}`)
	o.AvailableBytes = func(string) (int64, error) { return 100, nil }
	if _, err := Preflight(context.Background(), o); err == nil {
		t.Fatal("insufficient capacity accepted")
	}
	if _, err := os.Stat(o.Destination); !os.IsNotExist(err) {
		t.Fatal("preflight mutated destination")
	}
	o.AvailableBytes = func(string) (int64, error) { return 1 << 40, nil }
	o.Destination = filepath.Join(o.LegacyStateDir, "nested")
	if _, err := Preflight(context.Background(), o); err == nil {
		t.Fatal("overlap accepted")
	}
	o.Destination = o.LegacyStateDir
	if _, err := Preflight(context.Background(), o); err == nil {
		t.Fatal("overwrite accepted")
	}
}

func TestChangedSourceAbortsWithoutCompletionManifest(t *testing.T) {
	o := options(t)
	file := fixture(t, o.LegacyStateDir, "state.json", `{"sessions":{}}`)
	p, err := Preflight(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte(`{"sessions":{"changed":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), p, store.Options{}); err == nil {
		t.Fatal("changed snapshot accepted")
	}
	if _, err = os.Stat(filepath.Join(o.Destination, "migration-manifest.json")); !os.IsNotExist(err) {
		t.Fatal("failed import published completion manifest")
	}
	got, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(got), "changed") {
		t.Fatal("import altered legacy input", err)
	}
}
