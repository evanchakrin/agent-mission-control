// Package migration imports legacy history into an explicitly separate store.
// Discovery and import never modify or delete legacy files or source transcripts.
package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type Root struct {
	Path      string `json:"path"`
	Provider  string `json:"provider"`
	MachineID string `json:"machineId"`
}
type Options struct {
	LegacyStateDir string
	Destination    string
	LocalRoots     []Root
	ReserveBytes   int64
	AvailableBytes func(string) (int64, error)
}
type File struct {
	Path       string    `json:"path"`
	Relative   string    `json:"relative"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Raw        bool      `json:"raw"`
	Provider   string    `json:"provider,omitempty"`
	MachineID  string    `json:"machineId,omitempty"`
	NativeID   string    `json:"nativeId,omitempty"`
	LegacyKey  string    `json:"legacyKey,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
}
type Plan struct {
	Source             string `json:"source"`
	Destination        string `json:"destination"`
	Files              []File `json:"files"`
	SourceBytes        int64  `json:"sourceBytes"`
	RawBytes           int64  `json:"rawBytes"`
	AssetBytes         int64  `json:"assetBytes"`
	RequiredFreeBytes  int64  `json:"requiredFreeBytes"`
	AvailableFreeBytes int64  `json:"availableFreeBytes"`
	ReserveBytes       int64  `json:"reserveBytes"`
	MachineID          string `json:"machineId"`
	availableBytes     func(string) (int64, error)
}
type Result struct {
	Version           int               `json:"version"`
	CompletedAt       time.Time         `json:"completedAt"`
	Destination       string            `json:"destination"`
	RawSources        int64             `json:"rawSources"`
	CacheOnlySessions int64             `json:"cacheOnlySessions"`
	MetadataEntries   int64             `json:"metadataEntries"`
	Aliases           map[string]string `json:"aliases"`
	Sources           []protocol.Source `json:"sources"`
	Files             []File            `json:"files"`
	Warnings          []string          `json:"warnings"`
}

