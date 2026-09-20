// Package parser translates source records without treating prices as evidence.
// It never owns archive state or reads a user's transcript directories directly.
package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Version 7 preserves each Claude tool record's explicit working directory.
// Old checkpoints must not append new interpretation onto incomplete history.
const Version = "7"
const MaxRecordBytes = 8 << 20

var ErrRebuildRequired = errors.New("parser projection rebuild required")

type Counter struct {
	Input, Cache, CacheWrite, Output int64
	Epoch                            int64
	Set                              bool
	Model                            string
}
type State struct {
	CodexIdentitySet    bool               `json:"codexIdentitySet,omitempty"`
	ClaudeIdentityScope string             `json:"claudeIdentityScope,omitempty"`
	ClaudeSessionID     string             `json:"claudeSessionId,omitempty"`
	ClaudeAgentID       string             `json:"claudeAgentId,omitempty"`
	ScanOffset          int64              `json:"scanOffset,omitempty"`
	Version             string             `json:"version"`
	NativeID            string             `json:"nativeId"`
	ParentThreadID      string             `json:"parentThreadId,omitempty"`
	ForkedFromID        string             `json:"forkedFromId,omitempty"`
	Model               string             `json:"model"`
	Project             string             `json:"project"`
	Title               string             `json:"title"`
	Counters            map[string]Counter `json:"counters"`
}
type Result struct {
	Events []store.Event
	Usage  []store.UsageObservation
}

