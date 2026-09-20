package legacy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type attr struct {
	Key   string `json:"key"`
	Value struct {
		String string          `json:"stringValue"`
		Int    json.RawMessage `json:"intValue"`
		Double float64         `json:"doubleValue"`
	} `json:"value"`
}
type span struct {
	TraceID    string `json:"traceId"`
	SpanID     string `json:"spanId"`
	Name       string `json:"name"`
	Start      string `json:"startTimeUnixNano"`
	End        string `json:"endTimeUnixNano"`
	Attributes []attr `json:"attributes"`
	Status     struct {
		Code int `json:"code"`
	} `json:"status"`
}
type traceEnvelope struct {
	Resources []struct {
		Resource struct {
			Attributes []attr `json:"attributes"`
		} `json:"resource"`
		Scopes []struct {
			Spans []span `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func attributes(list []attr) map[string]string {
	m := map[string]string{}
	for _, a := range list {
		if a.Value.String != "" {
			m[a.Key] = a.Value.String
		} else if len(a.Value.Int) > 0 {
			m[a.Key] = strings.Trim(string(a.Value.Int), "\"")
		} else if a.Value.Double != 0 {
			m[a.Key] = strconv.FormatFloat(a.Value.Double, 'f', -1, 64)
		}
	}
	return m
}
func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if m[k] != "" {
			return m[k]
		}
	}
	return ""
}
func tokens(m map[string]string, keys ...string) int64 {
	n, _ := strconv.ParseInt(first(m, keys...), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
func nano(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func (h *Handler) traces(w http.ResponseWriter, r *http.Request) {
	b, ok := h.readJSON(w, r)
	if !ok {
		return
	}
	defer func() { <-h.largeJSON }()
	var envelope traceEnvelope
	if json.Unmarshal(b, &envelope) != nil {
		respond(w, 400, map[string]any{"error": "invalid OTLP JSON"})
		return
	}
	machine := r.Header.Get("x-relay-machine")
	if machine == "" {
		machine = "legacy-otel"
	}
	if !validMachine(machine) {
		respond(w, 400, map[string]any{"error": "bad machine"})
		return
	}
	services := map[string]bool{}
	count := 0
	for _, rs := range envelope.Resources {
		service := attributes(rs.Resource.Attributes)["service.name"]
		if service == "" {
			service = "otel-agent"
		}
		if len(service) > 512 {
			respond(w, 400, map[string]any{"error": "service name too long"})
			return
		}
		services[service] = true
		for _, scope := range rs.Scopes {
			count += len(scope.Spans)
		}
	}
	for service := range services {
		if err := h.traceService(r.Context(), machine, service, b); err != nil {
			failure(w, err)
			return
		}
	}
	h.recordMachine(r.Context(), machine)
	respond(w, 200, map[string]any{"partialSuccess": map[string]any{}, "spansIngested": count})
}

func (h *Handler) traceService(ctx context.Context, machine, service string, body []byte) error {
	path := "otel/" + parser.ID(service) + ".jsonl"
	lock := h.gate(machine, path)
	if !lock.TryLock() {
		return fmt.Errorf("%w: trace service busy", store.ErrConflict)
	}
	defer lock.Unlock()
	src, durable, err := h.resolve(ctx, machine, path, "otel", service)
	if err != nil {
		return err
	}
	if err = h.config.Store.SetExternalIndex(ctx, src.SourceID, true); err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	pending, err := h.config.Store.PendingLegacyRequests(ctx, src.SourceID, src.Generation)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if p.Offset+p.Length <= durable {
			if err = h.finishTraceFrame(ctx, src, service, p); err != nil {
				return err
			}
		} else if p.Hash != hash {
			return fmt.Errorf("%w: prior trace request needs retransmission", store.ErrConflict)
		}
	}
	frame, err := h.config.Store.ReserveLegacyRequest(ctx, store.LegacyRequest{SourceID: src.SourceID, Generation: src.Generation, Hash: hash, Offset: durable, Length: int64(len(body))})
	if err != nil {
		return err
	}
	if frame.Complete {
		return nil
	}
	if durable < frame.Offset || durable > frame.Offset+frame.Length {
		return fmt.Errorf("%w: trace frame offset", store.ErrConflict)
	}
	src.Size = frame.Offset + frame.Length
	src.ModifiedAt = time.Now().UTC()
	if durable < src.Size {
		if err = h.storeStream(ctx, src, durable, src.Size-durable, bytes.NewReader(body[durable-frame.Offset:])); err != nil {
			return err
		}
	}
	if err = h.config.Store.RememberLegacySource(ctx, src); err != nil {
		return err
	}
	if err = h.finishTraceFrame(ctx, src, service, frame); err != nil {
		return err
	}
	return h.alias(ctx, src)
}

func (h *Handler) finishTraceFrame(ctx context.Context, src protocol.Source, service string, frame store.LegacyRequest) error {
	st, err := h.config.Store.SourceState(ctx, src.SourceID, src.Generation)
	if err != nil {
		return err
	}
	if st.IndexedOffset >= frame.Offset+frame.Length {
		return h.config.Store.CompleteLegacyRequest(ctx, frame)
	}
	if st.IndexedOffset != frame.Offset || frame.Length > maxJSONBytes {
		return fmt.Errorf("%w: pending trace index bounds", store.ErrConflict)
	}
	raw, err := h.config.Store.OpenSource(ctx, src.SourceID, src.Generation, frame.Offset)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(raw, frame.Length))
	raw.Close()
	if err != nil {
		return err
	}
	if int64(len(b)) != frame.Length {
		return io.ErrUnexpectedEOF
	}
	var envelope traceEnvelope
	if err = json.Unmarshal(b, &envelope); err != nil {
		return err
	}
	id := parser.SessionID(src)
	batch := store.IndexBatch{SourceID: src.SourceID, Generation: src.Generation, FromOffset: frame.Offset, ToOffset: frame.Offset + frame.Length, Session: store.Session{ID: id, Title: service, Project: service, NativeID: service, Completeness: "otel-reported-spans"}}
	spanIndex := 0
	for _, rs := range envelope.Resources {
		name := attributes(rs.Resource.Attributes)["service.name"]
		if name == "" {
			name = "otel-agent"
		}
		if name != service {
			continue
		}
		for _, scope := range rs.Scopes {
			for _, sp := range scope.Spans {
				at := attributes(sp.Attributes)
				agent := first(at, "gen_ai.agent.name", "agent.name", "crewai.agent.role", "openinference.agent.name", "llm.agent.name", "traceloop.entity.name")
				if agent == "" {
					agent = "main"
				}
				model := first(at, "gen_ai.request.model", "gen_ai.response.model", "llm.model_name", "llm.request.model", "llm.response.model")
				native := sp.TraceID + ":" + sp.SpanID
				if sp.SpanID == "" {
					native = frame.Hash + ":" + strconv.Itoa(spanIndex)
				}
				spanIndex++
				ts := nano(sp.End)
				if ts.IsZero() {
					ts = nano(sp.Start)
				}
				if ts.After(batch.Session.LastActivity) {
					batch.Session.LastActivity = ts
				}
				kind := "tool-call"
				text := first(at, "gen_ai.tool.call.arguments", "tool.parameters", "input.value")
				oi := strings.ToUpper(first(at, "openinference.span.kind", "span.kind"))
				isLLM := oi == "LLM" || (oi != "TOOL" && oi != "RETRIEVER" && oi != "EMBEDDING" && (model != "" || strings.Contains(strings.ToLower(sp.Name), "chat")))
				data, _ := json.Marshal(map[string]any{"traceId": sp.TraceID, "spanId": sp.SpanID, "tool": bounded(sp.Name, 200), "error": sp.Status.Code == 2})
				if isLLM {
					kind = "assistant-text"
					text = first(at, "gen_ai.completion", "gen_ai.output.messages", "gen_ai.response.text", "output.value", "llm.output_messages.0.message.content")
					input := tokens(at, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt")
					cached := tokens(at, "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cached_input_tokens")
					if cached > input {
						cached = input
					}
					batch.Usage = append(batch.Usage, store.UsageObservation{ID: parser.ID(src.SourceID, src.Generation, "span-usage", native), AgentID: agent, Model: model, Timestamp: ts, Kind: "request", CounterScope: "otel-span", TokensIn: input - cached, TokensCache: cached, TokensOut: tokens(at, "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion"), Evidence: data})
					prompt := first(at, "gen_ai.prompt", "gen_ai.input.messages", "input.value", "llm.input_messages.0.message.content")
					if prompt != "" {
						batch.Events = append(batch.Events, store.Event{ID: parser.ID(src.SourceID, src.Generation, native, "prompt"), SessionID: id, AgentID: agent, Kind: "user-text", Timestamp: nano(sp.Start), SourceOffset: frame.Offset, SourceLength: frame.Length, Text: bounded(prompt, 500), SearchText: prompt, Data: data, DedupeKey: native + ":prompt"})
					}
				}
				if text == "" {
					text = sp.Name
				}
				batch.Events = append(batch.Events, store.Event{ID: parser.ID(src.SourceID, src.Generation, native, "result"), SessionID: id, AgentID: agent, Kind: kind, Timestamp: ts, SourceOffset: frame.Offset, SourceLength: frame.Length, Text: bounded(text, 500), SearchText: text, Data: data, DedupeKey: native + ":result"})
			}
		}
	}
	if err = h.config.Store.CommitIndex(ctx, batch); err != nil {
		return err
	}
	return h.config.Store.CompleteLegacyRequest(ctx, frame)
}
