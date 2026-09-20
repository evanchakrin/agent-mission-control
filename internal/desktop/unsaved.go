package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type UnsavedFile struct {
	RequestedPath             string     `json:"requestedPath"`
	RequestedWorkingDirectory string     `json:"requestedWorkingDirectory,omitempty"`
	Path                      string     `json:"path"`
	State                     string     `json:"state"`
	Problem                   string     `json:"problem,omitempty"`
	Repository                string     `json:"repository,omitempty"`
	SizeBytes                 int64      `json:"sizeBytes,omitempty"`
	ModifiedAt                *time.Time `json:"modifiedAt,omitempty"`
}

type UnsavedPath struct {
	Path             string `json:"path"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
}

func localAbsolutePath(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return false
		}
	}
	return len(path) <= 4096 && filepath.IsAbs(path) && strings.IndexFunc(path, func(r rune) bool { return r < 32 || r == 127 }) < 0 && !strings.HasPrefix(path, `\\`) && !strings.HasPrefix(path, "//") && !strings.Contains(strings.TrimPrefix(path, filepath.VolumeName(path)), ":")
}

// Resolve only explicit record context. Neither the service home nor the
// session's final project is a fallback. Parent traversal is left unresolved:
// cleaning it before link checks could change the meaning of a linked path.
func resolveUnsavedPath(file UnsavedPath, roots []string) (string, string) {
	if localAbsolutePath(file.Path) {
		return file.Path, ""
	}
	if !localAbsolutePath(file.WorkingDirectory) || !filepath.IsLocal(file.Path) || strings.ContainsAny(file.Path, ":\\") && filepath.Separator != '\\' {
		return "", "relative path needs an explicit local working directory"
	}
	for _, part := range strings.FieldsFunc(file.Path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", "parent traversal requires manual resolution"
		}
	}
	approved := false
	for _, root := range roots {
		if under(root, file.WorkingDirectory) {
			approved = true
			break
		}
	}
	if !approved {
		return "", "recorded working directory is outside approved local roots"
	}
	if noLinks(file.WorkingDirectory) != nil {
		return "", "recorded working directory is linked or unavailable"
	}
	path := filepath.Join(file.WorkingDirectory, file.Path)
	if !localAbsolutePath(path) {
		return "", "resolved path is not a supported local file path"
	}
	return path, ""
}

type UnsavedCheck struct {
	MachineID  string        `json:"machineId"`
	Files      []UnsavedFile `json:"files"`
	ObservedAt time.Time     `json:"observedAt"`
	Scope      string        `json:"scope"`
}

func (m *Manager) handleLocalIdentity(w http.ResponseWriter, r *http.Request) {
	if err := m.gate(r); err != nil {
		respond(w, nil, err)
		return
	}
	if r.Method != http.MethodGet {
		respond(w, nil, fail(405, "local identity is configured outside the network interface"))
		return
	}
	state := "configured"
	if m.opts.LocalMachineID == "" {
		state = "unconfigured"
	}
	respond(w, map[string]string{"machineId": m.opts.LocalMachineID, "state": state}, nil)
}

type localRepositoryRoot struct {
	path string
	err  error
}

type localRepositoryRoots struct {
	mu      sync.Mutex
	entries map[string]localRepositoryRoot
}

func (c *localRepositoryRoots) discover(ctx context.Context, dir string) localRepositoryRoot {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := norm(dir)
	if cached, ok := c.entries[key]; ok {
		return cached
	}
	path, err := readOnlyGit(ctx, dir, "rev-parse", "--show-toplevel")
	result := localRepositoryRoot{path: path, err: err}
	c.entries[key] = result
	return result
}

// Porcelain -z may include ignored siblings and a second pathname for renames.
// Match only complete current-path records; never treat that second pathname
// as another status or infer a child's state from a directory prefix.
func unsavedPathStatus(raw, rel string) string {
	if !strings.HasSuffix(raw, "\x00") {
		return ""
	}
	records := strings.Split(raw, "\x00")
	state := ""
	for i := 0; i < len(records)-1; i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' {
			return ""
		}
		code := record[:2]
		if code != "??" && code != "!!" && (code == "  " || strings.Trim(code, " MADRCU") != "") {
			return ""
		}
		if strings.ContainsAny(code, "RC") {
			i++
			if i >= len(records)-1 {
				return ""
			}
		}
		if record[3:] != rel {
			continue
		}
		if state != "" {
			return ""
		}
		switch code {
		case "??":
			state = "untracked"
		case "!!":
			state = "ignored"
		default:
			state = "tracked"
		}
	}
	return state
}

func (m *Manager) handleUnsaved(w http.ResponseWriter, r *http.Request) {
	if err := m.gate(r); err != nil {
		respond(w, nil, err)
		return
	}
	if r.Method != http.MethodPost {
		respond(w, nil, fail(405, "use POST for a bounded local check"))
		return
	}
	var input struct {
		Paths     []string      `json:"paths"`
		Files     []UnsavedPath `json:"files"`
		MachineID string        `json:"machineId"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		respond(w, nil, fail(400, "invalid local check request"))
		return
	}
	if m.opts.LocalMachineID == "" {
		respond(w, nil, fail(409, "configure desktop machineId to match its local collector before checking transcript paths"))
		return
	}
	if input.MachineID != m.opts.LocalMachineID {
		respond(w, nil, fail(403, "transcript machine does not match this desktop"))
		return
	}
	if input.Paths != nil && input.Files != nil {
		respond(w, nil, fail(400, "provide paths or files, not both"))
		return
	}
	var result UnsavedCheck
	var err error
	if input.Files != nil {
		result, err = m.CheckUnsavedFiles(r.Context(), input.Files)
	} else {
		result, err = m.CheckUnsaved(r.Context(), input.Paths)
	}
	respond(w, result, err)
}

