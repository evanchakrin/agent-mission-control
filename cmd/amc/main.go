// AMC v2 candidate. Installing or taking over legacy addresses is release-gated.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/evanchakrin/agent-mission-control/internal/accounting"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/backup"
	"github.com/evanchakrin/agent-mission-control/internal/collector"
	"github.com/evanchakrin/agent-mission-control/internal/desktop"
	"github.com/evanchakrin/agent-mission-control/internal/hub"
	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/legacy"
	"github.com/evanchakrin/agent-mission-control/internal/migration"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/runtimeinfo"
	"github.com/evanchakrin/agent-mission-control/internal/store"
	assets "github.com/evanchakrin/agent-mission-control/public"
)

const version = "8.0.0-stats.1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		jsonMode := false
		for _, arg := range os.Args[1:] {
			if arg == "--json" {
				jsonMode = true
			}
		}
		if jsonMode {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
func output(jsonMode bool, v any) error {
	if jsonMode {
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		fmt.Println(string(b))
	}
	return err
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: amc <hub|collector|collector-adopt|desktop|import-owner|status|doctor|start|stop|restart|backup|restore|reindex|reindex-outdated|reindex-hook-evidence|reindex-status|migration-preflight|migrate|stage-release|stage-evidence|stage-config|install|uninstall|version> [command options] [--json]; running roles require --config <absolute-file>")
	}
	command := args[0]
	if command == "version" {
		for _, arg := range args[1:] {
			if arg == "--json" {
				return output(true, map[string]any{"version": version, "candidate": true, "authoritative": false})
			}
		}
		fmt.Println(version)
		return nil
	}
	if command == "parser-worker" {
		return runParserWorker(args[1:])
	}
	if command == "collector-adopt" {
		return runCollectorAdopt(args[1:])
	}
	if command == "stage-release" {
		return runStageRelease(args[1:], platform.StageRelease)
	}
	if command == "stage-evidence" {
		return runStageEvidence(args[1:], platform.StageReleaseEvidence)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "", "absolute role configuration file")
	jsonMode := flags.Bool("json", false, "machine readable output")
	destination := flags.String("destination", "", "new backup/restore directory or stage-config file")
	backupPath := flags.String("backup", "", "verified backup directory for restore")
	sourcePath := flags.String("source", "", "explicit legacy state directory for migration or import-owner")
	desktopConfigPath := flags.String("desktop-config", "", "desktop configuration included in a complete backup")
	sessionID := flags.String("session", "", "stable session ID to rebuild")
	operationID := flags.String("operation-id", "", "stable idempotency key; reuse after an uncertain response")
	revisionID := flags.String("revision", "", "projection rebuild revision to inspect")
	maxRebuilds := flags.Int("max-rebuilds", 1, "maximum hook-evidence rebuilds to queue (1-25)")
	evidenceKind := flags.String("evidence", "javascript", "reindex-hook-evidence checkpoint: javascript or undo")
	installExecutable := flags.String("executable", "", "install: absolute path to the prepared release executable")
	gateManifest := flags.String("gate-manifest", "", "hub/desktop install: absolute path to complete release acceptance evidence; not required for collectors")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("--config is required; service accounts never discover sources from their home directory")
	}
	config, err := platform.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	switch command {
	case "import-owner":
		counts, err := importOwner(config, *sourcePath)
		if err != nil {
			return err
		}
		return output(*jsonMode, map[string]any{"ok": true, "imported": counts, "sourcePreserved": true})
	case "stage-config":
		if *destination == "" {
			return errors.New("stage-config requires a new absolute --destination configuration file")
		}
		if err := platform.StageServiceConfig(*destination, config); err != nil {
			return err
		}
		return output(*jsonMode, map[string]any{"configuration": *destination, "role": config.Role, "registered": false, "started": false})
	case "reindex-hook-evidence":
		if config.Role != platform.Hub && config.Role != platform.Desktop {
			return errors.New("hook recovery requires an owner hub or desktop configuration")
		}
		report, e := queueEvidenceRebuilds(ctx, pipeClient(config.PipeName), "http://amc", *operationID, *maxRebuilds, *evidenceKind)
		if outErr := output(*jsonMode, report); outErr != nil {
			return outErr
		}
		return e
	case "reindex-outdated":
		if config.Role != platform.Hub && config.Role != platform.Desktop {
			return errors.New("reindex-outdated requires an owner hub or desktop configuration")
		}
		report, e := queueOutdatedRebuilds(ctx, pipeClient(config.PipeName), "http://amc", *operationID)
		if outErr := output(*jsonMode, report); outErr != nil {
			return outErr
		}
		return e
	case "reindex", "reindex-status":
		if config.Role != platform.Hub && config.Role != platform.Desktop {
			return errors.New("reindex requires an owner hub or desktop configuration")
		}
		method, path, body := http.MethodGet, "", []byte(nil)
		if command == "reindex" {
			if *sessionID == "" || *operationID == "" {
				return errors.New("reindex requires --session and --operation-id")
			}
			method = http.MethodPost
			path = "/api/v2/sessions/" + url.PathEscape(*sessionID) + "/rebuild"
			body, _ = json.Marshal(map[string]string{"operationId": *operationID})
		} else {
			if *revisionID == "" {
				return errors.New("reindex-status requires --revision")
			}
			path = "/api/v2/rebuilds/" + url.PathEscape(*revisionID)
		}
		request, e := http.NewRequestWithContext(ctx, method, "http://amc"+path, bytes.NewReader(body))
		if e != nil {
			return e
		}
		request.Header.Set("Content-Type", "application/json")
		response, e := pipeClient(config.PipeName).Do(request)
		if e != nil {
			return e
		}
		defer response.Body.Close()
		var value any
		if e = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&value); e != nil {
			return e
		}
		if response.StatusCode >= 400 {
			return fmt.Errorf("reindex request failed (%d): %v", response.StatusCode, value)
		}
		return output(*jsonMode, value)
	case "migration-preflight", "migrate":
		if *sourcePath == "" || *destination == "" {
			return errors.New("shadow migration requires --source and a new --destination")
		}
		roots := []migration.Root{}
		for _, r := range config.Sources {
			roots = append(roots, migration.Root{Path: r.Path, Provider: r.Provider, MachineID: config.MachineID})
		}
		_, total, e := platform.FreeSpace(filepath.Dir(*destination))
		if e != nil {
			return e
		}
		plan, e := migration.Preflight(ctx, migration.Options{LegacyStateDir: *sourcePath, Destination: *destination, LocalRoots: roots, ReserveBytes: int64(max(uint64(5<<30), total/20))})
		if e != nil {
			return e
		}
		if command == "migration-preflight" {
			return output(*jsonMode, plan)
		}
		result, e := migration.Run(ctx, plan, store.Options{})
		if e != nil {
			return e
		}
		return output(*jsonMode, result)
	case "hub", "collector", "desktop":
		if string(config.Role) != command {
			return errors.New("command and configuration role do not match")
		}
		if err = os.MkdirAll(config.DataDir, 0700); err != nil {
			return err
		}
		lock, err := platform.AcquireInstance(config.Role, config.DataDir)
		if err != nil {
			return err
		}
		defer lock.Close()
		logger, closer, err := platform.NewJSONLogger(filepath.Join(config.DataDir, "logs", command+".jsonl"))
		if err != nil {
			return err
		}
		defer closer.Close()
		journal, err := runtimeinfo.Begin(config.DataDir, command, version)
		if err != nil {
			return err
		}
		logger.Info("starting", "role", command, "version", version, "runtime", journal.Snapshot())
		runRole := func(ctx context.Context) error {
			switch config.Role {
			case platform.Hub:
				return runHub(ctx, config, logger, journal)
			case platform.Collector:
				return runCollector(ctx, config, logger, journal)
			case platform.Desktop:
				return runDesktop(ctx, config)
			}
			return errors.New("unknown role")
		}
		if config.Role == platform.Desktop {
			err = runRole(ctx)
		} else {
			err = platform.RunService(ctx, config.Role, runRole)
		}
		journalReason := "stopped"
		if err != nil && !errors.Is(err, context.Canceled) {
			journalReason = "runtime-error"
		}
		if e := journal.Finish(journalReason); e != nil {
			logger.Error("runtime journal could not record exit", "error", e)
			if err == nil {
				err = e
			}
		}
		reason := "deliberate-stop"
		if err != nil && !errors.Is(err, context.Canceled) {
			reason = "fatal"
			logger.Error("exiting", "reason", reason, "error", err, "runtime", journal.Snapshot())
		} else {
			logger.Info("exiting", "reason", reason, "runtime", journal.Snapshot())
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case "status":
		var state platform.Status
		if config.Role == platform.Desktop {
			state, err = platform.DesktopTaskStatus(ctx, config.OwnerSID)
		} else {
			state, err = platform.ServiceStatus(config.Role)
		}
		if err != nil {
			return err
		}
		return output(*jsonMode, state)
	case "doctor":
		available, total, spaceErr := platform.FreeSpace(config.DataDir)
		report := map[string]any{"version": version, "role": config.Role, "dataDir": config.DataDir, "availableBytes": available, "volumeBytes": total, "reserveBytes": max(uint64(5<<30), total/20), "releaseReady": false, "productionChanged": false}
		if spaceErr != nil {
			report["storageError"] = spaceErr.Error()
		}
		var status platform.Status
		var statusErr error
		if config.Role == platform.Desktop {
			status, statusErr = platform.DesktopTaskStatus(ctx, config.OwnerSID)
		} else {
			status, statusErr = platform.ServiceStatus(config.Role)
		}
		if statusErr == nil {
			report["service"] = status
		} else {
			report["serviceError"] = statusErr.Error()
		}
		if config.Role == platform.Hub || config.Role == platform.Desktop {
			client := pipeClient(config.PipeName)
			r, e := client.Get("http://amc/api/v2/health")
			if e == nil {
				defer r.Body.Close()
				var health any
				e = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&health)
				if e == nil {
					report["health"] = health
				}
			}
			if e != nil {
				report["healthError"] = e.Error()
			}
		}
		return output(*jsonMode, report)
	case "start":
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if config.Role == platform.Desktop {
			err = platform.StartDesktopTask(ctx, config.OwnerSID)
		} else {
			err = platform.StartService(ctx, config.Role)
		}
	case "stop":
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if config.Role == platform.Desktop {
			err = platform.StopDesktopTask(ctx, config.OwnerSID)
		} else {
			err = platform.StopService(ctx, config.Role)
		}
	case "restart":
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if config.Role == platform.Desktop {
			if err = platform.StopDesktopTask(ctx, config.OwnerSID); err == nil {
				err = platform.StartDesktopTask(ctx, config.OwnerSID)
			}
		} else {
			if err = platform.StopService(ctx, config.Role); err == nil {
				err = platform.StartService(ctx, config.Role)
			}
		}
	case "install":
		err = installVerifiedRelease(ctx, config, *configPath, *installExecutable, *gateManifest, installActions{platform.InstallService, platform.InstallDesktopTask})
	case "uninstall":
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if config.Role == platform.Desktop {
			err = platform.UninstallDesktopTask(ctx, config.OwnerSID)
		} else {
			err = platform.UninstallService(ctx, config.Role)
		}
	case "backup":
		if config.Role != platform.Hub || *destination == "" || *desktopConfigPath == "" {
			return errors.New("complete backup requires hub configuration, --desktop-config and a new --destination directory")
		}
		desktopConfig, err := platform.LoadConfig(*desktopConfigPath)
		if err != nil {
			return err
		}
		if _, err = os.Stat(filepath.Join(config.DataDir, "ledger.sqlite")); err != nil {
			return err
		}
		s, err := store.Open(config.DataDir, store.Options{})
		if err != nil {
			return err
		}
		defer s.Close()
		manifest, err := backup.Create(ctx, s, config, desktopConfig, *destination)
		if err != nil {
			return err
		}
		return output(*jsonMode, manifest)
	case "restore":
		if *backupPath == "" || *destination == "" {
			return errors.New("restore requires --backup and a new --destination; existing history is never overwritten")
		}
		_, err := backup.Restore(ctx, *backupPath, *destination)
		if err != nil {
			return err
		}
		return output(*jsonMode, map[string]any{"restoredTo": *destination, "installed": false, "note": "Hub ledger, desktop history and configuration restored. Reconcile collectors before treating missing post-backup history as recovered."})
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return err
	}
	return output(*jsonMode, map[string]any{"command": command, "role": config.Role, "ok": true, "historyPreserved": true})
}

