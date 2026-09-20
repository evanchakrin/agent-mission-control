// Package legacy adapts existing outbound v1 relays to the durable v2 ledger.
package legacy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

const maxJSONBytes = 50 << 20

type Config struct {
	Store          *store.Store
	StateDir       string
	Token          string
	Version        string
	ReserveBytes   int64
	AvailableBytes func(string) (int64, error)
}
type Handler struct {
	config     Config
	stripes    [256]sync.Mutex
	largeJSON  chan struct{}
	rawStreams chan struct{}
}

func New(c Config) (*Handler, error) {
	if c.Store == nil || c.StateDir == "" || c.Token == "" {
		return nil, fmt.Errorf("legacy Store, StateDir and Token are required")
	}
	abs, err := filepath.Abs(c.StateDir)
	if err != nil {
		return nil, err
	}
	c.StateDir = abs
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	if c.Version == "" {
		c.Version = "8.0.0"
	}
	if c.ReserveBytes <= 0 {
		c.ReserveBytes = 5 << 30
	}
	if c.AvailableBytes == nil {
		c.AvailableBytes = freeBytes
	}
	return &Handler{config: c, largeJSON: make(chan struct{}, 1), rawStreams: make(chan struct{}, 2)}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-relay-token")), []byte(h.config.Token)) != 1 {
		respond(w, 401, map[string]any{"error": "bad or missing x-relay-token"})
		return
	}
	switch {
	case r.URL.Path == "/v1/boot" && r.Method == http.MethodGet:
		machine := r.Header.Get("x-relay-machine")
		if machine != "" {
			if !validMachine(machine) {
				respond(w, 400, map[string]any{"error": "bad machine"})
				return
			}
			if err := h.config.Store.RecordHeartbeat(r.Context(), protocol.Heartbeat{MachineID: machine, Name: machine, Version: r.Header.Get("x-relay-version"), State: "legacy-connected"}); err != nil {
				failure(w, err)
				return
			}
		}
		respond(w, 200, map[string]any{"boot": h.config.Store.RecoveryEpoch(), "version": h.config.Version, "archiveCreateOnly": true})
	case r.URL.Path == "/v1/relay/append" && r.Method == http.MethodPost:
		h.append(w, r)
	case (r.URL.Path == "/v1/archive/raw" || r.URL.Path == "/v1/archive/create") && r.Method == http.MethodPost:
		h.archiveRaw(w, r)
	case r.URL.Path == "/v1/archive/manifest" && r.Method == http.MethodGet:
		machine := r.URL.Query().Get("machine")
		if !validMachine(machine) {
			respond(w, 400, map[string]any{"error": "bad machine"})
			return
		}
		manifest, err := h.config.Store.LegacyManifest(r.Context(), machine)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"manifest": manifest})
	case r.URL.Path == "/v1/relay" && r.Method == http.MethodPost:
		h.parsed(w, r)
	case r.URL.Path == "/v1/archive" && r.Method == http.MethodPost:
		h.archiveJSON(w, r)
	case r.URL.Path == "/v1/traces" && r.Method == http.MethodPost:
		h.traces(w, r)
	default:
		respond(w, 404, map[string]any{"error": "unknown legacy route"})
	}
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
func failure(w http.ResponseWriter, err error) {
	status := 500
	if errors.Is(err, store.ErrInvalid) {
		status = 400
	} else if errors.Is(err, store.ErrConflict) {
		status = 409
	} else if errors.Is(err, store.ErrCapacity) {
		status = 507
	}
	respond(w, status, map[string]any{"error": err.Error()})
}
func validMachine(s string) bool {
	return s != "" && len(s) <= 128 && !strings.ContainsAny(s, "/\\:\x00\r\n")
}
func cleanPath(raw string) (string, error) {
	p, err := url.PathUnescape(raw)
	if err != nil {
		return "", store.ErrInvalid
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if len(p) == 0 || len(p) > 2048 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":\x00\r\n") || !strings.HasSuffix(p, ".jsonl") {
		return "", store.ErrInvalid
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", store.ErrInvalid
		}
	}
	return p, nil
}
func providerOf(p string) string {
	if strings.HasPrefix(p, "codex/") {
		return "codex"
	}
	return "claude"
}
func native(p, provider string) string {
	b := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if provider == "codex" && strings.HasPrefix(b, "rollout-") && len(b) >= 36 {
		return b[len(b)-36:]
	}
	return strings.TrimPrefix(b, "codex:")
}
func legacyKey(machine, provider, id string) string {
	return "R:" + escape(machine) + ":" + provider + ":" + escape(id)
}
func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func (h *Handler) gate(machine, path string) *sync.Mutex {
	hash := fnv.New32a()
	hash.Write([]byte(machine + "\x00" + path))
	return &h.stripes[hash.Sum32()%uint32(len(h.stripes))]
}
func newGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (h *Handler) resolve(ctx context.Context, machine, path, provider, nativeID string) (protocol.Source, int64, error) {
	if nativeID == "" {
		nativeID = native(path, provider)
	}
	id := parser.ID("legacy-source", machine, provider, path)
	src, err := h.config.Store.LegacySource(ctx, id)
	if err == nil {
		st, e := h.config.Store.SourceState(ctx, src.SourceID, src.Generation)
		if errors.Is(e, store.ErrNotFound) {
			return src, 0, nil
		}
		return src, st.DurableOffset, e
	}
	if !errors.Is(err, store.ErrNotFound) {
		return src, 0, err
	}
	// Migration's alias table permits adoption without merging unrelated native
	// IDs opportunistically. An unverified summary gets a fresh raw generation.
	alias := legacyKey(machine, provider, nativeID)
	if sessionID, e := h.config.Store.ResolveAlias(ctx, alias); e == nil {
		if sess, e := h.config.Store.GetSession(ctx, sessionID); e == nil && sess.MachineID == machine && sess.Provider == provider {
			st, e := h.config.Store.CurrentSource(ctx, sess.SourceID)
			if e == nil {
				src = st.Source
				src.Path = path
				if strings.Contains(sess.Completeness, "cache") || strings.Contains(sess.Completeness, "legacy-summary") {
					src.Generation = newGeneration()
					src.GenerationSequence++
					st.DurableOffset = 0
				}
				// The lookup reservation retains the deterministic v1 route identity
				// through a durable pointer, even if the adopted source ID differs.
				if err = h.config.Store.RememberLegacyRoute(ctx, id, src); err != nil {
					return src, 0, err
				}
				return src, st.DurableOffset, nil
			}
		}
	}
	src = protocol.Source{MachineID: machine, SourceID: id, Generation: newGeneration(), GenerationSequence: 1, Provider: provider, NativeID: nativeID, Path: path, ModifiedAt: time.Now().UTC()}
	if err = h.config.Store.RememberLegacySource(ctx, src); err != nil {
		return src, 0, err
	}
	return src, 0, nil
}
func (h *Handler) rotate(ctx context.Context, src protocol.Source) (protocol.Source, error) {
	src.Generation = newGeneration()
	src.GenerationSequence++
	src.Size = 0
	src.ModifiedAt = time.Now().UTC()
	return src, h.config.Store.RememberLegacySource(ctx, src)
}

