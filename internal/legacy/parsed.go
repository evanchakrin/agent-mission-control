package legacy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type usageBucket struct {
	In    int64 `json:"inTokens"`
	Cache int64 `json:"cacheTokens"`
	Write int64 `json:"cacheWriteTokens"`
	Out   int64 `json:"outTokens"`
}
type parsedEnvelope struct {
	Machine string `json:"machine"`
	File    string `json:"file"`
	Meta    struct {
		Session string          `json:"session"`
		Title   string          `json:"title"`
		Project string          `json:"project"`
		MTime   json.RawMessage `json:"mtime"`
	} `json:"meta"`
	Result *struct {
		Agents []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			usageBucket
			UsageByModel map[string]usageBucket `json:"usageByModel"`
		} `json:"agents"`
		Events []struct {
			TS    string `json:"ts"`
			Agent string `json:"agent"`
			Kind  string `json:"kind"`
			Text  string `json:"text"`
			Full  string `json:"full"`
			Tool  string `json:"tool"`
			Error bool   `json:"error"`
		} `json:"events"`
	} `json:"result"`
}

func bounded(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func (h *Handler) parsed(w http.ResponseWriter, r *http.Request) {
	b, ok := h.readJSON(w, r)
	if !ok {
		return
	}
	defer func() { <-h.largeJSON }()
	var body parsedEnvelope
	if json.Unmarshal(b, &body) != nil || !validMachine(body.Machine) || body.File == "" || body.Result == nil {
		respond(w, 400, map[string]any{"error": "need machine, file, result"})
		return
	}
	provider, path := "claude", "claude/"+strings.ReplaceAll(body.File, "\\", "/")
	nativeID := strings.TrimPrefix(body.Meta.Session, "codex:")
	if strings.HasPrefix(body.File, "codex:") {
		provider = "codex"
		nativeID = strings.TrimPrefix(body.File, "codex:")
		path = "codex/" + nativeID + ".jsonl"
	}
	var err error
	path, err = cleanPath(path)
	if err != nil {
		failure(w, err)
		return
	}
	lock := h.gate(body.Machine, path)
	if !lock.TryLock() {
		respond(w, 503, map[string]any{"error": "source busy", "retryable": true})
		return
	}
	defer lock.Unlock()
	src, prior, err := h.resolve(r.Context(), body.Machine, path, provider, nativeID)
	if err != nil {
		failure(w, err)
		return
	}
	// Every whole parsed POST is a replacement summary snapshot, not appended
	// model usage. Prior snapshots remain raw evidence in older generations.
	digest := sha256.Sum256(b)
	hash := hex.EncodeToString(digest[:])
	oldGeneration, oldHash, snapshotErr := h.config.Store.LegacySnapshot(r.Context(), src.SourceID)
	if snapshotErr != nil && !errors.Is(snapshotErr, store.ErrNotFound) {
		failure(w, snapshotErr)
		return
	}
	// Summary retries may replace summary generations, never source transcripts.
	// Keep this check under the source gate shared with append/archive ingestion.
	deltaOwned, err := h.config.Store.LegacyDeltaOwned(r.Context(), src.SourceID)
	if err != nil {
		failure(w, err)
		return
	}
	if deltaOwned {
		respond(w, http.StatusConflict, map[string]any{"error": "raw source takes precedence over parsed summary"})
		return
	}
	if oldGeneration != src.Generation {
		_, stateErr := h.config.Store.SourceState(r.Context(), src.SourceID, src.Generation)
		if stateErr == nil {
			respond(w, http.StatusConflict, map[string]any{"error": "raw source takes precedence over parsed summary"})
			return
		}
		if !errors.Is(stateErr, store.ErrNotFound) {
			failure(w, stateErr)
			return
		}
	}
	reuse := oldGeneration == src.Generation && oldHash == hash
	if reuse && prior == int64(len(b)) {
		state, e := h.config.Store.SourceState(r.Context(), src.SourceID, src.Generation)
		if e == nil && state.IndexedOffset == prior {
			if err = h.alias(r.Context(), src); err != nil {
				failure(w, err)
				return
			}
			h.recordMachine(r.Context(), body.Machine)
			respond(w, 200, map[string]any{"ok": true, "id": "relay:" + body.Machine + ":" + body.File, "boot": h.config.Store.RecoveryEpoch()})
			return
		}
	}
	if prior > 0 && !reuse {
		src, err = h.rotate(r.Context(), src)
		if err != nil {
			failure(w, err)
			return
		}
		prior = 0
	}
	src.Size = int64(len(b))
	src.ModifiedAt = time.Now().UTC()
	if err = h.config.Store.SetExternalIndex(r.Context(), src.SourceID, true); err != nil {
		failure(w, err)
		return
	}
	if err = h.config.Store.RememberLegacySnapshot(r.Context(), src.SourceID, src.Generation, hash); err != nil {
		failure(w, err)
		return
	}
	if prior > int64(len(b)) {
		failure(w, fmt.Errorf("%w: snapshot offset", store.ErrConflict))
		return
	}
	if err = h.storeStream(r.Context(), src, prior, int64(len(b))-prior, bytes.NewReader(b[prior:])); err != nil {
		failure(w, err)
		return
	}
	if err = h.config.Store.RememberLegacySource(r.Context(), src); err != nil {
		failure(w, err)
		return
	}
	sessionID := parser.SessionID(src)
	batch := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, ToOffset: int64(len(b)), Session: store.Session{ID: sessionID, Title: body.Meta.Title, Project: body.Meta.Project, LastActivity: src.ModifiedAt, Completeness: "legacy-summary-only"}}
	for i, a := range body.Result.Agents {
		buckets := a.UsageByModel
		if len(buckets) == 0 {
			buckets = map[string]usageBucket{a.Model: a.usageBucket}
		}
		for model, u := range buckets {
			batch.Usage = append(batch.Usage, store.UsageObservation{ID: parser.ID(src.SourceID, src.Generation, "agent", fmt.Sprint(i), model), AgentID: a.ID, Model: model, Kind: "legacy-summary", CounterScope: "unverified-legacy-summary", TokensIn: u.In, TokensCache: u.Cache, TokensCacheWrite: u.Write, TokensOut: u.Out, Evidence: json.RawMessage(`{"complete":false,"provenance":"legacy-parsed-relay"}`)})
		}
	}
	for i, e := range body.Result.Events {
		ts, _ := time.Parse(time.RFC3339Nano, e.TS)
		text := e.Full
		if text == "" {
			text = e.Text
		}
		data, _ := json.Marshal(map[string]any{"tool": bounded(e.Tool, 200), "error": e.Error, "legacyEventIndex": i})
		batch.Events = append(batch.Events, store.Event{ID: parser.ID(src.SourceID, src.Generation, "event", fmt.Sprint(i)), SessionID: sessionID, AgentID: e.Agent, Kind: e.Kind, Timestamp: ts, SourceLength: int64(len(b)), Text: bounded(e.Text, 500), SearchText: text, Data: data})
	}
	if err = h.config.Store.CommitIndex(r.Context(), batch); err != nil {
		failure(w, err)
		return
	}
	if err = h.alias(r.Context(), src); err != nil {
		failure(w, err)
		return
	}
	h.recordMachine(r.Context(), body.Machine)
	respond(w, 200, map[string]any{"ok": true, "id": "relay:" + body.Machine + ":" + body.File, "boot": h.config.Store.RecoveryEpoch()})
}
