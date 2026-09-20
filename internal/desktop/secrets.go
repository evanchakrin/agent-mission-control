package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/secrets"
)

type SecretFile struct {
	RequestedPath             string            `json:"requestedPath"`
	RequestedWorkingDirectory string            `json:"requestedWorkingDirectory,omitempty"`
	Path                      string            `json:"path"`
	State                     string            `json:"state"`
	Problem                   string            `json:"problem,omitempty"`
	Findings                  []secrets.Finding `json:"findings"`
	Capped                    bool              `json:"capped"`
}
type SecretCheck struct {
	MachineID  string       `json:"machineId"`
	ObservedAt time.Time    `json:"observedAt"`
	Scope      string       `json:"scope"`
	Files      []SecretFile `json:"files"`
}

func (m *Manager) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if err := m.gate(r); err != nil {
		respond(w, nil, err)
		return
	}
	if r.Method != http.MethodPost {
		respond(w, nil, fail(405, "use POST for a bounded local scan"))
		return
	}
	var input struct {
		MachineID string        `json:"machineId"`
		Files     []UnsavedPath `json:"files"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		respond(w, nil, fail(400, "invalid local scan request"))
		return
	}
	if m.opts.LocalMachineID == "" {
		respond(w, nil, fail(409, "configure desktop machineId before scanning transcript paths"))
		return
	}
	if input.MachineID != m.opts.LocalMachineID {
		respond(w, nil, fail(403, "transcript machine does not match this desktop"))
		return
	}
	result, err := m.CheckSecrets(r.Context(), input.Files)
	respond(w, result, err)
}

var secretNoiseName = regexp.MustCompile(`(?i)^(?:package-lock\.json|yarn\.lock|pnpm-lock\.yaml|composer\.lock|Cargo\.lock)$|\.min\.[a-z0-9]+$|\.map$|\.(?:example|sample|template|dist)$|^\.env\.(?:example|sample|template)$|\.(?:test|spec)\.[a-z0-9]+$`)
var secretNoiseDirs = map[string]bool{}

func init() {
	for _, dir := range strings.Fields("node_modules dist build .git coverage vendor .next .nuxt .venv site-packages fixtures __fixtures__ testdata test-fixtures __snapshots__ test tests __tests__ spec __mocks__ docs doc examples example samples") {
		secretNoiseDirs[dir] = true
	}
}
func secretNoise(path string) bool {
	if secretNoiseName.MatchString(filepath.Base(path)) {
		return true
	}
	for _, part := range strings.FieldsFunc(filepath.Dir(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if secretNoiseDirs[part] {
			return true
		}
	}
	return false
}

// CheckSecrets reads at most 25 x 512 KiB, sequentially, with one cooperative
// five-second deadline. No contents or complete matches enter state, logs or
// responses. Unknown/skipped evidence is never a clean result.
func (m *Manager) CheckSecrets(ctx context.Context, files []UnsavedPath) (SecretCheck, error) {
	out := SecretCheck{MachineID: m.opts.LocalMachineID, ObservedAt: time.Now().UTC(), Scope: "approved-local-paths-only", Files: []SecretFile{}}
	if len(files) < 1 || len(files) > 25 {
		return out, fail(400, "scan between one and 25 files")
	}
	for _, f := range files {
		if f.Path == "" || len(f.Path) > 4096 || len(f.WorkingDirectory) > 4096 || strings.IndexFunc(f.Path+f.WorkingDirectory, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return out, fail(400, "invalid recorded file context")
		}
	}
	if !m.secretsMu.TryLock() {
		return out, fail(429, "a local secret scan is already running")
	}
	defer m.secretsMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots, err := m.approvedRoots()
	if err != nil {
		return out, err
	}
	for _, f := range files {
		row := SecretFile{RequestedPath: f.Path, RequestedWorkingDirectory: f.WorkingDirectory, Path: f.Path, State: "unknown", Findings: []secrets.Finding{}}
		if ctx.Err() != nil {
			row.Problem = "scan deadline or cancellation reached"
		} else {
			path, problem := resolveUnsavedPath(f, roots)
			if problem != "" {
				row.State = "unsupported-path"
				row.Problem = problem
			} else {
				row.Path = filepath.Clean(path)
				scanSecretFile(ctx, &row, roots)
			}
		}
		out.Files = append(out.Files, row)
	}
	return out, nil
}

func scanSecretFile(ctx context.Context, row *SecretFile, roots []string) {
	approved := false
	for _, root := range roots {
		if under(root, row.Path) && norm(root) != norm(row.Path) {
			approved = true
			break
		}
	}
	if !approved {
		row.State = "outside-approved-roots"
		return
	}
	if secretNoise(row.Path) {
		row.State = "skipped-noise"
		return
	}
	if noLinks(row.Path) != nil {
		row.Problem = "file is linked or unavailable"
		return
	}
	before, err := os.Lstat(row.Path)
	if os.IsNotExist(err) {
		row.State = "missing"
		return
	}
	if err != nil || !before.Mode().IsRegular() {
		row.Problem = "regular file is unavailable"
		return
	}
	if before.Size() > secrets.MaxBytes {
		row.State = "too-large"
		return
	}
	// Discover .git by metadata only. No repository is a supported loose file;
	// discovery/ignore failures remain unknown, not evidence that ignore is false.
	for dir := filepath.Dir(row.Path); ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		if _, err := os.Lstat(marker); err == nil {
			repoApproved := false
			for _, root := range roots {
				if under(root, dir) {
					repoApproved = true
					break
				}
			}
			if !repoApproved {
				row.Problem = "repository root is outside approved local roots"
				return
			}
			if noLinks(marker) != nil {
				row.Problem = "repository metadata is linked or unavailable"
				return
			}
			rel, _ := filepath.Rel(dir, row.Path)
			_, err = readOnlyGit(ctx, dir, "check-ignore", "--quiet", "--", filepath.ToSlash(rel))
			if err == nil {
				row.State = "ignored"
				return
			}
			if !errors.Is(err, errGitIgnoreNoMatch) {
				row.Problem = "Git ignore status is unavailable"
				return
			}
			break
		} else if !os.IsNotExist(err) {
			row.Problem = "repository discovery is unavailable"
			return
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if ctx.Err() != nil {
		row.Problem = "scan deadline or cancellation reached"
		return
	}
	file, err := os.Open(row.Path)
	if err != nil {
		row.Problem = "file could not be opened"
		return
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || noLinks(row.Path) != nil {
		row.Problem = "file identity changed before reading"
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, secrets.MaxBytes+1))
	if err != nil {
		row.Problem = "file could not be read"
		return
	}
	after, err := os.Lstat(row.Path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || noLinks(row.Path) != nil {
		row.Problem = "file changed during the scan"
		return
	}
	if ctx.Err() != nil {
		row.Problem = "scan deadline or cancellation reached"
		return
	}
	result := secrets.Scan(data)
	if ctx.Err() != nil {
		row.Problem = "scan deadline or cancellation reached"
		return
	}
	row.State = result.State
	row.Findings = result.Findings
	row.Capped = result.Capped
}