func (h *Handler) append(w http.ResponseWriter, r *http.Request) {
	select {
	case h.rawStreams <- struct{}{}:
		defer func() { <-h.rawStreams }()
	default:
		respond(w, 503, map[string]any{"error": "legacy streaming ingestion busy", "retryable": true})
		return
	}
	machine := r.Header.Get("x-relay-machine")
	path, err := cleanPath(r.Header.Get("x-relay-path"))
	off, e1 := strconv.ParseInt(r.Header.Get("x-relay-offset"), 10, 64)
	declared, e2 := strconv.ParseInt(r.Header.Get("x-relay-bytes"), 10, 64)
	if !validMachine(machine) || err != nil || e1 != nil || e2 != nil || off < 0 || declared < 0 || declared > (1<<63-1)-off {
		respond(w, 400, map[string]any{"error": "invalid append headers"})
		return
	}
	if len(r.Header.Get("x-relay-meta")) > 12000 {
		respond(w, 431, map[string]any{"error": "metadata too large"})
		return
	}
	var meta struct {
		File string `json:"file"`
	}
	var metadataPayload []byte
	if raw := r.Header.Get("x-relay-meta"); raw != "" {
		decoded, e := url.PathUnescape(raw)
		if e != nil || json.Unmarshal([]byte(decoded), &meta) != nil || "claude/"+strings.ReplaceAll(meta.File, "\\", "/") != path {
			respond(w, 400, map[string]any{"error": "metadata does not describe append path"})
			return
		}
		metadataPayload = []byte(decoded)
	}
	lock := h.gate(machine, path)
	if !lock.TryLock() {
		respond(w, 503, map[string]any{"error": "source busy", "retryable": true})
		return
	}
	defer lock.Unlock()
	src, size, err := h.resolve(r.Context(), machine, path, providerOf(path), "")
	if err != nil {
		failure(w, err)
		return
	}
	if off != size && off != 0 {
		h.offsetConflict(w, size, "offset mismatch")
		return
	}
	if off == 0 && size > 0 && size <= declared {
		h.offsetConflict(w, size, "have prefix")
		return
	}
	if off > 0 && r.Header.Get("x-relay-anchor") != "" {
		n := int64(64)
		if n > off {
			n = off
		}
		stream, e := h.config.Store.OpenSource(r.Context(), src.SourceID, src.Generation, off-n)
		if e != nil {
			failure(w, e)
			return
		}
		buf := make([]byte, n)
		_, e = io.ReadFull(stream, buf)
		stream.Close()
		anchor := sha1.Sum(buf)
		if e != nil || hex.EncodeToString(anchor[:]) != r.Header.Get("x-relay-anchor") {
			if _, e = h.rotate(r.Context(), src); e != nil {
				failure(w, e)
				return
			}
			h.offsetConflict(w, 0, "anchor mismatch; prior generation preserved")
			return
		}
	}
	if off == 0 && size > 0 {
		src, err = h.rotate(r.Context(), src)
		if err != nil {
			failure(w, err)
			return
		}
	}
	src.Size = off + declared
	src.ModifiedAt = time.Now().UTC()
	if err = h.config.Store.MarkLegacyDelta(r.Context(), src.SourceID); err != nil {
		failure(w, err)
		return
	}
	if err = h.config.Store.SetExternalIndex(r.Context(), src.SourceID, false); err != nil {
		failure(w, err)
		return
	}
	if err = h.storeStream(r.Context(), src, off, declared, r.Body); err != nil {
		failure(w, err)
		return
	}
	if err = h.config.Store.RememberLegacySource(r.Context(), src); err != nil {
		failure(w, err)
		return
	}
	if len(metadataPayload) > 0 {
		if err = h.config.Store.RememberLegacyEnvelope(r.Context(), src.SourceID, src.Generation, off+declared, metadataPayload); err != nil {
			failure(w, err)
			return
		}
	}
	if err = h.alias(r.Context(), src); err != nil {
		failure(w, err)
		return
	}
	h.recordMachine(r.Context(), machine)
	respond(w, 200, map[string]any{"ok": true, "size": off + declared, "boot": h.config.Store.RecoveryEpoch()})
}
func (h *Handler) offsetConflict(w http.ResponseWriter, size int64, message string) {
	respond(w, 409, map[string]any{"error": message, "size": size, "boot": h.config.Store.RecoveryEpoch()})
}
func (h *Handler) alias(ctx context.Context, src protocol.Source) error {
	return h.config.Store.PutLegacyAlias(ctx, legacyKey(src.MachineID, src.Provider, src.NativeID), parser.SessionID(src))
}
func (h *Handler) recordMachine(ctx context.Context, machine string) {
	_ = h.config.Store.TouchLegacyMachine(ctx, machine)
}

