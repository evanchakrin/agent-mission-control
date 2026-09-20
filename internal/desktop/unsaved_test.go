package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUnsavedLocalEvidenceAndReadOnlyBoundaries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	f := newFixture(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent"))
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.repo
		hideCommand(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
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
	run("-c", "init.templateDir=", "init", "-b", "fixture")
	run("config", "user.name", "AMC Fixture")
	run("config", "user.email", "fixture@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "core.hooksPath", t.TempDir())
	run("config", "core.excludesFile", filepath.Join(t.TempDir(), "absent-ignore"))
	write(".gitignore", "ignored/\nignored.txt\n")
	run("add", "--", "CLAUDE.md", ".gitignore")
	run("commit", "-m", "fixture base")
	run("switch", "-c", "other")
	write("branch-only.txt", "saved on another branch")
	run("add", "--", "branch-only.txt")
	run("commit", "-m", "other branch evidence")
	run("switch", "fixture")
	paths := []string{
		write("new file.txt", "new"),
		f.file,
		write("branch-only.txt", "new copy of historical path"),
		write("ignored.txt", "ignored"),
		write("nested/ leading name.txt", "leading space"),
		filepath.Join(f.repo, "missing.txt"),
		filepath.Join(f.home, "outside.txt"),
		write("literal[1].txt", "literal pathspec"),
		write("ignored/nested.txt", "ignored directory"),
	}
	head := run("rev-parse", "HEAD")
	indexPath := filepath.Join(f.repo, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := f.m.CheckUnsaved(context.Background(), paths)
	t.Logf("nine-file bounded local check=%s", time.Since(started))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"untracked-no-reachable-history", "tracked", "in-history", "ignored", "untracked-no-reachable-history", "missing", "outside-approved-roots", "untracked-no-reachable-history", "ignored"}
	if len(result.Files) != len(want) {
		t.Fatal(result)
	}
	for i, file := range result.Files {
		if file.State != want[i] {
			rel, _ := filepath.Rel(f.repo, paths[i])
			status, se := readOnlyGit(context.Background(), f.repo, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--", filepath.ToSlash(rel))
			ignored, ie := readOnlyGit(context.Background(), f.repo, "check-ignore", "--quiet", "--", filepath.ToSlash(rel))
			t.Logf("fixture status=%q error=%v ignore=%q error=%v", status, se, ignored, ie)
			t.Errorf("%s: %+v want %s", paths[i], file, want[i])
		}
	}
	if run("rev-parse", "HEAD") != head {
		t.Fatal("read-only check changed HEAD")
	}
	contextual, err := f.m.CheckUnsavedFiles(context.Background(), []UnsavedPath{{Path: filepath.Base(f.file), WorkingDirectory: f.repo}, {Path: "new file.txt", WorkingDirectory: f.repo}})
	if err != nil || len(contextual.Files) != 2 || contextual.Files[0].State != "tracked" || contextual.Files[1].State != "untracked-no-reachable-history" {
		t.Fatal("relative file Git evidence differs from absolute evidence", contextual, err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !reflect.DeepEqual(indexBefore, indexAfter) {
		t.Fatal("read-only check changed index", err)
	}
	// Environment overrides cannot redirect evidence into another Git directory.
	t.Setenv("GIT_DIR", filepath.Join(f.home, "not-a-repository"))
	redirected, err := f.m.CheckUnsaved(context.Background(), paths[:1])
	if err != nil || redirected.Files[0].State != want[0] {
		t.Fatal(redirected, err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".git", "shallow"), []byte(head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shallow, err := f.m.CheckUnsaved(context.Background(), paths[:1])
	if err != nil || shallow.Files[0].State != "unknown" || !strings.Contains(shallow.Files[0].Problem, "shallow") {
		t.Fatal("shallow history claimed complete absence", shallow, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := f.m.CheckUnsaved(ctx, paths[:1])
	if err != nil || stopped.Files[0].State != "unknown" || stopped.Files[0].Problem == "" {
		t.Fatal(stopped, err)
	}
}

func TestUnsavedRequestGateAndBounds(t *testing.T) {
	f := newFixture(t)
	f.m.opts.LocalMachineID = "fixture-machine"
	for _, paths := range [][]string{nil, make([]string, 26), {"relative.txt"}, {f.file + ":stream"}} {
		if code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", map[string]any{"machineId": "fixture-machine", "paths": paths}); code != 400 {
			t.Fatal("unbounded/invalid request", code)
		}
	}
	if code, _ := request(t, f.m, "GET", "/api/v2/local/unsaved", nil); code != 405 {
		t.Fatal(code)
	}
	f.m.unsavedMu.Lock()
	code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", map[string]any{"machineId": "fixture-machine", "paths": []string{f.file}})
	f.m.unsavedMu.Unlock()
	if code != 429 {
		t.Fatal("concurrent expensive check was not rejected", code)
	}
	for _, mode := range []string{"no-csrf", "remote", "cross-origin"} {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4173/api/v2/local/unsaved", strings.NewReader(`{"paths":[]}`))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-MC-CSRF", f.opts.CSRFToken)
		if mode == "no-csrf" {
			r.Header.Del("X-MC-CSRF")
		}
		if mode == "remote" {
			r.RemoteAddr = "192.0.2.5:12345"
		}
		if mode == "cross-origin" {
			r.Header.Set("Origin", "https://example.invalid")
		}
		w := httptest.NewRecorder()
		f.m.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(mode, w.Code)
		}
	}
}

func TestGitCappedOutputRecordsTruncation(t *testing.T) {
	b := &cappedBuffer{remaining: 3}
	if n, err := b.Write([]byte("abcd")); n != 4 || err != nil || !b.truncated || b.String() != "abc" {
		t.Fatal(b, n, err)
	}
}

func TestUnsavedPorcelainPathIdentity(t *testing.T) {
	for _, tc := range []struct{ raw, path, want string }{
		{"?? wanted.txt\x00!! ignored/\x00", "wanted.txt", "untracked"},
		{"!! wanted.txt\x00!! ignored/\x00", "wanted.txt", "ignored"},
		{"!! ignored/\x00", "ignored/wanted.txt", ""},
		{"R  renamed.txt\x00?? wanted.txt\x00", "wanted.txt", ""},
		{"R  renamed.txt\x00old.txt\x00?? wanted.txt\x00", "wanted.txt", "untracked"},
		{" M wanted.txt\x00", "wanted.txt", "tracked"},
		{"?? wanted.txt", "wanted.txt", ""},
		{"?? wanted.txt\x00?? wanted.txt\x00", "wanted.txt", ""},
		{"XX wanted.txt\x00", "wanted.txt", ""},
	} {
		if got := unsavedPathStatus(tc.raw, tc.path); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestUnsavedExplicitMachineBinding(t *testing.T) {
	f := newFixture(t)
	identity := mustRequest(t, f.m, "GET", "/api/v2/local/identity", nil)
	if identity["state"] != "unconfigured" || identity["machineId"] != "" {
		t.Fatal(identity)
	}
	if code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", map[string]any{"machineId": "remote", "paths": []string{f.file}}); code != 409 {
		t.Fatal("unbound desktop accepted transcript inspection", code)
	}
	f.m.opts.LocalMachineID = "local-fixture"
	identity = mustRequest(t, f.m, "GET", "/api/v2/local/identity", nil)
	if identity["state"] != "configured" || identity["machineId"] != "local-fixture" {
		t.Fatal(identity)
	}
	for _, id := range []string{"", "remote"} {
		if code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", map[string]any{"machineId": id, "paths": []string{f.file}}); code != 403 {
			t.Fatal("foreign/unspecified machine accepted", code)
		}
	}
	result := mustRequest(t, f.m, "POST", "/api/v2/local/unsaved", map[string]any{"machineId": "local-fixture", "paths": []string{filepath.Join(f.home, "not-approved.txt")}})
	if result["machineId"] != "local-fixture" || result["files"].([]any)[0].(map[string]any)["state"] != "outside-approved-roots" {
		t.Fatal(result)
	}
}