func contained(root, target string) bool {
	r, err := filepath.Rel(root, target)
	return err == nil && (r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))))
}
func checkDirectory(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a regular directory: %s", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}
func destinationPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(abs); err == nil {
		return "", fmt.Errorf("destination already exists: %s", abs)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := checkDirectory(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("destination parent must already exist: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// Preflight assumes no compression savings and reserves an additional raw-sized
// allocation for indexes, plus 64 MiB for transactional/temp work. The old store
// already occupies disk and remains untouched; RequiredFreeBytes is additional
// capacity, not total volume capacity.
func Preflight(ctx context.Context, options Options) (Plan, error) {
	var p Plan
	var err error
	p.Source, err = checkDirectory(options.LegacyStateDir)
	if err != nil {
		return p, err
	}
	p.Destination, err = destinationPath(options.Destination)
	if err != nil {
		return p, err
	}
	if contained(p.Source, p.Destination) || contained(p.Destination, p.Source) {
		return p, fmt.Errorf("destination and legacy source overlap")
	}
	p.ReserveBytes = options.ReserveBytes
	if p.ReserveBytes <= 0 {
		p.ReserveBytes = 5 << 30
	}
	p.availableBytes = options.AvailableBytes
	if p.availableBytes == nil {
		p.availableBytes = freeBytes
	}
	statePath := filepath.Join(p.Source, "state.json")
	if b, readErr := readSmall(statePath, 64<<20); readErr == nil {
		var state struct {
			MachineID string `json:"machineId"`
		}
		if json.Unmarshal(b, &state) != nil {
			return p, fmt.Errorf("legacy state.json is corrupt")
		}
		p.MachineID = state.MachineID
	} else if !os.IsNotExist(readErr) {
		return p, readErr
	}
	if p.MachineID == "" {
		p.MachineID = parser.ID("legacy-local", p.Source)
	}
	walk := func(root string, local *Root, index int) error {
		return filepath.WalkDir(root, func(path string, e fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink requires explicit migration root: %s", path)
			}
			if e.IsDir() {
				return nil
			}
			if !e.Type().IsRegular() {
				return fmt.Errorf("nonregular source file: %s", path)
			}
			st, err := e.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			f := File{Path: path, Relative: rel, Size: st.Size(), ModifiedAt: st.ModTime().UTC()}
			parts := strings.Split(rel, "/")
			if local != nil {
				if !strings.HasSuffix(strings.ToLower(rel), ".jsonl") && !strings.HasSuffix(strings.ToLower(rel), ".json") {
					return nil
				}
				f.Relative = fmt.Sprintf("local/%d/%s", index, rel)
				f.Raw = strings.HasSuffix(strings.ToLower(rel), ".jsonl")
				f.Provider = local.Provider
				f.MachineID = local.MachineID
				if f.MachineID == "" {
					f.MachineID = p.MachineID
				}
			} else if len(parts) >= 4 && parts[0] == "archive" && (parts[2] == "claude" || parts[2] == "codex") && strings.HasSuffix(strings.ToLower(rel), ".jsonl") {
				f.Raw = true
				f.MachineID = parts[1]
				f.Provider = parts[2]
			}
			if f.Raw {
				f.NativeID = nativeID(f.Path, f.Provider)
				prefix := "R:"
				if local != nil {
					prefix = "L:"
				}
				f.LegacyKey = prefix + encode(f.MachineID) + ":" + f.Provider + ":" + encode(f.NativeID)
				p.RawBytes += f.Size
			} else {
				p.AssetBytes += f.Size
			}
			p.SourceBytes += f.Size
			p.Files = append(p.Files, f)
			return nil
		})
	}
	if err = walk(p.Source, nil, 0); err != nil {
		return p, err
	}
	seen := []string{p.Source}
	for i, r := range options.LocalRoots {
		if r.Provider != "claude" && r.Provider != "codex" {
			return p, fmt.Errorf("unsupported local provider %q", r.Provider)
		}
		root, err := checkDirectory(r.Path)
		if err != nil {
			return p, err
		}
		if contained(root, p.Destination) || contained(p.Destination, root) {
			return p, fmt.Errorf("destination overlaps transcript root")
		}
		for _, prior := range seen {
			if contained(prior, root) || contained(root, prior) {
				return p, fmt.Errorf("overlapping migration roots: %s", root)
			}
		}
		seen = append(seen, root)
		if err = walk(root, &r, i); err != nil {
			return p, err
		}
	}
	if p.RawBytes > (1<<63-1-p.AssetBytes-p.ReserveBytes-(64<<20))/2 {
		return p, fmt.Errorf("migration size overflow")
	}
	p.RequiredFreeBytes = 2*p.RawBytes + p.AssetBytes + p.ReserveBytes + (64 << 20)
	p.AvailableFreeBytes, err = p.availableBytes(filepath.Dir(p.Destination))
	if err != nil {
		return p, err
	}
	if p.AvailableFreeBytes < p.RequiredFreeBytes {
		return p, fmt.Errorf("migration needs %d free bytes; only %d available", p.RequiredFreeBytes, p.AvailableFreeBytes)
	}
	return p, nil
}

func encode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func nativeID(path, provider string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if provider == "codex" && strings.HasPrefix(base, "rollout-") && len(base) >= 36 {
		return base[len(base)-36:]
	}
	return base
}
func sourceFor(f File) protocol.Source {
	return protocol.Source{MachineID: f.MachineID, SourceID: parser.ID("legacy-file", f.MachineID, f.Provider, f.Path), Generation: parser.ID("legacy-generation", f.ModifiedAt.Format(time.RFC3339Nano), strconv.FormatInt(f.Size, 10)), Provider: f.Provider, NativeID: f.NativeID, Path: f.Path, Size: f.Size, ModifiedAt: f.ModifiedAt}
}

// Run imports only a preflighted snapshot into a new destination. Changed input
// files abort the run with the incomplete destination preserved for diagnosis;
// an incomplete migration never receives migration-manifest.json.
func Run(ctx context.Context, p Plan, options store.Options) (Result, error) {
	r := Result{Version: 1, Destination: p.Destination, Aliases: map[string]string{}, Warnings: []string{}}
	if p.Source == "" || p.Destination == "" || contained(p.Source, p.Destination) || contained(p.Destination, p.Source) {
		return r, fmt.Errorf("invalid migration plan")
	}
	if _, err := destinationPath(p.Destination); err != nil {
		return r, err
	}
	available := p.availableBytes
	if available == nil {
		available = freeBytes
	}
	n, err := available(filepath.Dir(p.Destination))
	if err != nil {
		return r, err
	}
	if n < p.RequiredFreeBytes {
		return r, fmt.Errorf("migration capacity changed since preflight")
	}
	if options.ReserveBytes <= 0 {
		options.ReserveBytes = p.ReserveBytes
	}
	if options.AvailableBytes == nil {
		options.AvailableBytes = available
	}
	s, err := store.Open(p.Destination, options)
	if err != nil {
		return r, err
	}
	defer s.Close()
	for _, f := range p.Files {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		if err = unchanged(f); err != nil {
			return r, err
		}
		if f.Raw {
			src := sourceFor(f)
			id := parser.SessionID(src)
			if prior := r.Aliases[f.LegacyKey]; prior != "" && prior != id {
				return r, fmt.Errorf("ambiguous legacy identity %s; raw copies must be reconciled explicitly", f.LegacyKey)
			}
			f.SHA256, err = importRaw(ctx, s, src, f)
			if err != nil {
				return r, err
			}
			if err = s.CommitIndex(ctx, store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, Session: store.Session{ID: id, Title: f.NativeID, NativeID: f.NativeID, LastActivity: f.ModifiedAt, Completeness: "raw-captured-pending-index"}}); err != nil {
				return r, err
			}
			r.Aliases[f.LegacyKey] = id
			if strings.HasPrefix(f.Relative, "archive/") {
				parts := strings.SplitN(f.Relative, "/", 3)
				if len(parts) == 3 {
					compat := src
					compat.Path = parts[2]
					if err = s.RememberLegacyRoute(ctx, parser.ID("legacy-source", src.MachineID, src.Provider, compat.Path), compat); err != nil {
						return r, err
					}
				}
			}
			r.Sources = append(r.Sources, src)
			r.RawSources++
		} else {
			dest := filepath.Join(p.Destination, "legacy-assets", filepath.FromSlash(f.Relative))
			if !contained(filepath.Join(p.Destination, "legacy-assets"), dest) {
				return r, fmt.Errorf("unsafe asset path")
			}
			f.SHA256, err = copyAsset(ctx, f.Path, dest)
			if err != nil {
				return r, err
			}
		}
		if err = unchanged(f); err != nil {
			return r, err
		}
		r.Files = append(r.Files, f)
	}
	// Relay caches without raw evidence stay visible but explicitly incomplete.
	for _, f := range p.Files {
		if f.Raw || !strings.HasPrefix(f.Relative, "relay/") || !strings.HasSuffix(f.Relative, ".json") {
			continue
		}
		if err = importCache(ctx, s, f, &r); err != nil {
			return r, err
		}
	}
	statePath := filepath.Join(p.Destination, "legacy-assets", "state.json")
	if b, readErr := readSmall(statePath, 64<<20); readErr == nil {
		var state struct {
			Sessions     map[string]json.RawMessage `json:"sessions"`
			Projects     []store.LegacyProject      `json:"projects"`
			MachineNames map[string]string          `json:"machineNames"`
		}
		if err = json.Unmarshal(b, &state); err != nil {
			return r, err
		}
		if err = s.ImportLegacyProjects(ctx, state.Projects); err != nil {
			return r, err
		}
		if err = s.ImportLegacyMachineLabels(ctx, state.MachineNames); err != nil {
			return r, err
		}
		if err = s.ImportLegacyMetadata(ctx, r.Aliases, state.Sessions); err != nil {
			return r, err
		}
		r.MetadataEntries = int64(len(state.Sessions))
		for key := range state.Sessions {
			if r.Aliases[key] == "" {
				r.Warnings = append(r.Warnings, "Metadata preserved without matching source: "+key)
			}
		}
	} else if !os.IsNotExist(readErr) {
		return r, readErr
	}
	r.CompletedAt = time.Now().UTC()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return r, err
	}
	if err = writeNew(filepath.Join(p.Destination, "migration-manifest.json"), b); err != nil {
		return r, err
	}
	return r, nil
}

func unchanged(f File) error {
	st, err := os.Lstat(f.Path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != f.Size || !st.ModTime().Equal(f.ModifiedAt) {
		return fmt.Errorf("source changed during shadow migration: %s", f.Path)
	}
	return nil
}
func importRaw(ctx context.Context, s *store.Store, src protocol.Source, f File) (string, error) {
	in, err := os.Open(f.Path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	whole := sha256.New()
	buf := make([]byte, protocol.MaxChunkBytes)
	off := int64(0)
	for off < f.Size {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		nmax := int64(len(buf))
		if nmax > f.Size-off {
			nmax = f.Size - off
		}
		n, err := io.ReadFull(in, buf[:nmax])
		if err != nil {
			return "", err
		}
		data := buf[:n]
		whole.Write(data)
		h := sha256.Sum256(data)
		_, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: off, Length: int64(n), SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		off += int64(n)
	}
	if f.Size == 0 {
		h := sha256.Sum256(nil)
		if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(nil)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(whole.Sum(nil)), nil
}
func importCache(ctx context.Context, s *store.Store, f File, r *Result) error {
	b, err := readSmall(f.Path, 64<<20)
	if err != nil {
		return err
	}
	var rec struct {
		ID      string `json:"id"`
		Machine string `json:"machine"`
		Meta    struct {
			Session string `json:"session"`
			Title   string `json:"title"`
		} `json:"meta"`
		Result struct {
			Agents []struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				In    int64  `json:"inTokens"`
				Cache int64  `json:"cacheTokens"`
				Write int64  `json:"cacheWriteTokens"`
				Out   int64  `json:"outTokens"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err = json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("invalid relay cache %s: %w", f.Path, err)
	}
	if rec.ID == "" || rec.Machine == "" {
		r.Warnings = append(r.Warnings, "Unrecognized cache retained as asset: "+f.Relative)
		return nil
	}
	provider := "claude"
	if strings.Contains(rec.ID, ":codex:") {
		provider = "codex"
	}
	native := strings.TrimPrefix(rec.Meta.Session, "codex:")
	if native == "" {
		native = rec.ID[strings.LastIndex(rec.ID, ":")+1:]
	}
	native = strings.TrimSuffix(filepath.Base(native), ".jsonl")
	key := "R:" + encode(rec.Machine) + ":" + provider + ":" + encode(native)
	if r.Aliases[key] != "" {
		return nil
	}
	src := protocol.Source{MachineID: rec.Machine, SourceID: parser.ID("legacy-cache", rec.ID), Generation: "legacy-cache", Provider: provider, NativeID: native, Path: f.Path, ModifiedAt: f.ModifiedAt}
	h := sha256.Sum256(nil)
	if _, err = s.IngestChunk(ctx, protocol.Chunk{Source: src, SHA256: hex.EncodeToString(h[:])}, bytes.NewReader(nil)); err != nil {
		return err
	}
	id := parser.SessionID(src)
	batch := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, Session: store.Session{ID: id, Title: rec.Meta.Title, NativeID: native, LastActivity: f.ModifiedAt, Completeness: "legacy-cache-only"}}
	for i, a := range rec.Result.Agents {
		// Legacy model attribution may have been scaled from transcript tails;
		// preserve totals as unknown-model evidence, never silently certify it.
		batch.Usage = append(batch.Usage, store.UsageObservation{ID: parser.ID(src.SourceID, strconv.Itoa(i)), AgentID: a.ID, TokensIn: a.In, TokensCache: a.Cache, TokensCacheWrite: a.Write, TokensOut: a.Out, Kind: "legacy-summary", CounterScope: "unverified-legacy-summary", Evidence: json.RawMessage(`{"provenance":"legacy-cache","complete":false}`)})
	}
	if err = s.CommitIndex(ctx, batch); err != nil {
		return err
	}
	r.Aliases[key] = id
	r.CacheOnlySessions++
	r.Warnings = append(r.Warnings, "Raw transcript missing for legacy cache: "+key)
	return nil
}
func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("legacy document exceeds %d-byte validation bound: %s", limit, path)
	}
	return b, nil
}
func copyAsset(ctx context.Context, from, to string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return "", err
	}
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		n, re := in.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if _, err = out.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return "", re
		}
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
