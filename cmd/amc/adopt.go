package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/collector"
	"github.com/evanchakrin/agent-mission-control/internal/migration"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/platform"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

func adoptionSelection(manifestPath, id string) (protocol.Source, string, error) {
	var source protocol.Source
	f, err := os.Open(manifestPath)
	if err != nil {
		return source, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return source, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<20 {
		return source, "", errors.New("migration manifest must be a regular file no larger than 128 MiB")
	}
	var manifest migration.Result
	d := json.NewDecoder(io.LimitReader(f, (128<<20)+1))
	if err = d.Decode(&manifest); err != nil {
		return source, "", err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return source, "", errors.New("trailing migration manifest data")
	}
	if manifest.Version != 1 || manifest.CompletedAt.IsZero() || id == "" {
		return source, "", errors.New("completed version-1 migration manifest and source ID required")
	}
	found := 0
	for _, candidate := range manifest.Sources {
		if candidate.SourceID == id {
			source = candidate
			found++
		}
	}
	if found != 1 {
		return source, "", errors.New("source ID must select exactly one imported source")
	}
	digest, matches := "", 0
	for _, file := range manifest.Files {
		if !file.Raw || file.Path != source.Path || file.MachineID != source.MachineID || file.Provider != source.Provider {
			continue
		}
		if file.Size != source.Size || file.NativeID != source.NativeID || !file.ModifiedAt.Equal(source.ModifiedAt) ||
			parser.ID("legacy-file", file.MachineID, file.Provider, file.Path) != source.SourceID ||
			parser.ID("legacy-generation", file.ModifiedAt.Format(time.RFC3339Nano), strconv.FormatInt(file.Size, 10)) != source.Generation || source.GenerationSequence != 0 {
			return source, "", errors.New("manifest source and raw file identity disagree")
		}
		digest = file.SHA256
		matches++
	}
	if matches != 1 || digest == "" {
		return source, "", errors.New("source must have exactly one checksummed raw file in the manifest")
	}
	return source, digest, nil
}

// One explicit source per invocation makes interrupted fleet adoption resumable
// without silently guessing remote paths or remapping an existing spool.
func runCollectorAdopt(args []string) error {
	flags := flag.NewFlagSet("collector-adopt", flag.ContinueOnError)
	configPath := flags.String("config", "", "absolute collector configuration")
	manifestPath := flags.String("manifest", "", "absolute completed migration manifest")
	id := flags.String("source-id", "", "imported source ID selected from the manifest")
	path := flags.String("source-path", "", "explicit absolute path on this collector machine")
	jsonMode := flags.Bool("json", false, "machine readable output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*configPath) || !filepath.IsAbs(*manifestPath) || !filepath.IsAbs(*path) {
		return errors.New("collector-adopt requires absolute --config, --manifest and --source-path, plus --source-id")
	}
	cfg, err := platform.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != platform.Collector || cfg.MachineID == "" {
		return errors.New("adoption requires an explicit collector machine identity")
	}
	source, digest, err := adoptionSelection(*manifestPath, *id)
	if err != nil {
		return err
	}
	if source.MachineID != cfg.MachineID {
		return errors.New("imported machine ID differs from collector configuration")
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	lock, err := platform.AcquireInstance(platform.Collector, cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	roots := make([]collector.Root, 0, len(cfg.Sources))
	for _, root := range cfg.Sources {
		roots = append(roots, collector.Root{Provider: root.Provider, Path: root.Path})
	}
	c, err := collector.Open(collector.Config{DataDir: cfg.DataDir, MachineID: cfg.MachineID, HubURL: cfg.HubURL, Token: cfg.Token, Roots: roots, SpoolMaxBytes: cfg.SpoolMaxBytes, BytesPerSecond: cfg.BytesPerSecond})
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err = c.AdoptImportedSource(ctx, source, *path, digest); err != nil {
		return err
	}
	return output(*jsonMode, map[string]any{"adopted": true, "sourceId": source.SourceID, "machineId": source.MachineID, "importedBytes": source.Size, "note": "Identity adoption committed. Start collection after all selected sources are adopted; hub receipts still require reconciliation."})
}
