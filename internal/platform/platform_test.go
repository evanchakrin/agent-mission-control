package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testOwnerSID = "S-1-5-21-1-2-3-1001"

func fixtureConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "transcripts")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{Role: Collector, DataDir: filepath.Join(root, "state"), OwnerSID: testOwnerSID, Sources: []SourceRoot{{Provider: "claude", Path: source}}, HubURL: "http://127.0.0.1:4173", Token: "never-log-this-secret"}
}
func TestRoleConfigValidation(t *testing.T) {
	base := fixtureConfig(t)
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"relative-data", func(c *Config) { c.DataDir = "relative" }},
		{"root-data", func(c *Config) { c.DataDir = filepath.VolumeName(c.DataDir) + string(filepath.Separator) }},
		{"source-overlap", func(c *Config) { c.DataDir = filepath.Join(c.Sources[0].Path, "state") }},
		{"duplicate-source", func(c *Config) { c.Sources = append(c.Sources, c.Sources[0]) }},
		{"remote-source", func(c *Config) { c.Sources = []SourceRoot{{Provider: "claude", Path: `\\server\share\chats`}} }},
		{"unknown-provider", func(c *Config) { c.Sources = []SourceRoot{{Provider: "unknown", Path: c.Sources[0].Path}} }},
		{"credential-url", func(c *Config) { c.HubURL = "http://secret@example.com" }},
		{"hub-path", func(c *Config) { c.HubURL = "http://example.com/path" }},
		{"wildcard-listener", func(c *Config) { c.ListenAddress = "0.0.0.0:4173" }},
		{"invalid-port", func(c *Config) { c.ListenAddress = "127.0.0.1:wat" }},
		{"desktop-network", func(c *Config) { c.Role = Desktop; c.ListenAddress = "192.168.1.2:4173" }},
		{"negative-resource-limit", func(c *Config) { c.SpoolMaxBytes = -1 }},
		{"remote-pipe", func(c *Config) { c.PipeName = `\\server\pipe\bad` }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			c := base
			c.Sources = append([]SourceRoot(nil), base.Sources...)
			test.change(&c)
			if c.Validate() == nil {
				t.Fatal("unsafe configuration was accepted")
			}
		})
	}
	base.Sources[0].Path = filepath.Join(t.TempDir(), "temporarily-offline")
	if err := base.Validate(); err != nil {
		t.Fatalf("temporarily missing source must remain runtime-valid: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", base, base, base), base.Token) {
		t.Fatal("config formatting leaked token")
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("config", slog.Any("config", base))
	if strings.Contains(log.String(), base.Token) {
		t.Fatal("structured config logging leaked token")
	}
}

func TestServiceDataOwnershipRefusesUnrelatedDirectories(t *testing.T) {
	c := fixtureConfig(t)
	if err := validateServiceData(c); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "keep.txt"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceData(c); err == nil {
		t.Fatal("unrelated directory accepted for ACL replacement")
	}
	if err := markServiceData(c); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A marker fabricated by an ordinary owner must not authorize an
		// elevated installer to seize a nonempty directory's ACL.
		if err := validateServiceData(c); err == nil {
			t.Fatal("unprotected owner-created marker authorized service data adoption")
		}
		return
	}
	if err := validateServiceData(c); err != nil {
		t.Fatal(err)
	}
	c.Role = Hub
	if err := validateServiceData(c); err == nil {
		t.Fatal("different role accepted another role's state")
	}
}
func TestLoadConfigRejectsUnknownAndTrailingJSON(t *testing.T) {
	c := fixtureConfig(t)
	p := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{append(append([]byte{}, b...), []byte(" {}")...), []byte(`{"surprise":true}`)} {
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
func TestRotatingLogsAreBoundedAndValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.log")
	w, err := newRotatingWriter(p, 128, 5)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		b, _ := json.Marshal(map[string]any{"event": "test", "sequence": i})
		if _, err = w.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = w.Write(make([]byte, 129)); err == nil {
		t.Fatal("oversized record accepted")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(p + "*")
	if len(files) != 5 {
		t.Fatalf("got %d files", len(files))
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 128 {
			t.Fatalf("log exceeded cap: %d", len(b))
		}
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatalf("rotation split a JSON record: %s", line)
			}
		}
	}
	if _, err = w.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed logger accepted writes")
	}
}
func TestJSONLoggerAndFreeSpace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.log")
	logger, closer, err := NewJSONLogger(p)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("startup", "role", "hub", "bootId", "test")
	closer.Close()
	b, err := os.ReadFile(p)
	if err != nil || !json.Valid(bytes.TrimSpace(b)) {
		t.Fatalf("invalid structured log: %s %v", b, err)
	}
	available, total, err := FreeSpace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || available > total {
		t.Fatalf("invalid disk information: %d/%d", available, total)
	}
}
func TestInstanceLockExcludesDuplicateAndReleases(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireInstance(Hub, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireInstance(Hub, dir); !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("duplicate lock: %v", err)
	}
	other, err := AcquireInstance(Collector, dir)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	first.Close()
	last, err := AcquireInstance(Hub, dir)
	if err != nil {
		t.Fatal(err)
	}
	last.Close()
}
func TestDesktopTaskIsUnelevatedAndHasNoTimeout(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "owner & settings.json")
	os.WriteFile(config, []byte("{}"), 0600)
	b, err := DesktopTaskXML(DesktopSpec{Executable: exe, ConfigPath: config, OwnerSID: testOwnerSID})
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v", err)
		}
	}
	for _, required := range []string{"<RunLevel>LeastPrivilege</RunLevel>", "<LogonType>InteractiveToken</LogonType>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<RestartOnFailure>", "&amp;"} {
		if !bytes.Contains(b, []byte(required)) {
			t.Fatalf("missing task property %s", required)
		}
	}
	if _, err = DesktopTaskName("S-1-5-18"); err == nil {
		t.Fatal("system identity accepted for desktop")
	}
}
func TestInvokeConvertsPanicToFatalError(t *testing.T) {
	err := invoke(context.Background(), func(context.Context) error { panic("broken invariant") })
	if err == nil || !strings.Contains(err.Error(), "broken invariant") {
		t.Fatalf("panic was swallowed: %v", err)
	}
}

func TestWorkerHelper(t *testing.T) {
	if os.Getenv("AMC_PLATFORM_WORKER_TEST") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "memory" {
		var chunks [][]byte
		for i := 0; i < 50; i++ {
			b := make([]byte, 4*1024*1024)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			chunks = append(chunks, b)
		}
		runtime.KeepAlive(chunks)
		os.Exit(0)
	}
	if mode == "exit" {
		fmt.Print("worker-ready")
		os.Exit(0)
	}
	fmt.Print("worker-ready")
	time.Sleep(time.Minute)
	os.Exit(0)
}
func helperCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWorkerHelper$", "--", mode)
	cmd.Env = append(os.Environ(), "AMC_PLATFORM_WORKER_TEST=1")
	return cmd
}
func TestWorkerStartsAndCancellationTerminates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := helperCommand(t, "exit")
	var output bytes.Buffer
	cmd.Stdout = &output
	w, err := StartWorker(ctx, WorkerSpec{Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if err = w.Wait(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "worker-ready" {
		t.Fatalf("worker did not run: %s", output.String())
	}
	childCtx, stop := context.WithCancel(ctx)
	cmd = helperCommand(t, "sleep")
	w, err = StartWorker(childCtx, WorkerSpec{Command: cmd})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	defer w.Stop()
	stop()
	select {
	case <-w.Done():
		if w.Wait() == nil {
			t.Fatal("canceled worker exited successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker survived cancellation")
	}
}
