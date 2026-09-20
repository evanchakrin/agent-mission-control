// Package platform contains the operating-system boundary for AMC processes.
// Nothing in this package installs or changes a service until explicitly called.
package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

type Role string

const (
	Hub       Role = "hub"
	Collector Role = "collector"
	Desktop   Role = "desktop"
)

func (r Role) ServiceName() string {
	switch r {
	case Hub:
		return "AMCHub"
	case Collector:
		return "AMCCollector"
	}
	return ""
}

func (r Role) valid() bool { return r == Hub || r == Collector || r == Desktop }

type SourceRoot struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
}

// Config always uses explicit absolute paths. A service account's profile is
// never a source of defaults for transcripts, credentials, or durable state.
type Config struct {
	Role            Role         `json:"role"`
	DataDir         string       `json:"dataDir"`
	OwnerSID        string       `json:"ownerSid"`
	Token           string       `json:"token,omitempty"`
	MachineID       string       `json:"machineId,omitempty"`
	MachineName     string       `json:"machineName,omitempty"`
	PipeName        string       `json:"pipeName,omitempty"`
	SpoolMaxBytes   int64        `json:"spoolMaxBytes,omitempty"`
	BytesPerSecond  int64        `json:"bytesPerSecond,omitempty"`
	Sources         []SourceRoot `json:"sources,omitempty"`
	HubURL          string       `json:"hubUrl,omitempty"`
	ListenAddress   string       `json:"listenAddress,omitempty"`
	OwnerHomeDir    string       `json:"ownerHomeDir,omitempty"`
	RepositoryRoots []string     `json:"repositoryRoots,omitempty"`
}

func LoadConfig(filename string) (Config, error) {
	var c Config
	f, err := os.Open(filename)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1024*1024))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("read role configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("role configuration must contain one JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !c.Role.valid() {
		return fmt.Errorf("unknown AMC role %q", c.Role)
	}
	data, err := safeAbsolutePath(c.DataDir)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if runtime.GOOS == "windows" && !ownerSIDPattern.MatchString(c.OwnerSID) {
		return errors.New("the individual desktop owner's Windows SID is required")
	}
	if c.Role == Collector && len(c.Sources) == 0 {
		return errors.New("collector requires at least one explicit transcript source")
	}
	if c.SpoolMaxBytes < 0 || c.BytesPerSecond < 0 {
		return errors.New("collector resource limits must not be negative")
	}
	if c.PipeName != "" && !pipeNamePattern.MatchString(c.PipeName) {
		return errors.New("invalid local pipe name")
	}
	seen := map[string]bool{}
	for _, source := range c.Sources {
		if source.Provider != "claude" && source.Provider != "codex" && source.Provider != "otel" {
			return fmt.Errorf("unknown source provider %q", source.Provider)
		}
		root, err := sourcePath(source.Path)
		if err != nil {
			return fmt.Errorf("%s source: %w", source.Provider, err)
		}
		if within(root, data) || within(data, root) {
			return errors.New("transcript sources and AMC data must not overlap")
		}
		key := pathKey(root)
		if seen[key] {
			return fmt.Errorf("duplicate source directory: %s", source.Path)
		}
		seen[key] = true
	}
	if c.HubURL != "" {
		u, err := url.Parse(c.HubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("hub URL must be an HTTP(S) origin without credentials or a path")
		}
	}
	if c.Role == Collector && c.HubURL == "" {
		return errors.New("collector requires a hub URL")
	}
	if c.ListenAddress != "" {
		host, port, err := net.SplitHostPort(c.ListenAddress)
		ip := net.ParseIP(host)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 1 || n > 65535 || ip == nil || ip.IsUnspecified() {
			return errors.New("listener must name one explicit IP address and valid port; wildcard listeners are not allowed")
		}
		if c.Role == Desktop && !ip.IsLoopback() {
			return errors.New("desktop listener must be loopback only")
		}
	}
	return nil
}

var pipeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}$`)

// An unplugged source is a collector health condition, not invalid runtime
// configuration. Installation performs the stronger accessibility check.
func sourcePath(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") || !filepath.IsAbs(raw) || strings.HasPrefix(raw, `\\`) || strings.HasPrefix(raw, "//") {
		return "", errors.New("an absolute local source path is required")
	}
	p := filepath.Clean(raw)
	if filepath.Dir(p) == p {
		return "", errors.New("a filesystem root is too broad")
	}
	if canonical, err := filepath.EvalSymlinks(p); err == nil {
		p = canonical
	}
	return p, nil
}

// Resolve the existing prefix, including junctions, even for a data directory
// that the installer has not created yet.
func safeAbsolutePath(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") || !filepath.IsAbs(raw) {
		return "", errors.New("an absolute local path is required")
	}
	if strings.HasPrefix(raw, `\\`) || strings.HasPrefix(raw, "//") {
		return "", errors.New("network and device paths are not allowed")
	}
	p := filepath.Clean(raw)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			p = resolved
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", errors.New("path has no accessible parent")
		}
		missing = append(missing, filepath.Base(p))
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p = filepath.Join(p, missing[i])
	}
	if filepath.Dir(p) == p {
		return "", errors.New("a filesystem root is too broad")
	}
	return p, nil
}

func pathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.Clean(p))
	}
	return filepath.Clean(p)
}
func within(root, p string) bool {
	rel, err := filepath.Rel(pathKey(root), pathKey(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var ErrUnsupported = errors.New("this operation requires Windows")
var ErrAlreadyRunning = errors.New("this AMC role is already running for the data directory")

type ServiceSpec struct {
	Config       Config
	Executable   string
	ConfigPath   string
	GateManifest string
}

func (s ServiceSpec) Validate() error {
	if err := s.Config.Validate(); err != nil {
		return err
	}
	if s.Config.Role.ServiceName() == "" {
		return errors.New("desktop uses an unelevated logon task, not a service")
	}
	if s.Config.Role == Hub && s.Config.PipeName == "" {
		return errors.New("installed hub requires an explicit local pipe name")
	}
	for _, source := range s.Config.Sources {
		st, err := os.Stat(source.Path)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("source directory is not accessible for installation: %s", source.Path)
		}
	}
	data, _ := safeAbsolutePath(s.Config.DataDir)
	for label, p := range map[string]string{"executable": s.Executable, "configuration": s.ConfigPath} {
		canonical, err := safeAbsolutePath(p)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if within(data, canonical) {
			return fmt.Errorf("%s must not be stored inside writable service data", label)
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%s must be an existing regular file", label)
		}
	}
	loaded, err := LoadConfig(s.ConfigPath)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(loaded)
	b, _ := json.Marshal(s.Config)
	if string(a) != string(b) {
		return errors.New("service configuration differs from the configuration file")
	}
	return nil
}

// Accidental fmt logging must never reveal the bearer credential.
func (c Config) String() string {
	return fmt.Sprintf("AMC %s (data=%s, sources=%d, token=[redacted])", c.Role, c.DataDir, len(c.Sources))
}
func (c Config) GoString() string { return c.String() }

type Status struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	State     string `json:"state"`
	ProcessID uint32 `json:"processId,omitempty"`
	Account   string `json:"account,omitempty"`
	StartType uint32 `json:"startType,omitempty"`
}