func hubStoreOptions() store.Options {
	return store.Options{ReserveBytes: 5 << 30, AvailableBytes: func(path string) (int64, error) {
		available, total, err := platform.FreeSpace(path)
		if err != nil {
			return 0, err
		}
		reserve := max(uint64(5<<30), total/20)
		return int64(available) - int64(reserve-(5<<30)), nil
	}}
}
func runHub(ctx context.Context, c platform.Config, logger *slog.Logger, journal *runtimeinfo.Journal) error {
	debug.SetMemoryLimit(192 << 20)
	s, err := store.Open(c.DataDir, hubStoreOptions())
	if err != nil {
		return err
	}
	defer s.Close()
	// Build derived read models before serving requests. A first browser request
	// must not repeatedly time out trying to backfill an imported large corpus.
	logger.Info("preparing query read models")
	if err = s.SetupAnalytics(ctx); err != nil {
		return fmt.Errorf("prepare query read models: %w", err)
	}
	h := hub.New(s, c.Token, version)
	h.HealthStall = slowRequestReporter(logger, s.ConnectionStats)
	ledgerMonitor := &hub.LedgerMonitor{Probe: h.CheckLedger}
	h.LedgerStatus = func() hub.LedgerObservation { return ledgerMonitor.Snapshot(time.Now()) }
	h.RuntimeStatus = func() any { return journal.Snapshot() }
	historySampler := &accounting.HistorySampler{Capture: s.CaptureEconomics, OnError: func(err error) { logger.Error("economics history capture blocked", "error", err) }}
	h.EconomicsStatus = func() any { return historySampler.Status() }
	h.HubID, err = s.HubID(ctx)
	if err != nil {
		return err
	}
	storageSampler := &storageStatusSampler{probe: func() (uint64, uint64, error) { return platform.FreeSpace(c.DataDir) }}
	h.StorageStatus = func() any { return storageSampler.snapshot(time.Now()) }
	x := &indexer.Indexer{Store: s}
	h.Indexer = x
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go storageSampler.run(ctx)
	go ledgerMonitor.Run(ctx)
	if s.StatsOnly() {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, e := s.ReclaimIndexedBlobs(ctx, 128); e != nil && ctx.Err() == nil {
						logger.Warn("indexed chunk reclamation delayed", "error", e)
					}
				}
			}
		}()
	}
	worker := &indexer.ProcessWorker{Executable: executable, DataDir: c.DataDir, Lifetime: ctx}
	defer worker.Close()
	x.Process = func(ctx context.Context, sourceID, generation string) (int64, error) {
		return x.PublishOnce(ctx, sourceID, generation, worker.Prepare)
	}
	x.ProcessGroup = func(ctx context.Context, sources []indexer.SourceWork) ([]indexer.SourceResult, error) {
		return x.PublishGroup(ctx, sources, worker.Prepare)
	}
	x.RebuildProcess = func(ctx context.Context, revision string) (int64, error) {
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		return x.RebuildPrepared(ctx, revision, worker.PrepareRebuild)
	}
	pipe, err := platform.ListenOwnerPipe(c.PipeName, c.OwnerSID)
	if err != nil {
		return err
	}
	owner := hub.Server(h.OwnerHandler())
	ingest := http.NewServeMux()
	ingest.Handle("/v2/", h.IngestionHandler())
	if s.StatsOnly() {
		ingest.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "legacy transcript relay disabled in stats-only mode; upgrade this collector to /v2", http.StatusGone)
		})
	} else {
		compat, e := legacy.New(legacy.Config{Store: s, StateDir: filepath.Join(c.DataDir, "legacy-staging"), Token: c.Token, Version: version, ReserveBytes: 5 << 30, AvailableBytes: func(path string) (int64, error) {
			available, total, e := platform.FreeSpace(path)
			reserve := max(uint64(5<<30), total/20)
			return int64(available) - int64(reserve-(5<<30)), e
		}})
		if e != nil {
			return e
		}
		ingest.Handle("/v1/", compat)
	}
	owner.BaseContext = func(net.Listener) context.Context { return ctx }
	results := make(chan error, 6)
	go func() {
		results <- s.RunProjectDeletions(ctx, func(e error) { logger.Error("project deletion blocked", "error", e) })
	}()
	go func() { results <- owner.Serve(pipe) }()
	go func() {
		err := x.Run(ctx)
		if err != nil {
			err = fmt.Errorf("indexing worker: %w", err)
		}
		results <- err
	}()
	go func() {
		err := accounting.RunReporting(ctx, s, func(e error) { logger.Warn("pricing worker waiting for database writer", "error", e) })
		if err != nil {
			err = fmt.Errorf("pricing worker: %w", err)
		}
		results <- err
	}()
	go func() { results <- historySampler.Run(ctx) }()
	go func() { results <- serveIngestion(ctx, c.ListenAddress, ingest, logger) }()
	finished := 0
	progressTick := time.NewTicker(30 * time.Second)
	defer progressTick.Stop()
	lastProgress := func() {
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		p, e := s.IndexingProgress(c)
		if e == nil && !p.LastProgress.IsZero() {
			if e = journal.NoteProgress(p.LastProgress); e != nil {
				logger.Error("runtime progress journal blocked", "error", e)
			}
		}
	}
	defer lastProgress()