// CheckUnsaved observes only explicitly supplied approved local paths. It does
// not assert that a remote transcript's identical path refers to this machine.
// The caller must bind source-machine identity before attributing these results.
func (m *Manager) CheckUnsaved(ctx context.Context, paths []string) (UnsavedCheck, error) {
	if len(paths) < 1 || len(paths) > 25 {
		return UnsavedCheck{}, fail(400, "check between one and 25 paths")
	}
	files := make([]UnsavedPath, len(paths))
	for _, path := range paths {
		if !localAbsolutePath(path) {
			return UnsavedCheck{}, fail(400, "use absolute local file paths without stream names")
		}
	}
	for i, path := range paths {
		files[i].Path = path
	}
	return m.CheckUnsavedFiles(ctx, files)
}

func (m *Manager) CheckUnsavedFiles(ctx context.Context, files []UnsavedPath) (UnsavedCheck, error) {
	result := UnsavedCheck{MachineID: m.opts.LocalMachineID, Files: []UnsavedFile{}, ObservedAt: time.Now().UTC(), Scope: "approved-local-paths-only"}
	if len(files) < 1 || len(files) > 25 {
		return result, fail(400, "check between one and 25 paths")
	}
	for _, file := range files {
		if file.Path == "" || len(file.Path) > 4096 || len(file.WorkingDirectory) > 4096 || strings.IndexFunc(file.Path+file.WorkingDirectory, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return result, fail(400, "invalid recorded file context")
		}
	}
	if !m.unsavedMu.TryLock() {
		return result, fail(429, "a local unsaved-work check is already running")
	}
	defer m.unsavedMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots, err := m.approvedRoots()
	if err != nil {
		return result, err
	}
	rootCache := &localRepositoryRoots{entries: map[string]localRepositoryRoot{}}
	result.Files = make([]UnsavedFile, len(files))
	// Two bounded workers overlap native Git startup, not one process per path.
	// Distinct slots preserve request order; the shared deadline and scan lock
	// still cover the entire batch. Discovery is cached safely between workers.
	jobs := make(chan int, len(files))
	for i := range files {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for worker := 0; worker < min(2, len(files)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					result.Files[i] = UnsavedFile{RequestedPath: files[i].Path, RequestedWorkingDirectory: files[i].WorkingDirectory, Path: files[i].Path, State: "unknown", Problem: "check deadline or cancellation reached"}
					continue
				}
				path, problem := resolveUnsavedPath(files[i], roots)
				file := UnsavedFile{Path: files[i].Path, State: "unsupported-path", Problem: problem}
				if problem == "" {
					file = checkUnsavedFile(ctx, filepath.Clean(path), roots, rootCache)
				}
				file.RequestedPath = files[i].Path
				file.RequestedWorkingDirectory = files[i].WorkingDirectory
				if file.State == "unknown" && ctx.Err() != nil {
					file.Problem = "check deadline or cancellation reached"
				}
				result.Files[i] = file
			}
		}()
	}
	workers.Wait()
	return result, nil
}