func (h *Handler) storeStream(ctx context.Context, src protocol.Source, off, length int64, input io.Reader) error {
	buf := make([]byte, protocol.MaxChunkBytes)
	left := length
	for left > 0 {
		nmax := int64(len(buf))
		if nmax > left {
			nmax = left
		}
		n, err := io.ReadFull(input, buf[:nmax])
		if err != nil {
			return fmt.Errorf("%w: incomplete request body", store.ErrInvalid)
		}
		data := buf[:n]
		hash := sha256.Sum256(data)
		if _, err = h.config.Store.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: off, Length: int64(n), SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(data)); err != nil {
			return err
		}
		off += int64(n)
		left -= int64(n)
	}
	var extra [1]byte
	n, err := input.Read(extra[:])
	if n > 0 {
		return fmt.Errorf("%w: body exceeds declaration", store.ErrInvalid)
	}
	if err != nil && err != io.EOF {
		return err
	}
	if length == 0 {
		hash := sha256.Sum256(nil)
		_, err = h.config.Store.IngestChunk(ctx, protocol.Chunk{Source: src, Offset: off, SHA256: hex.EncodeToString(hash[:])}, bytes.NewReader(nil))
		return err
	}
	return nil
}

func (h *Handler) archiveRaw(w http.ResponseWriter, r *http.Request) {
	select {
	case h.rawStreams <- struct{}{}:
		defer func() { <-h.rawStreams }()
	default:
		respond(w, 503, map[string]any{"error": "legacy streaming ingestion busy", "retryable": true})
		return
	}
	machine := r.Header.Get("x-archive-machine")
	createOnly := r.URL.Path == "/v1/archive/create" || r.Header.Get("If-None-Match") == "*"
	if v := r.Header.Get("If-None-Match"); v != "" && v != "*" {
		respond(w, 400, map[string]any{"error": "unsupported archive precondition"})
		return
	}
	path, err := cleanPath(r.Header.Get("x-archive-path"))
	if !validMachine(machine) || err != nil {
		respond(w, 400, map[string]any{"error": "invalid archive identity"})
		return
	}
	// Legacy raw uploads have no length header. Spool through a fixed buffer so
	// even terabyte input cannot create a whole-file allocation.
	tmp, err := os.CreateTemp(h.config.StateDir, "archive-*")
	if err != nil {
		failure(w, err)
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	n, err := h.spool(r.Context(), tmp, r.Body)
	if err == nil {
		err = tmp.Sync()
	}
	tmp.Close()
	if err != nil {
		failure(w, err)
		return
	}
	in, err := os.Open(name)
	if err != nil {
		failure(w, err)
		return
	}
	defer in.Close()
	if err = h.archiveConditional(r.Context(), machine, path, n, in, createOnly); err != nil {
		if errors.Is(err, errArchiveExists) {
			respond(w, http.StatusPreconditionFailed, map[string]any{"error": "archive source already exists"})
			return
		}
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"ok": true})
}

