package desktop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/secrets"
)

func TestSecretsLocalBoundedReadAndRedaction(t *testing.T) {
	f := newFixture(t)
	key := "AKIAQ7MZ2KP9R4NT6VW8" // deliberately constructed, nonfunctional fixture
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(f.repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	paths := []string{write("config.txt", "setting\n"+key+"\n"), write("docs/readme.txt", key), write("blob.bin", "a\x00b"), write("big.txt", strings.Repeat("x", secrets.MaxBytes+1)), filepath.Join(f.repo, "missing.txt"), filepath.Join(f.home, "outside.txt")}
	files := make([]UnsavedPath, len(paths))
	for i, p := range paths {
		files[i].Path = p
	}
	result, err := f.m.CheckSecrets(context.Background(), files)
	if err != nil || len(result.Files) != len(paths) {
		t.Fatal("scan failed", err)
	}
	want := []string{"scanned", "skipped-noise", "binary", "too-large", "missing", "outside-approved-roots"}
	for i, row := range result.Files {
		if row.State != want[i] || row.RequestedPath != paths[i] {
			t.Fatalf("file %d: state %s want %s", i, row.State, want[i])
		}
	}
	if len(result.Files[0].Findings) != 1 || result.Files[0].Findings[0].Line != 2 {
		t.Fatal("expected redacted finding")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), key) {
		t.Fatal("response exposed full fixture credential")
	}
	after, _ := os.ReadFile(paths[0])
	if string(after) != "setting\n"+key+"\n" {
		t.Fatal("scan mutated source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := f.m.CheckSecrets(ctx, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stopped.Files {
		if row.State != "unknown" || len(row.Findings) != 0 {
			t.Fatal("canceled check returned findings")
		}
	}
	if _, err := f.m.CheckSecrets(context.Background(), make([]UnsavedPath, 26)); err == nil {
		t.Fatal("file bound missing")
	}
}

func TestSecretsIgnoreAndMachineGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	f := newFixture(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent"))
	cmd := exec.Command("git", "-c", "init.templateDir=", "init", "-b", "fixture")
	cmd.Dir = f.repo
	hideCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal("disposable Git init failed", err)
	}
	cmd = exec.Command("git", "config", "core.excludesFile", filepath.Join(f.repo, "absent-ignore"))
	cmd.Dir = f.repo
	hideCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal("disposable ignore config failed", err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored.txt", "visible.txt"} {
		if err := os.WriteFile(filepath.Join(f.repo, name), []byte("AKIAQ7MZ2KP9R4NT6VW8"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files := []UnsavedPath{{Path: "ignored.txt", WorkingDirectory: f.repo}, {Path: "visible.txt", WorkingDirectory: f.repo}}
	result, err := f.m.CheckSecrets(context.Background(), files)
	if err != nil || len(result.Files) != 2 || result.Files[0].State != "ignored" || result.Files[1].State != "scanned" || len(result.Files[1].Findings) != 1 {
		for i, row := range result.Files {
			t.Logf("file %d state=%s problem=%s findings=%d", i, row.State, row.Problem, len(row.Findings))
		}
		t.Fatal("ignore evidence not established", err)
	}
	body := map[string]any{"machineId": "local", "files": files}
	if code, _ := request(t, f.m, "POST", "/api/v2/local/secrets", body); code != 409 {
		t.Fatal("unbound scan accepted", code)
	}
	f.m.opts.LocalMachineID = "local"
	body["machineId"] = "remote"
	if code, _ := request(t, f.m, "POST", "/api/v2/local/secrets", body); code != 403 {
		t.Fatal("remote scan accepted", code)
	}
	body["machineId"] = "local"
	f.m.secretsMu.Lock()
	code, _ := request(t, f.m, "POST", "/api/v2/local/secrets", body)
	f.m.secretsMu.Unlock()
	if code != 429 {
		t.Fatal("concurrent scan accepted", code)
	}
	if code, _ := request(t, f.m, "POST", "/api/v2/local/secrets", body); code != 200 {
		t.Fatal("bound scan failed", code)
	}
}