func checkUnsavedFile(ctx context.Context, path string, roots []string, rootCache *localRepositoryRoots) UnsavedFile {
	out := UnsavedFile{Path: path, State: "unknown"}
	if ctx.Err() != nil {
		out.Problem = "check deadline or cancellation reached"
		return out
	}
	approved := false
	for _, root := range roots {
		if under(root, path) && norm(root) != norm(path) {
			approved = true
			break
		}
	}
	if !approved {
		out.State = "outside-approved-roots"
		return out
	}
	if err := noLinks(path); err != nil {
		out.Problem = "path is linked or unavailable"
		return out
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		out.State = "missing"
		return out
	}
	if err != nil || !st.Mode().IsRegular() {
		out.Problem = "regular file is unavailable"
		return out
	}
	out.SizeBytes = st.Size()
	modified := st.ModTime().UTC()
	out.ModifiedAt = &modified
	dir := filepath.Dir(path)
	rootResult := rootCache.discover(ctx, dir)
	root, err := rootResult.path, rootResult.err
	if err != nil {
		out.Problem = "repository discovery failed"
		return out
	}
	root = strings.TrimSuffix(strings.TrimSuffix(root, "\n"), "\r")
	if !filepath.IsAbs(root) || !under(root, path) || noLinks(root) != nil {
		out.Problem = "repository identity is unavailable"
		return out
	}
	rootApproved := false
	for _, approvedRoot := range roots {
		if under(approvedRoot, root) {
			rootApproved = true
			break
		}
	}
	if !rootApproved {
		out.Problem = "repository root is outside approved roots"
		return out
	}
	out.Repository = root
	rel, err := filepath.Rel(root, path)
	if err != nil {
		out.Problem = "repository path is unavailable"
		return out
	}
	rel = filepath.ToSlash(rel)
	status, err := readOnlyGit(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--", rel)
	if err != nil {
		out.Problem = "Git status could not be established"
		return out
	}
	// Select the exact current-path record. Git can also report ignored siblings;
	// truncated, duplicate or malformed records never prove an untracked file.
	switch unsavedPathStatus(status, rel) {
	case "untracked":
	case "ignored":
		out.State = "ignored"
		return out
	case "tracked":
		out.State = "tracked"
		return out
	default:
		if _, err := readOnlyGit(ctx, root, "check-ignore", "--quiet", "--", rel); err == nil {
			out.State = "ignored"
			return out
		}
		tracked, err := readOnlyGit(ctx, root, "ls-files", "--cached", "-z", "--", rel)
		if err == nil && tracked == rel+"\x00" {
			out.State = "tracked"
			return out
		}
		out.Problem = "Git did not report an exact untracked file"
		return out
	}
	history, err := readOnlyGit(ctx, root, "log", "--all", "--format=%H", "-1", "--", rel)
	if err != nil {
		out.Problem = "reachable branch history could not be checked"
		return out
	}
	if strings.TrimSpace(history) != "" {
		out.State = "in-history"
		return out
	}
	shallow, err := readOnlyGit(ctx, root, "rev-parse", "--is-shallow-repository")
	if err != nil || strings.TrimSpace(shallow) != "false" {
		out.Problem = "complete reachable history is unavailable (possibly a shallow clone)"
		return out
	}
	// A concurrent editor/Git operation can invalidate the prior observations.
	// Recheck untracked status and file identity; do not claim a stable snapshot.
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(st, after) || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) || noLinks(path) != nil {
		out.Problem = "file changed during the check"
		return out
	}
	status, err = readOnlyGit(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--", rel)
	if err != nil || unsavedPathStatus(status, rel) != "untracked" {
		out.Problem = "Git state changed during the check"
		return out
	}
	out.State = "untracked-no-reachable-history"
	return out
}
