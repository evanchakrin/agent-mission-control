package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitMutationFailureDoesNotClaimUnchangedRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	for _, mode := range []string{"hook-refusal", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = f.repo
				hideCommand(cmd)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fixture git: %v %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			run("-c", "init.templateDir=", "init", "-b", "fixture")
			run("config", "user.name", "AMC Fixture")
			run("config", "user.email", "fixture@example.invalid")
			run("config", "commit.gpgSign", "false")
			hooks := t.TempDir()
			run("config", "core.hooksPath", hooks)
			run("add", "--", "CLAUDE.md")
			run("commit", "-m", "initial fixture")
			originalHead := run("rev-parse", "HEAD")
			if err := os.WriteFile(f.file, []byte("Changed disposable guidance\n"), 0600); err != nil {
				t.Fatal(err)
			}
			hook := "#!/bin/sh\nexit 1\n"
			if mode == "deadline" {
				hook = "#!/bin/sh\nsleep 30\nexit 0\n"
			}
			if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0700); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if mode == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
			}
			started := time.Now()
			out, err := f.m.gitMutation(ctx, "git-commit", Directive{ID: "fixture", Title: "Fixture"}, DirectiveTarget{Path: f.file})
			if err != nil {
				t.Fatal(err)
			}
			result := out.(map[string]any)
			if result["done"] != false || result["uncertain"] != true {
				t.Fatalf("failed child claimed no effects: %+v", result)
			}
			if mode == "deadline" && time.Since(started) > 8*time.Second {
				t.Fatal("Git operation exceeded shared caller budget and bounded shutdown")
			}
			if run("rev-parse", "HEAD") != originalHead {
				t.Fatal("failed pre-commit created a commit")
			}
			if run("diff", "--cached", "--name-only") != "CLAUDE.md" {
				t.Fatal("fixture did not demonstrate partial index mutation")
			}
			var status string
			if err := f.m.db.QueryRow("SELECT status FROM audit WHERE kind='directive-git-commit' ORDER BY id DESC LIMIT 1").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "uncertain" {
				t.Fatalf("misleading audit status: %s", status)
			}
		})
	}
}
