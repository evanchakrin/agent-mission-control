package parser

import (
	"math"
	"strconv"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func validCounter(c Counter) bool {
	return c.Input >= 0 && c.Output >= 0 && c.Cache >= 0 && c.CacheWrite >= 0 &&
		c.Cache <= c.Input && c.CacheWrite <= c.Input-c.Cache && c.Output <= math.MaxInt64-c.Input
}

// Codex's protocol appends last_token_usage into total_token_usage, but can also
// replace it with a context-window estimate. A decrease alone therefore cannot
// establish a new billable epoch. See README.md for primary-source references.
func readCounter(u map[string]any) (Counter, bool) {
	var c Counter
	var ok bool
	if c.Input, ok = token(u["input_tokens"]); !ok {
		return c, false
	}
	if c.Output, ok = token(u["output_tokens"]); !ok {
		return c, false
	}
	for _, field := range []struct {
		name  string
		value *int64
	}{{"cached_input_tokens", &c.Cache}, {"cache_write_input_tokens", &c.CacheWrite}} {
		if v, exists := u[field.name]; exists {
			if *field.value, ok = token(v); !ok {
				return c, false
			}
		}
	}
	if !validCounter(c) {
		return c, false
	}
	// Reasoning is a subset of output and is never added a second time. Some
	// context-window estimates report total_tokens but zero input/output buckets.
	if total, exists := u["total_tokens"]; exists {
		n, valid := token(total)
		if !valid || c.Output > (1<<63-1)-c.Input || n != c.Input+c.Output {
			return c, false
		}
	}
	c.Set = true
	return c, true
}

func subtractCounter(a, b Counter) (Counter, bool) {
	d := Counter{Input: a.Input - b.Input, Cache: a.Cache - b.Cache, CacheWrite: a.CacheWrite - b.CacheWrite, Output: a.Output - b.Output, Set: true}
	return d, d.Input >= 0 && d.Cache >= 0 && d.CacheWrite >= 0 && d.Output >= 0 && d.Cache <= d.Input && d.CacheWrite <= d.Input-d.Cache
}
func sameCounter(a, b Counter) bool {
	return a.Input == b.Input && a.Cache == b.Cache && a.CacheWrite == b.CacheWrite && a.Output == b.Output
}

func codexUsage(s protocol.Source, state *State, offset, length int64, ts time.Time, p map[string]any) Result {
	var result Result
	info := obj(p["info"])
	scope := attribute(p["thread_id"])
	if scope == "" {
		scope = state.NativeID
	}
	if scope == "" {
		scope = s.SourceID
	}
	agent := "main"
	if scope != state.NativeID && scope != s.SourceID {
		agent = scope
	}
	emit := func(code, text string) {
		result.Events = append(result.Events, store.Event{ID: recordID(s, offset, code), SessionID: SessionID(s), AgentID: agent, Kind: "indexing-error", Timestamp: ts, SourceOffset: offset, SourceLength: length, Text: text, Data: marshal(map[string]any{"code": code, "counterScope": scope})})
	}
	if info == nil {
		// A token_count event with info:null is a legitimate quota-only update.
		result.Events = append(result.Events, store.Event{ID: recordID(s, offset, "rate-limits"), SessionID: SessionID(s), AgentID: agent, Kind: "rate-limit-update", Timestamp: ts, SourceOffset: offset, SourceLength: length})
		return result
	}
	next, valid := readCounter(obj(info["total_token_usage"]))
	if !valid {
		emit("invalid_usage_counter", "Invalid or context-only usage counter preserved without inventing billable tokens")
		return result
	}
	prev := state.Counters[scope]
	next.Epoch = prev.Epoch
	model := state.Model
	if explicit := attribute(p["model"]); explicit != "" {
		model = explicit
	}
	if agent != "main" && str(p["model"]) == "" {
		model = prev.Model
	} // parent model is not child evidence
	next.Model = model
	if prev.Set && sameCounter(next, prev) {
		return result
	}
	delta, monotonic := subtractCounter(next, prev)
	if !monotonic {
		// Keep the high-water checkpoint. Accepting a smaller replay as baseline
		// would count the same interval again when the next newer row arrives.
		if prev.Epoch < math.MaxInt64 {
			prev.Epoch++
		}
		state.Counters[scope] = prev
		emit("counter_discontinuity", "Counter decreased or changed cache classification; reset versus replay is unverified, original evidence retained")
		return result
	}
	state.Counters[scope] = next
	last, lastValid := readCounter(obj(info["last_token_usage"]))
	add := func(suffix, model, kind string, count Counter) {
		if count.Input == 0 && count.Output == 0 {
			return
		}
		result.Usage = append(result.Usage, store.UsageObservation{
			ID: ID(s.MachineID, s.SourceID, s.Generation, "usage", strconv.FormatInt(offset, 10), scope, suffix), SessionID: SessionID(s), AgentID: agent, Model: model, Timestamp: ts, CounterScope: "thread:" + scope, Kind: kind,
			TokensIn: count.Input - count.Cache - count.CacheWrite, TokensCache: count.Cache, TokensCacheWrite: count.CacheWrite, TokensOut: count.Output,
			Evidence: marshal(map[string]any{"offset": offset, "counter": next, "previousCounter": prev, "lastCounter": last, "epoch": next.Epoch, "attribution": kind}),
		})
	}
	// Exact deltas in an unchanged known model context can be priced. At the
	// first snapshot or a model transition, only a fitting reported last call is
	// attributable; a prior-history residual remains explicitly unpriced.
	if model != "" && prev.Set && prev.Model == model {
		add("delta", model, "cumulative-delta", delta)
		return result
	}
	if model != "" && lastValid {
		if residual, fits := subtractCounter(delta, last); fits {
			add("prior", "", "incomplete-attribution", residual)
			add("last", model, "reported-last-call", last)
			return result
		}
	}
	add("delta", "", "incomplete-attribution", delta)
	return result
}