func ID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func SessionID(s protocol.Source) string {
	// Source identity survives rename. Provider-native identity is recorded as an
	// alias only: adopting an untrusted duplicate native ID must not merge evidence.
	return ID(s.MachineID, s.Provider, s.SourceID)
}
func DecodeState(raw json.RawMessage) (State, error) {
	var s State
	if len(raw) > 0 {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return s, errors.New("damaged parser checkpoint: null")
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("damaged parser checkpoint: %w", err)
		}
	}
	if s.Version != "" && s.Version != Version {
		return s, fmt.Errorf("%w: checkpoint version %s, required %s; existing projection retained", ErrRebuildRequired, s.Version, Version)
	}
	s.Version = Version
	if s.ScanOffset < 0 {
		return s, errors.New("damaged parser checkpoint: negative scan offset")
	}
	if !validClaudeIdentity(s) {
		return s, errors.New("damaged parser checkpoint: inconsistent Claude identity")
	}
	for _, c := range s.Counters {
		if !c.Set || c.Epoch < 0 || !validCounter(c) {
			return s, errors.New("damaged parser checkpoint: inconsistent usage counter")
		}
	}
	if s.Counters == nil {
		s.Counters = make(map[string]Counter)
	}
	return s, nil
}
func recordID(s protocol.Source, off int64, suffix string) string {
	return ID(s.MachineID, s.SourceID, s.Generation, strconv.FormatInt(off, 10), suffix)
}
func preview(s string) string {
	n := 0
	for pos := range s {
		if n == 500 {
			return s[:pos] + "…"
		}
		n++
	}
	return s
}
func str(v any) string { s, _ := v.(string); return s }
func attribute(v any) string {
	s := str(v)
	if len(s) > 4096 {
		return ""
	}
	return s
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func number(v any) int64 {
	n, _ := v.(json.Number)
	i, _ := n.Int64()
	if i < 0 {
		return 0
	}
	return i
}
func token(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil && i >= 0
}
func marshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// Tool results may contain megabytes of inline image encoding. The raw source
// remains complete; image bytes are not natural-language search documents. Keep
// the original preview and dedupe identity, changing only the FTS contribution.
func toolOutputSearch(output any, fallback string) string {
	blocks, ok := output.([]any)
	if !ok {
		return fallback
	}
	var clean []any
	for i, value := range blocks {
		block := obj(value)
		if str(block["type"]) != "input_image" || !strings.HasPrefix(str(block["image_url"]), "data:image/") {
			continue
		}
		if clean == nil {
			clean = append([]any(nil), blocks...)
		}
		copyBlock := make(map[string]any, len(block))
		for key, value := range block {
			copyBlock[key] = value
		}
		copyBlock["image_url"] = "[embedded image preserved in complete source]"
		clean[i] = copyBlock
	}
	if clean == nil {
		return fallback
	}
	return string(marshal(clean))
}
func metadata(v any) json.RawMessage {
	b := marshal(v)
	if len(b) <= 16384 {
		return b
	}
	// The raw record is the lossless authority. A maliciously huge identifier or
	// extension field must not wedge every later record behind an oversized SQL
	// metadata value; retain a bounded diagnostic and the original raw reference.
	h := sha256.Sum256(b)
	return marshal(map[string]any{"code": "metadata_exceeds_budget", "sha256": hex.EncodeToString(h[:])})
}
func content(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	a, _ := v.([]any)
	var out []string
	for _, v := range a {
		m := obj(v)
		if t := str(m["text"]); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// Record consumes one complete JSONL record. Errors become indexed diagnostic
// events referencing the unchanged raw bytes, never a discarded line.
func Record(s protocol.Source, state *State, offset int64, line []byte) Result {
	var result Result
	sessionID := SessionID(s)
	ts := time.Time{}
	emit := func(kind, text, agent, key string, data any) {
		e := store.Event{ID: recordID(s, offset, key), SessionID: sessionID, AgentID: agent, Kind: kind, Timestamp: ts, SourceOffset: offset, SourceLength: int64(len(line)), Text: preview(text), SearchText: text, Data: metadata(data)}
		result.Events = append(result.Events, e)
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return result
	}
	if len(line) > MaxRecordBytes {
		emit("indexing-error", "Record exceeds parser memory budget; download original source record", "main", "oversize", map[string]any{"code": "record_too_large"})
		return result
	}
	var o map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		emit("indexing-error", "Malformed JSON record preserved in raw history", "main", "malformed", map[string]any{"code": "malformed_json", "detail": err.Error()})
		return result
	}
	if o == nil || dec.Decode(new(any)) != io.EOF {
		emit("indexing-error", "Malformed JSON record preserved in raw history", "main", "malformed", map[string]any{"code": "malformed_json"})
		return result
	}
	ts, _ = time.Parse(time.RFC3339Nano, str(o["timestamp"]))
	if state.NativeID == "" && state.ClaudeIdentityScope != "ambiguous" {
		state.NativeID = s.NativeID
	}
	if state.Counters == nil {
		state.Counters = map[string]Counter{}
	}
	state.Version = Version
	usage := func(key, agent, model, scope, kind string, in, cache, write, out int64, evidence any) {
		result.Usage = append(result.Usage, store.UsageObservation{ID: ID(s.MachineID, s.SourceID, s.Generation, "usage", key), SessionID: sessionID, AgentID: agent, Model: model, Timestamp: ts, CounterScope: scope, Kind: kind, TokensIn: in, TokensCache: cache, TokensCacheWrite: write, TokensOut: out, Evidence: metadata(evidence)})
	}
	switch s.Provider {
	case "claude":
		// Do not substitute state.Project: later records can change it, and a
		// missing per-record cwd is not proof of the last observed directory.
		recordCWD := str(o["cwd"])
		if len(recordCWD) > 4096 || strings.IndexFunc(recordCWD, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			recordCWD = ""
		}
		if cwd := attribute(o["cwd"]); cwd != "" {
			state.Project = cwd
		}
		agent := attribute(o["agentId"])
		if agent == "" {
			agent = "main"
			if o["isSidechain"] == true {
				agent = "sidechain:unresolved"
			}
		}
		lineage := map[string]any{"messageId": o["uuid"], "parentMessageId": o["parentUuid"], "sidechain": o["isSidechain"] == true}
		sidechain := o["isSidechain"] == true
		if o["isSidechain"] == nil && state.ClaudeIdentityScope == "agent" && attribute(o["agentId"]) == state.ClaudeAgentID {
			sidechain = true // An omitted flag does not retract established identity.
		}
		if observeClaudeIdentity(state, attribute(o["sessionId"]), attribute(o["agentId"]), sidechain) {
			emit("indexing-error", "Conflicting or unresolved Claude source identity; source evidence retained without a native alias", agent, "claude-identity", map[string]any{"code": "ambiguous_claude_identity"})
		}
		if parent := attribute(o["parentSessionId"]); parent != "" {
			if state.ClaudeIdentityScope != "ambiguous" {
				state.ParentThreadID = parent
			}
		}
		m := obj(o["message"])
		role := attribute(m["role"])
		if role == "" {
			role = attribute(o["type"])
		}
		text := content(m["content"])
		if role == "assistant" || role == "user" {
			key := str(o["uuid"])
			if key == "" {
				key = strconv.FormatInt(offset, 10)
			}
			if text != "" {
				emit(role+"-text", text, agent, key, lineage)
				if str(o["uuid"]) != "" {
					result.Events[len(result.Events)-1].DedupeKey = ID(agent, key, role, text)
				}
				if role == "user" {
					selectUserTitle(state, text)
				}
			}
			blocks, _ := m["content"].([]any)
			for i, b := range blocks {
				block := obj(b)
				kind := str(block["type"])
				if kind == "tool_use" {
					toolData := map[string]any{"tool": block["name"], "toolUseId": block["id"]}
					if recordCWD != "" {
						toolData["workingDirectory"] = recordCWD
					}
					emit("tool-call", string(marshal(block["input"])), agent, key+"tool"+strconv.Itoa(i), toolData)
					if id := str(block["id"]); id != "" {
						result.Events[len(result.Events)-1].DedupeKey = ID(agent, "tool-call", id, string(marshal(block)))
					}
				}
				if kind == "tool_result" {
					emit("tool-result", content(block["content"]), agent, key+"result"+strconv.Itoa(i), map[string]any{"toolUseId": block["tool_use_id"], "error": block["is_error"]})
					if id := str(block["tool_use_id"]); id != "" {
						result.Events[len(result.Events)-1].DedupeKey = ID(agent, "tool-result", id, string(marshal(block)))
					}
				}
				if kind == "thinking" {
					emit("assistant-reasoning", str(block["thinking"]), agent, key+"thinking"+strconv.Itoa(i), lineage)
					if str(o["uuid"]) != "" {
						result.Events[len(result.Events)-1].DedupeKey = ID(agent, key, kind, str(block["thinking"]))
					}
				}
			}
			if role == "assistant" {
				u := obj(m["usage"])
				model := attribute(m["model"])
				if len(u) > 0 {
					msg := str(m["id"])
					kind := "message-partial"
					if msg == "" {
						msg = "offset:" + strconv.FormatInt(offset, 10)
						kind = "message-without-id"
					}
					values := make([]int64, 4)
					present := []string{}
					for i, field := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"} {
						if value, ok := token(u[field]); ok {
							values[i] = value
							present = append(present, []string{"tokensIn", "tokensCache", "tokensCacheWrite", "tokensOut"}[i])
						} else if _, exists := u[field]; exists {
							emit("indexing-error", "Invalid usage field preserved; previous known value retained", agent, "invalid-usage-"+field, map[string]any{"code": "invalid_usage_field", "field": field})
						}
					}
					if _, exists := m["model"].(string); exists {
						present = append(present, "model")
					}
					if len(present) > 0 {
						usage(ID(agent, msg), agent, model, "provider-message", kind, values[0], values[1], values[2], values[3], map[string]any{"offset": offset, "messageId": m["id"], "usage": u})
						result.Usage[len(result.Usage)-1].Present = present
					}
				}
			}
		} else {
			emit("source-record", text, agent, "other", map[string]any{"sourceType": o["type"]})
		}
	case "codex":
		p := obj(o["payload"])
		typ := attribute(o["type"])
		agent := "main"
		if typ == "session_meta" {
			id := attribute(p["id"])
			if state.CodexIdentitySet && id != "" && id != state.NativeID {
				// Forks can embed older parent headers. Preserve those records as
				// evidence, but never let them replace the owning thread or move
				// its cumulative counter into a new scope (counting it again).
				emit("session-meta", "", agent, "meta", map[string]any{"nativeId": id, "ownerNativeId": state.NativeID, "inheritedOrConflicting": true, "parentThreadId": p["parent_thread_id"], "forkedFromId": p["forked_from_id"], "cwd": p["cwd"]})
				emit("indexing-error", "Embedded metadata names a different thread; owning identity retained and inherited history requires reconciliation", agent, "identity-conflict", map[string]any{"code": "embedded_thread_metadata", "nativeId": id, "ownerNativeId": state.NativeID})
				return result
			}
			if id != "" {
				state.NativeID = id
				state.CodexIdentitySet = true
			}
			if cwd := attribute(p["cwd"]); cwd != "" {
				state.Project = cwd
			}
			if m := attribute(p["model"]); m != "" {
				state.Model = m
			}
			source := obj(p["source"])
			sub := obj(obj(source["subagent"])["thread_spawn"])
			if parent := attribute(sub["parent_thread_id"]); parent != "" {
				state.ParentThreadID = parent
			}
			if fork := attribute(p["forked_from_id"]); fork != "" {
				state.ForkedFromID = fork
			}
			if parent := attribute(p["parent_thread_id"]); parent != "" {
				state.ParentThreadID = parent
			}
			emit("session-meta", "", agent, "meta", map[string]any{"nativeId": state.NativeID, "parentThreadId": state.ParentThreadID, "forkedFromId": state.ForkedFromID, "cwd": state.Project})
		} else if typ == "turn_context" {
			if model := attribute(p["model"]); model != "" {
				state.Model = model
			}
			emit("model-context", state.Model, agent, "context", nil)
		} else if typ == "response_item" {
			kind := str(p["type"])
			switch kind {
			case "message":
				text := content(p["content"])
				role := attribute(p["role"])
				emit(role+"-text", text, agent, "message", nil)
				if id := str(p["id"]); id != "" {
					result.Events[len(result.Events)-1].DedupeKey = ID(agent, "message", id, text)
				}
				if role == "user" {
					selectUserTitle(state, text)
				}
			case "function_call", "custom_tool_call":
				text := str(p["arguments"])
				if text == "" {
					text = str(p["input"])
				}
				emit("tool-call", text, agent, "call", map[string]any{"tool": p["name"], "toolUseId": p["call_id"]})
				if id := str(p["call_id"]); id != "" {
					result.Events[len(result.Events)-1].DedupeKey = ID(agent, kind, id, text)
				}
			case "function_call_output", "custom_tool_call_output":
				text := str(p["output"])
				if text == "" {
					text = string(marshal(p["output"]))
				}
				emit("tool-result", text, agent, "output", map[string]any{"toolUseId": p["call_id"]})
				result.Events[len(result.Events)-1].SearchText = toolOutputSearch(p["output"], text)
				if id := str(p["call_id"]); id != "" {
					result.Events[len(result.Events)-1].DedupeKey = ID(agent, kind, id, text)
				}
			case "reasoning":
				emit("assistant-reasoning", content(p["summary"])+content(p["content"]), agent, "reasoning", nil)
			default:
				emit("source-record", "", agent, "response", map[string]any{"sourceType": kind})
			}
		} else if typ == "event_msg" && str(p["type"]) == "token_count" {
			add := codexUsage(s, state, offset, int64(len(line)), ts, p)
			result.Events = append(result.Events, add.Events...)
			result.Usage = append(result.Usage, add.Usage...)
		} else if typ == "event_msg" && str(p["type"]) == "model_reroute" {
			if model := attribute(p["to_model"]); model != "" {
				state.Model = model
			}
			emit("model-context", state.Model, agent, "reroute", nil)
		} else if typ == "event_msg" && str(p["type"]) == "sub_agent_activity" {
			emit("agent-lifecycle", str(p["agent_path"]), agent, "child", map[string]any{"threadId": p["agent_thread_id"], "agentPath": p["agent_path"], "state": p["kind"]})
		} else {
			emit("source-record", str(p["message"]), agent, "other", map[string]any{"sourceType": typ, "eventType": p["type"]})
		}
	default:
		emit("indexing-error", "Unsupported provider; complete original source remains available", "main", "unsupported", map[string]any{"code": "unsupported_provider"})
	}
	return result
}
