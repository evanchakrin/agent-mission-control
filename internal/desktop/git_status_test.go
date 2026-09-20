package desktop

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitStatusKeepsUnavailableObservationsUnknown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	f := newFixture(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	cmd := exec.Command("git", "init", "--initial-branch=fixture", f.repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %s %v", out, err)
	}
	d := Directive{ID: "rule", Targets: []DirectiveTarget{{Path: f.file, Label: "Fixture"}}}
	if err := f.m.save("directives", []Directive{d}, "fixture"); err != nil {
		t.Fatal(err)
	}
	result := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "git-status", "id": d.ID, "path": f.file})
	states := result["states"].([]any)
	if len(states) != 1 {
		t.Fatal("unbounded targeted probe")
	}
	state := states[0].(map[string]any)
	if state["isRepo"] != true || state["fileDirty"] != true || state["ignored"] != false || state["branch"] != "fixture" {
		t.Fatalf("known observations missing: %+v", state)
	}
	if state["committed"] != nil || state["ahead"] != nil {
		t.Fatalf("unborn HEAD was treated as known: %+v", state)
	}
	probes := state["probeErrors"].(map[string]any)
	if probes["committed"] == nil || probes["ahead"] == nil {
		t.Fatalf("missing probe explanations: %+v", state)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state = f.m.gitStatus(ctx, d, d.Targets[0])
	if state["isRepo"] != nil || state["fileDirty"] != nil || state["error"] == nil {
		t.Fatalf("cancelled probe fabricated clean state: %+v", state)
	}
	if code, _ := request(t, f.m, "POST", "/api/directives", map[string]any{"op": "git-status", "id": d.ID, "path": filepath.Join(f.repo, "other.md")}); code != 400 {
		t.Fatalf("unregistered target accepted: %d", code)
	}
}
