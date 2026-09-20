package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

type cappedBuffer struct {
	bytes.Buffer
	remaining int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n > b.remaining {
		b.truncated = true
		p = p[:b.remaining]
	}
	_, _ = b.Buffer.Write(p)
	b.remaining -= len(p)
	return n, nil
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutput(ctx, dir, false, args...)
}

// Read-only repository evidence needs byte-exact NUL-delimited paths and must
// not inherit Git directory overrides or invoke a configured filesystem monitor.
func readOnlyGit(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutput(ctx, dir, true, args...)
}

var errGitIgnoreNoMatch = errors.New("Git explicitly reported no ignored paths")

func gitOutput(ctx context.Context, dir string, readOnly bool, args ...string) (string, error) {
	ignoreProbe := readOnly && len(args) > 0 && args[0] == "check-ignore"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if readOnly {
		prefix := []string{"--no-pager", "--no-replace-objects", "--no-lazy-fetch", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}
		// check-ignore takes literal filenames, not pathspecs, and explicitly
		// rejects the literal pathspec magic used by status/log/ls-files.
		if len(args) == 0 || args[0] != "check-ignore" {
			prefix = append(prefix, "--literal-pathspecs")
		}
		args = append(prefix, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(strings.ToUpper(item), "=")
		if readOnly && strings.HasPrefix(key, "GIT_") && key != "GIT_CONFIG_NOSYSTEM" && key != "GIT_CONFIG_GLOBAL" && key != "GIT_CONFIG_SYSTEM" {
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GCM_INTERACTIVE=Never")
	if readOnly {
		cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	}
	hideCommand(cmd)
	cmd.WaitDelay = 2 * time.Second
	out := &cappedBuffer{remaining: 1024 * 1024}
	stderr := &cappedBuffer{remaining: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	// A bounded Job Object/process group also removes hook/credential-helper
	// descendants on timeout; killing just git.exe can leave them running.
	worker, err := platform.StartWorker(ctx, platform.WorkerSpec{Command: cmd})
	if err != nil {
		return "", err
	}
	defer worker.Close()
	if err := worker.Wait(); err != nil {
		var exitErr *exec.ExitError
		if ignoreProbe && ctx.Err() == nil && stderr.Len() == 0 && !out.truncated && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", fmt.Errorf("%w: %w", errGitIgnoreNoMatch, err)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("git exceeded the operation deadline and was stopped")
		}
		return "", fmt.Errorf("git: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.truncated {
		return "", errors.New("git output exceeded the bounded response size")
	}
	if readOnly {
		if stderr.Len() > 0 {
			return "", errors.New("git reported diagnostics; repository evidence may be incomplete")
		}
		return out.String(), nil
	}
	return strings.TrimSpace(out.String()), nil
}
func gitRoot(ctx context.Context, file string) (string, error) {
	return git(ctx, filepath.Dir(file), "rev-parse", "--show-toplevel")
}
func (m *Manager) gitStatus(ctx context.Context, d Directive, t DirectiveTarget) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	state := map[string]any{"path": t.Path, "label": t.Label, "isRepo": nil, "branch": nil, "fileDirty": nil, "ignored": nil, "committed": nil, "ahead": nil}
	probeErrors := map[string]string{}
	state["probeErrors"] = probeErrors
	if err := m.validateFile(t.Path); err != nil {
		state["error"] = err.Error()
		return state
	}
	root, err := gitRoot(ctx, t.Path)
	if err != nil {
		state["error"] = err.Error()
		return state
	}
	state["isRepo"] = true
	state["root"] = root
	branch, branchErr := git(ctx, root, "branch", "--show-current")
	if branchErr != nil {
		probeErrors["branch"] = branchErr.Error()
	} else {
		state["branch"] = branch
	}
	dirty, dirtyErr := git(ctx, root, "status", "--porcelain", "--", t.Path)
	if dirtyErr != nil {
		probeErrors["fileDirty"] = dirtyErr.Error()
	} else {
		state["fileDirty"] = dirty != ""
	}
	_, ignoreErr := git(ctx, root, "check-ignore", "--", t.Path)
	var exitErr *exec.ExitError
	if ignoreErr == nil {
		state["ignored"] = true
	} else if errors.As(ignoreErr, &exitErr) && exitErr.ExitCode() == 1 {
		state["ignored"] = false
	} else {
		probeErrors["ignored"] = ignoreErr.Error()
	}
	rel, _ := filepath.Rel(root, t.Path)
	saved, e := git(ctx, root, "show", "HEAD:"+filepath.ToSlash(rel))
	start, _ := markers(d.ID)
	if e != nil {
		probeErrors["committed"] = e.Error()
	} else {
		state["committed"] = strings.Contains(saved, start)
	}
	ahead, e := git(ctx, root, "rev-list", "--count", "@{upstream}..HEAD")
	if e == nil {
		n, parseErr := strconv.Atoi(ahead)
		if parseErr != nil || n < 0 {
			probeErrors["ahead"] = "invalid Git ahead count"
		} else {
			state["ahead"] = n
		}
	} else {
		probeErrors["ahead"] = e.Error()
	}
	return state
}
func (m *Manager) gitMutation(ctx context.Context, op string, d Directive, t DirectiveTarget) (any, error) {
	// One budget covers preflight, hooks, credential helpers and follow-up reads.
	// Individual child commands cannot each consume a fresh 15 seconds.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := m.validateFile(t.Path); err != nil {
		return nil, err
	}
	root, err := gitRoot(ctx, t.Path)
	if err != nil {
		return map[string]any{"ok": true, "done": false, "note": err.Error()}, nil
	}
	if err = m.audit("directive-"+op, t.Path, "pending", map[string]string{"directiveId": d.ID}); err != nil {
		return nil, err
	}
	done := false
	mutationStarted := false
	note := ""
	if op == "git-push" {
		mutationStarted = true
		_, err = git(ctx, root, "push")
		if err == nil {
			done = true
			note = "Sent. Other machines receive it the next time they pull."
		}
	} else {
		branch, e := git(ctx, root, "branch", "--show-current")
		if e != nil {
			err = e
		} else if branch == "" {
			err = errors.New("switch to a branch before committing")
		} else {
			mutationStarted = true
			_, err = git(ctx, root, "add", "--", t.Path)
			if err == nil {
				var changed string
				changed, err = git(ctx, root, "diff", "--cached", "--name-only", "--", t.Path)
				if err == nil && changed == "" {
					note = "Nothing to commit — this file already matches the last commit."
				} else if err == nil {
					_, err = git(ctx, root, "commit", "-m", "AMC standing order: "+clean(d.Title, 72), "-m", "Mission-Control-Directive: "+d.ID, "--", t.Path)
					if err == nil {
						done = true
						sha, _ := git(ctx, root, "rev-parse", "--short", "HEAD")
						note = "Committed as " + sha + " on this machine. Nothing has been sent anywhere yet."
					}
				}
			}
		}
	}
	if err != nil {
		note = err.Error()
	}
	status := "not-applied"
	uncertain := err != nil && mutationStarted
	if done {
		status = "applied"
	} else if uncertain {
		// A failed child can have changed the index, created a commit, or sent
		// data before failing. Never describe that outcome as a proven no-op.
		status = "uncertain"
	}
	if auditErr := m.audit("directive-"+op, t.Path, status, map[string]any{"directiveId": d.ID, "done": done, "uncertain": uncertain, "note": note}); auditErr != nil {
		return nil, auditErr
	}
	return map[string]any{"ok": true, "done": done, "uncertain": uncertain, "note": note}, nil
}