roleLoop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break roleLoop
		case err = <-results:
			finished++
			break roleLoop
		case <-progressTick.C:
			lastProgress()
		}
	}
	cancel()
	_ = worker.Close()
	stop, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if e := owner.Shutdown(stop); e != nil {
		_ = owner.Close()
	}
	for finished < cap(results) {
		select {
		case <-results:
			finished++
		case <-stop.Done():
			return errors.New("hub shutdown timed out before all workers stopped")
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func serveIngestion(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	if address == "" {
		return errors.New("hub needs an explicit ingestion listenAddress")
	}
	for {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			logger.Warn("ingestion blocked", "reason", "interface unavailable", "address", address)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(30 * time.Second):
				continue
			}
		}
		server := hub.Server(handler)
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
			}
		}()
		logger.Info("ingestion listening", "address", address)
		return server.Serve(listener)
	}
}
func runCollector(ctx context.Context, c platform.Config, logger *slog.Logger, journal *runtimeinfo.Journal) error {
	debug.SetMemoryLimit(96 << 20)
	roots := []collector.Root{}
	for _, root := range c.Sources {
		roots = append(roots, collector.Root{Path: root.Path, Provider: root.Provider})
	}
	client, err := collector.Open(collector.Config{DataDir: c.DataDir, MachineID: c.MachineID, Name: c.MachineName, Version: version, HubURL: c.HubURL, Token: c.Token, Roots: roots, SpoolMaxBytes: c.SpoolMaxBytes, BytesPerSecond: c.BytesPerSecond})
	if err != nil {
		return err
	}
	defer client.Close()
	lastProgress := func() {
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		p, e := client.Status(c)
		if e != nil {
			return
		}
		at := time.Time{}
		if p.LastCaptureAt != nil {
			at = *p.LastCaptureAt
		}
		if p.LastUploadAt != nil && p.LastUploadAt.After(at) {
			at = *p.LastUploadAt
		}
		if !at.IsZero() {
			if e = journal.NoteProgress(at); e != nil {
				logger.Error("runtime progress journal blocked", "error", e)
			}
		}
	}
	defer lastProgress()
	result := make(chan error, 1)
	go func() { result <- client.Run(ctx) }()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case e := <-result:
			return e
		case <-tick.C:
			lastProgress()
		}
	}
}
func runParserWorker(args []string) error {
	flags := flag.NewFlagSet("parser-worker", flag.ContinueOnError)
	dir := flags.String("data-dir", "", "hub data directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*dir) {
		return errors.New("parser requires explicit absolute data directory")
	}
	debug.SetMemoryLimit(96 << 20)
	workerOptions := hubStoreOptions()
	workerOptions.ExternalCheckpointOwner = true
	s, err := store.Open(*dir, workerOptions)
	if err != nil {
		return err
	}
	defer s.Close()
	x := &indexer.Indexer{Store: s}
	decoder := indexer.NewWorkerDecoder(os.Stdin, indexer.MaxWorkerRequestBytes)
	for {
		var request indexer.WorkRequest
		if err = decoder.Decode(&request); err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		var count int64
		var sourceResults []indexer.SourceResult
		var prepared *indexer.Preparation
		var e error
		if request.Prepare {
			e = request.ValidatePreparation()
			if e == nil {
				var value indexer.Preparation
				if request.Revision != "" {
					value, e = x.PrepareRebuild(ctx, request.Revision)
				} else {
					value, e = x.PrepareOnce(ctx, request.SourceID, request.Generation)
				}
				if e == nil {
					prepared = &value
				}
			}
		} else if len(request.Sources) > 0 {
			sourceResults, e = x.GroupOnce(ctx, request.Sources)
		} else if request.Revision != "" {
			count, e = x.RebuildOnce(ctx, request.Revision)
		} else {
			count, e = x.Once(ctx, request.SourceID, request.Generation)
		}
		cancel()
		result := indexer.WorkResult{Records: count, Sources: sourceResults, Prepared: prepared}
		if e != nil {
			result.Error = e.Error()
		}
		if err = indexer.WriteWorkerResult(os.Stdout, result); err != nil {
			return err
		}
	}
}
func pipeClient(name string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return platform.DialOwnerPipe(ctx, name) }}}
}
func runDesktop(ctx context.Context, c platform.Config) error {
	if err := platform.ValidateDesktopIdentity(c.OwnerSID); err != nil {
		return err
	}
	listenIP, listenPort, e := net.SplitHostPort(c.ListenAddress)
	if e != nil || listenPort == "" || net.ParseIP(listenIP) == nil || !net.ParseIP(listenIP).IsLoopback() {
		return errors.New("desktop requires an explicit loopback listenAddress")
	}
	ownerHome := c.OwnerHomeDir
	if ownerHome == "" {
		var err error
		ownerHome, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	csrf := hex.EncodeToString(random[:])
	measurementClient := pipeClient(c.PipeName)
	defer measurementClient.CloseIdleConnections()
	manager, err := desktop.New(desktop.Options{LocalMachineID: c.MachineID, StateDir: c.DataDir, HomeDir: ownerHome, Roots: c.RepositoryRoots, CSRFToken: csrf, RemeasureDirective: func(ctx context.Context, body string) (desktop.Measurement, error) {
		return desktop.RemeasureFromHub(ctx, measurementClient, "http://amc/api/v2/analytics/economics/measurement", body)
	}})
	if err != nil {
		return err
	}
	defer manager.Close()
	target, _ := url.Parse("http://amc")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = measurementClient.Transport
	mux := http.NewServeMux()
	manager.Register(mux)
	mux.HandleFunc("GET /api/v2/bootstrap", bootstrapHandler(measurementClient, csrf))
	mux.Handle("/api/v2/", proxy)
	assets.RegisterPreview(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<!doctype html><title>AMC backend candidate</title><main><h1>AMC backend candidate</h1><p>This isolated backend is under acceptance testing. The existing dashboard remains authoritative.</p><p><a href="/preview/">Dashboard integration preview</a> · <a href="/api/v2/health">Health</a> · <a href="/api/v2/sessions">Sessions</a> · <a href="/api/v2/machines">Machines</a></p></main>`)
	})
	// DNS rebinding and cross-origin reads/writes are rejected before proxying to
	// the owner-only pipe. The network receiver has no route to this component.
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, _, e := net.SplitHostPort(r.RemoteAddr)
		if e != nil || net.ParseIP(remote) == nil || !net.ParseIP(remote).IsLoopback() {
			http.Error(w, "Local desktop access only", 403)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "Invalid local host", 403)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != "http://"+r.Host {
			http.Error(w, "Cross-origin access denied", 403)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Cross-site access denied", 403)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-MC-CSRF")), []byte(csrf)) != 1 {
			http.Error(w, "Save token expired; refresh desktop bootstrap", 403)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
	server := hub.Server(guarded)
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Addr = c.ListenAddress
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