var errArchiveExists = errors.New("archive source already exists")

func (h *Handler) archive(ctx context.Context, machine, path string, size int64, input io.Reader) error {
	return h.archiveConditional(ctx, machine, path, size, input, false)
}
func (h *Handler) archiveConditional(ctx context.Context, machine, path string, size int64, input io.Reader, createOnly bool) error {
	lock := h.gate(machine, path)
	if !lock.TryLock() {
		return fmt.Errorf("%w: source busy", store.ErrConflict)
	}
	defer lock.Unlock()
	src, prior, err := h.resolve(ctx, machine, path, providerOf(path), "")
	if err != nil {
		return err
	}
	// Check while holding the same source gate as append and ordinary archive.
	// A recovery snapshot must never rotate evidence accepted by another sender.
	if createOnly {
		_, e := h.config.Store.SourceState(ctx, src.SourceID, src.Generation)
		if e == nil {
			return errArchiveExists
		}
		if !errors.Is(e, store.ErrNotFound) {
			return e
		}
	}
	// The live Claude append route owns a published mirror; a snapshot must not
	// replace an independently progressing stream. Existing relays understand409.
	owned, err := h.config.Store.LegacyDeltaOwned(ctx, src.SourceID)
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("%w: delta-owned mirror", store.ErrConflict)
	}
	if prior > 0 {
		src, err = h.rotate(ctx, src)
		if err != nil {
			return err
		}
	}
	src.Size = size
	src.ModifiedAt = time.Now().UTC()
	if err = h.config.Store.SetExternalIndex(ctx, src.SourceID, false); err != nil {
		return err
	}
	if err = h.storeStream(ctx, src, 0, size, input); err != nil {
		return err
	}
	if err = h.config.Store.RememberLegacySource(ctx, src); err != nil {
		return err
	}
	return h.alias(ctx, src)
}
func (h *Handler) readJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	select {
	case h.largeJSON <- struct{}{}:
	default:
		respond(w, 503, map[string]any{"error": "legacy JSON ingestion busy", "retryable": true})
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		<-h.largeJSON
		respond(w, 413, map[string]any{"error": "legacy JSON exceeds request limit or body incomplete"})
		return nil, false
	}
	return b, true
}
func (h *Handler) archiveJSON(w http.ResponseWriter, r *http.Request) {
	b, ok := h.readJSON(w, r)
	if !ok {
		return
	}
	defer func() { <-h.largeJSON }()
	var body struct {
		Machine string `json:"machine"`
		Path    string `json:"relPath"`
		Data    string `json:"data"`
	}
	if json.Unmarshal(b, &body) != nil || !validMachine(body.Machine) {
		respond(w, 400, map[string]any{"error": "invalid archive JSON"})
		return
	}
	path, err := cleanPath(body.Path)
	if err != nil {
		failure(w, err)
		return
	}
	// Decode to disk: base64 string is bounded by v1's JSON transport; a second
	// decoded whole-file byte allocation is unnecessary.
	tmp, err := os.CreateTemp(h.config.StateDir, "base64-*")
	if err != nil {
		failure(w, err)
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	n, err := h.spool(r.Context(), tmp, base64.NewDecoder(base64.StdEncoding, strings.NewReader(body.Data)))
	tmp.Close()
	if err != nil {
		var badBase64 base64.CorruptInputError
		if errors.As(err, &badBase64) {
			err = fmt.Errorf("%w: invalid base64 payload", store.ErrInvalid)
		}
		failure(w, err)
		return
	}
	in, err := os.Open(name)
	if err != nil {
		failure(w, err)
		return
	}
	defer in.Close()
	if err = h.archive(r.Context(), body.Machine, path, n, in); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"ok": true})
}

func (h *Handler) spool(ctx context.Context, out io.Writer, input io.Reader) (int64, error) {
	buf := make([]byte, protocol.MaxChunkBytes)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		free, err := h.config.AvailableBytes(h.config.StateDir)
		if err != nil {
			return total, err
		}
		if free < h.config.ReserveBytes+int64(len(buf)) {
			return total, store.ErrCapacity
		}
		n, re := input.Read(buf)
		if n > 0 {
			written, err := out.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if re == io.EOF {
			return total, nil
		}
		if re != nil {
			return total, re
		}
	}
}
