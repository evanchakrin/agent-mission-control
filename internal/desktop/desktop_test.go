package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	m                *Manager
	opts             Options
	home, repo, file string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	repo := filepath.Join(base, "project")
	for _, p := range []string{home, repo, filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(repo, "CLAUDE.md")
	if err := os.WriteFile(file, []byte("# Existing project guidance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{StateDir: filepath.Join(base, "owner-state"), HomeDir: home, Roots: []string{repo}, CSRFToken: "desktop-test-token"}
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return fixture{m, opts, home, repo, file}
}
func request(t *testing.T, m *Manager, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:4173"+path, &buf)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Origin", "http://127.0.0.1:4173")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-MC-CSRF", m.opts.CSRFToken)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	return w.Code, out
}
func mustRequest(t *testing.T, m *Manager, method, path string, body any) map[string]any {
	t.Helper()
	code, out := request(t, m, method, path, body)
	if code != 200 {
		t.Fatalf("%s %s: %d %+v", method, path, code, out)
	}
	return out
}
func TestBrainSaveChecksVersionSnapshotsAndPersists(t *testing.T) {
	f := newFixture(t)
	id := idFor(f.file)
	get := mustRequest(t, f.m, "GET", "/api/brain/file?id="+url.QueryEscape(id), nil)
	mustRequest(t, f.m, "POST", "/api/brain/file", map[string]any{"id": id, "content": "# Updated\n", "expectedHash": get["expectedHash"]})
	if code, _ := request(t, f.m, "POST", "/api/brain/file", map[string]any{"id": id, "content": "stale", "expectedHash": get["expectedHash"]}); code != 409 {
		t.Fatalf("stale update got %d", code)
	}
	history := mustRequest(t, f.m, "GET", "/api/brain/history?id="+url.QueryEscape(id), nil)["history"].([]any)
	if len(history) != 1 {
		t.Fatalf("snapshots: %d", len(history))
	}
	stamp := history[0].(map[string]any)["stamp"].(string)
	snap := mustRequest(t, f.m, "GET", "/api/brain/snapshot?id="+url.QueryEscape(id)+"&stamp="+stamp, nil)
	if snap["content"] != "# Existing project guidance\n" {
		t.Fatalf("wrong snapshot: %+v", snap)
	}
	f.m.Close()
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	history = mustRequest(t, again, "GET", "/api/brain/history?id="+url.QueryEscape(id), nil)["history"].([]any)
	if len(history) != 1 {
		t.Fatal("history did not survive restart")
	}
}
func TestLocalGateRejectsNetworkCrossSiteAndMissingCSRF(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct{ name, addr, origin, csrf string }{{"remote", "100.102.85.46:1", "http://127.0.0.1:4173", f.opts.CSRFToken}, {"crosssite", "127.0.0.1:1", "https://attacker.example", f.opts.CSRFToken}, {"csrf", "127.0.0.1:1", "http://127.0.0.1:4173", ""}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:4173/api/brain/file", strings.NewReader(`{}`))
			r.RemoteAddr = test.addr
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-MC-CSRF", test.csrf)
			w := httptest.NewRecorder()
			f.m.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("got %d", w.Code)
			}
		})
	}
}
func TestTargetsDoNotAuthorizeRemoteOrArbitraryFiles(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(filepath.Dir(f.repo), "unapproved.txt")
	os.WriteFile(outside, []byte("keep"), 0600)
	if code, _ := request(t, f.m, "POST", "/api/brain/file", map[string]any{"id": idFor(outside), "content": "overwrite", "expectedHash": hash([]byte("keep"))}); code != 404 {
		t.Fatalf("arbitrary target got %d", code)
	}
	out := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Safe", "body": "Rule", "targets": []string{idFor(outside)}})
	results := out["results"].([]any)
	if results[0].(map[string]any)["status"] != "unknown-target" {
		t.Fatal("unapproved target accepted")
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "keep" {
		t.Fatal("unapproved file changed")
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": "add-root", "path": `\\remote\share\repo`}); code != 400 {
		t.Fatalf("remote root got %d", code)
	}
}
func TestHardLinksAndSymlinkParentsAreRejected(t *testing.T) {
	f := newFixture(t)
	alias := filepath.Join(f.repo, "AGENTS.md")
	if err := os.Link(f.file, alias); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.m, "GET", "/api/brain/file?id="+url.QueryEscape(idFor(f.file)), nil); code != 404 {
		t.Fatalf("hardlink got %d", code)
	}
	t.Run("symlink-parent", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(f.home, "linked-project")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := f.m.validateRoot(link); err == nil {
			t.Fatal("symlink root accepted")
		}
	})
}
func TestDirectivePlantRemeasureAndRetirePreserveOtherText(t *testing.T) {
	f := newFixture(t)
	out := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Measured rule", "topic": "model-tiering", "body": "Old measured facts", "targets": []string{idFor(f.file)}})
	items := out["items"].([]any)
	id := items[0].(map[string]any)["id"].(string)
	f.m.opts.Remeasure = func(context.Context) (Measurement, error) {
		return Measurement{Body: "New measured facts", Sessions: 20, Subs: 300}, nil
	}
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "remeasure", "id": id})
	content, _ := os.ReadFile(f.file)
	if !bytes.Contains(content, []byte("New measured facts")) || bytes.Contains(content, []byte("Old measured facts")) {
		t.Fatalf("remeasure failed: %s", content)
	}
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "retire", "id": id})
	content, _ = os.ReadFile(f.file)
	if !bytes.Contains(content, []byte("# Existing project guidance")) || bytes.Contains(content, []byte("mission-control:directive:")) {
		t.Fatalf("retire damaged file: %s", content)
	}
}
func TestPlaybooksTriageAndAuditSurviveRestart(t *testing.T) {
	f := newFixture(t)
	mustRequest(t, f.m, "POST", "/api/playbooks", map[string]any{"op": "save", "name": "Plan", "body": "Steps", "kind": "custom"})
	mustRequest(t, f.m, "POST", "/api/triage", map[string]any{"key": "finding:test", "status": "resolved", "note": "done"})
	f.m.Close()
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(mustRequest(t, again, "GET", "/api/playbooks", nil)["items"].([]any)) != 1 {
		t.Fatal("playbook lost")
	}
	if len(mustRequest(t, again, "GET", "/api/triage", nil)["triage"].(map[string]any)) != 1 {
		t.Fatal("triage lost")
	}
	if len(mustRequest(t, again, "GET", "/api/audit", nil)["entries"].([]any)) < 2 {
		t.Fatal("audit lost")
	}
}
func TestInterruptedFileWriteIsReconciled(t *testing.T) {
	f := newFixture(t)
	before, _ := os.ReadFile(f.file)
	after := []byte("completed before process stopped")
	_, err := f.m.db.Exec("INSERT INTO file_operations(id,path,before_hash,after_hash,status,at)VALUES(?,?,?,?,?,?)", "test-crash", f.file, hash(before), hash(after), "pending", now())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f.file, after, 0600)
	f.m.Close()
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var status string
	if err = again.db.QueryRow("SELECT status FROM file_operations WHERE id='test-crash'").Scan(&status); err != nil || status != "applied" {
		t.Fatalf("recovery state: %s %v", status, err)
	}
}
func TestLegacyOwnerImportPreservesRecords(t *testing.T) {
	f := newFixture(t)
	legacy := t.TempDir()
	os.WriteFile(filepath.Join(legacy, "playbooks.json"), []byte(`{"items":[{"id":"pb_old","name":"Old plan","body":"preserved","kind":"custom","createdAt":1,"updatedAt":1}]}`), 0600)
	os.WriteFile(filepath.Join(legacy, "triage.json"), []byte(`{"old-finding":{"status":"resolved"}}`), 0600)
	os.WriteFile(filepath.Join(legacy, "audit.jsonl"), []byte("{\"at\":1,\"kind\":\"brain-save\",\"path\":\"old path\",\"custom\":\"preserved\"}\n"), 0600)
	counts, err := f.m.ImportLegacy(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if counts["playbooks"] != 1 || counts["triage"] != 1 || counts["audit"] != 1 {
		t.Fatalf("import counts: %+v", counts)
	}
	items := mustRequest(t, f.m, "GET", "/api/playbooks", nil)["items"].([]any)
	if items[0].(map[string]any)["id"] != "pb_old" {
		t.Fatal("stable playbook ID changed")
	}
	if counts, err = f.m.ImportLegacy(legacy); err != nil || counts["audit"] != 0 || counts["playbooks"] != 0 {
		t.Fatalf("repeat migration must resume idempotently: %+v %v", counts, err)
	}
	os.WriteFile(filepath.Join(legacy, "audit.jsonl"), []byte("{\"at\":1,\"kind\":\"changed\"}\n"), 0600)
	if _, err = f.m.ImportLegacy(legacy); err == nil {
		t.Fatal("changed already-imported audit was accepted")
	}
}

func TestGitCommitOnlyStagesGuidanceAndDoesNotPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	// Keep both fixture Git and the broker's Git child away from the real
	// owner's global config/ignore files. This changes only this test process.
	gitConfigHome := t.TempDir()
	gitConfigFile := filepath.Join(gitConfigHome, "gitconfig")
	if err := os.WriteFile(gitConfigFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", gitConfigHome)
	t.Setenv("XDG_CONFIG_HOME", gitConfigHome)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfigFile)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newFixture(t)
	fixtureGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.repo
		hideCommand(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	fixtureGit("-c", "init.templateDir=", "init")
	fixtureGit("config", "user.name", "AMC Test")
	fixtureGit("config", "user.email", "amc-test@example.invalid")
	fixtureGit("config", "commit.gpgSign", "false")
	fixtureGit("add", "CLAUDE.md")
	fixtureGit("commit", "-m", "fixture")
	// The only remote is a disposable local directory, never the user's remote.
	remote := filepath.Join(t.TempDir(), "remote.git")
	fixtureGit("-c", "init.templateDir=", "init", "--bare", remote)
	fixtureGit("remote", "add", "origin", remote)
	branch := strings.TrimSpace(fixtureGit("branch", "--show-current"))
	fixtureGit("push", "--set-upstream", "origin", branch)
	initialHead := strings.TrimSpace(fixtureGit("rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(f.repo, "unrelated.txt"), []byte("user staged work"), 0600)
	fixtureGit("add", "unrelated.txt")
	out := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "title": "Commit just guidance", "body": "Keep unrelated work", "targets": []string{idFor(f.file)}})
	id := out["items"].([]any)[0].(map[string]any)["id"].(string)
	for _, op := range []string{"git-commit", "git-push"} {
		status, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": op, "id": id, "path": f.file, "expectedHash": strings.Repeat("0", 64)})
		if status != 409 {
			t.Fatalf("stale %s was not rejected: %d", op, status)
		}
	}
	registry := mustRequest(t, f.m, "GET", "/api/directives?limit=10", nil)
	stateHash := registry["items"].([]any)[0].(map[string]any)["stateHash"].(string)
	out = mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "git-commit", "id": id, "path": f.file, "expectedHash": stateHash})
	if out["done"] != true {
		t.Fatalf("commit failed: %+v", out)
	}
	staged := fixtureGit("diff", "--cached", "--name-only")
	if strings.TrimSpace(staged) != "unrelated.txt" {
		t.Fatalf("unrelated staged file was touched: %q", staged)
	}
	changed := fixtureGit("show", "--format=", "--name-only", "HEAD")
	if strings.TrimSpace(changed) != "CLAUDE.md" {
		t.Fatalf("commit touched extra files: %q", changed)
	}
	if !strings.Contains(fmt.Sprint(out["note"]), "Nothing has been sent") {
		t.Fatal("commit implied a push")
	}
	remoteHead := strings.TrimSpace(fixtureGit("--git-dir="+remote, "rev-parse", "refs/heads/"+branch))
	if remoteHead != initialHead {
		t.Fatal("commit unexpectedly changed the remote")
	}
	out = mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "git-push", "id": id, "path": f.file, "expectedHash": stateHash})
	if out["done"] != true {
		t.Fatalf("explicit local-remote push failed: %+v", out)
	}
	if strings.TrimSpace(fixtureGit("--git-dir="+remote, "rev-parse", "refs/heads/"+branch)) != strings.TrimSpace(fixtureGit("rev-parse", "HEAD")) {
		t.Fatal("explicit push did not publish the committed guidance")
	}
	if strings.TrimSpace(fixtureGit("diff", "--cached", "--name-only")) != "unrelated.txt" {
		t.Fatal("push modified unrelated staged work")
	}
}
