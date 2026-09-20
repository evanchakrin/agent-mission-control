package desktop

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Item struct {
	ID           string  `json:"id"`
	Category     string  `json:"category,omitempty"`
	Path         string  `json:"path"`
	Name         string  `json:"name"`
	Label        string  `json:"label,omitempty"`
	Size         int64   `json:"size"`
	Mtime        float64 `json:"mtime"`
	Exists       bool    `json:"exists"`
	ExpectedHash string  `json:"expectedHash,omitempty"`
}
type Root struct {
	Path    string `json:"path"`
	Label   string `json:"label"`
	AddedAt int64  `json:"addedAt"`
	OK      bool   `json:"ok"`
}

func idFor(p string) string { return strings.ReplaceAll(url.QueryEscape(p), "+", "%20") }

func noLinks(p string) error {
	current := filepath.Clean(p)
	for {
		st, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return errors.New("linked files and folders are not editable here")
			}
			if err = platformLinkCheck(current, st, current == filepath.Clean(p)); err != nil {
				return err
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}
func (m *Manager) validateRoot(raw string) (string, error) {
	if !filepath.IsAbs(raw) || strings.HasPrefix(raw, `\\`) || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\x00\r\n") || len(raw) > 400 {
		return "", errors.New("use a full local project folder path")
	}
	p := filepath.Clean(raw)
	if filepath.Dir(p) == p || norm(p) == norm(m.opts.HomeDir) || under(m.opts.StateDir, p) {
		return "", errors.New("that folder is too broad or belongs to AMC")
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.EqualFold(part, ".claude") || strings.EqualFold(part, ".codex") {
			return "", errors.New("agent configuration folders are not project roots")
		}
	}
	for _, blocked := range []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData"), "/etc", "/usr", "/bin", "/sbin", "/System", "/Library"} {
		if blocked != "" && filepath.IsAbs(blocked) && under(blocked, p) {
			return "", errors.New("system folders are not project roots")
		}
	}
	if err := noLinks(p); err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return "", errors.New("project folder is unavailable")
	}
	return p, nil
}

var cwdPattern = regexp.MustCompile(`"cwd"\s*:\s*("(?:[^"\\]|\\.)*")`)

func (m *Manager) rootList() ([]Root, error) {
	var roots []Root
	if err := m.load("roots", &roots); err != nil {
		return nil, err
	}
	for i := range roots {
		_, err := m.validateRoot(roots[i].Path)
		roots[i].OK = err == nil
	}
	if roots == nil {
		roots = []Root{}
	}
	return roots, nil
}
func (m *Manager) approvedRoots() ([]string, error) {
	var roots []string
	seen := map[string]bool{}
	add := func(raw string) {
		if p, e := m.validateRoot(raw); e == nil && !seen[norm(p)] {
			seen[norm(p)] = true
			roots = append(roots, p)
		}
	}
	for _, p := range m.opts.Roots {
		add(p)
	}
	stored, err := m.rootList()
	if err != nil {
		return nil, err
	}
	for _, r := range stored {
		add(r.Path)
	}
	// Discovery reads only the explicitly configured local owner tree. The hub's
	// network inventory is never a source of local write authority.
	if m.opts.ClaudeProjectsDir != "" {
		projects, _ := os.ReadDir(m.opts.ClaudeProjectsDir)
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			dir := filepath.Join(m.opts.ClaudeProjectsDir, project.Name())
			if noLinks(dir) != nil {
				continue
			}
			files, _ := os.ReadDir(dir)
			read := 0
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
					continue
				}
				p := filepath.Join(dir, file.Name())
				if noLinks(p) != nil {
					continue
				}
				f, e := os.Open(p)
				if e != nil {
					continue
				}
				head, _ := io.ReadAll(io.LimitReader(f, 16384))
				f.Close()
				read++
				match := cwdPattern.FindSubmatch(head)
				if len(match) == 2 {
					var cwd string
					if json.Unmarshal(match[1], &cwd) == nil {
						add(cwd)
						break
					}
				}
				if read == 3 {
					break
				}
			}
		}
	}
	sort.Strings(roots)
	return roots, nil
}
func (m *Manager) allTargets() ([]Item, error) {
	roots, err := m.approvedRoots()
	if err != nil {
		return nil, err
	}
	var out []Item
	seen := map[string]bool{}
	add := func(category, p, name, label string) {
		if seen[norm(p)] || noLinks(p) != nil {
			return
		}
		seen[norm(p)] = true
		item := Item{ID: idFor(p), Path: p, Name: name, Label: label, Category: category}
		if st, e := os.Stat(p); e == nil {
			if !st.Mode().IsRegular() || st.Size() > MaxBrainBytes {
				return
			}
			item.Exists = true
			item.Size = st.Size()
			item.Mtime = float64(st.ModTime().UnixNano()) / 1e6
		}
		out = append(out, item)
	}
	for _, f := range []string{"CLAUDE.md", "settings.json", "settings.local.json"} {
		add("Claude · global", filepath.Join(m.opts.HomeDir, ".claude", f), f, "Global — every session on this machine")
	}
	for _, f := range []string{"AGENTS.md", "config.toml"} {
		add("Codex", filepath.Join(m.opts.HomeDir, ".codex", f), f, "Codex global")
	}
	for _, root := range roots {
		label := filepath.Base(root)
		for _, f := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".cursorrules", filepath.Join(".claude", "settings.json")} {
			add("Guidance · "+label, filepath.Join(root, f), filepath.Base(f), label)
		}
	}
	if m.opts.ClaudeProjectsDir != "" {
		projects, _ := os.ReadDir(m.opts.ClaudeProjectsDir)
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			dir := filepath.Join(m.opts.ClaudeProjectsDir, project.Name(), "memory")
			if noLinks(dir) != nil {
				continue
			}
			files, _ := os.ReadDir(dir)
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") {
					add("Claude · project memory", filepath.Join(dir, f.Name()), f.Name(), project.Name())
				}
			}
		}
	}
	return out, nil
}
func (m *Manager) inventory() ([]Item, error) {
	all, err := m.allTargets()
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, item := range all {
		if item.Exists {
			out = append(out, item)
		}
	}
	return out, nil
}
func (m *Manager) directiveTargets() ([]Item, error) {
	all, err := m.allTargets()
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, item := range all {
		if item.Name == "CLAUDE.md" || item.Name == "AGENTS.md" {
			out = append(out, item)
		}
	}
	return out, nil
}
func (m *Manager) resolve(id string, existing bool) (Item, error) {
	all, err := m.allTargets()
	if err != nil {
		return Item{}, err
	}
	for _, item := range all {
		if (id == item.ID || norm(id) == norm(item.Path)) && (!existing || item.Exists) {
			return item, nil
		}
	}
	return Item{}, fail(404, "unknown local file")
}
func (m *Manager) validateFile(p string) error {
	_, err := m.resolve(idFor(p), false)
	if err != nil {
		// An owner removing a root stops offering it for new directives, but
		// already-planted targets remain explicitly owned and can be retired.
		items, loadErr := m.directiveItems()
		if loadErr != nil {
			return loadErr
		}
		allowed := false
		for _, d := range items {
			for _, target := range d.Targets {
				if norm(target.Path) == norm(p) && (filepath.Base(p) == "CLAUDE.md" || filepath.Base(p) == "AGENTS.md") {
					if _, e := m.validateRoot(filepath.Dir(p)); e == nil {
						allowed = true
					}
				}
			}
		}
		if !allowed {
			return err
		}
	}
	return noLinks(p)
}
