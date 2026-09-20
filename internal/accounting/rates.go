// Package accounting derives explicit, versioned estimates from immutable usage.
package accounting

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type Rate struct {
	ID                                   string     `json:"id"`
	Model                                string     `json:"model"`
	Aliases                              []string   `json:"aliases"`
	Context                              string     `json:"context"`
	EffectiveFrom                        time.Time  `json:"effectiveFrom"`
	EffectiveTo                          *time.Time `json:"effectiveTo,omitempty"`
	Source                               string     `json:"source"`
	Tier                                 string     `json:"tier,omitempty"`
	Input, CacheRead, CacheWrite, Output float64
}

// Missing prices are unknown, not free. Typed internal fixtures can still use
// explicit zero values; every serialized catalog must carry all four prices.
func (r *Rate) UnmarshalJSON(raw []byte) error {
	type wire Rate
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	keys := json.NewDecoder(bytes.NewReader(raw))
	if _, err := keys.Token(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for keys.More() {
		key, err := keys.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("invalid rate field")
		}
		name = strings.ToLower(name)
		if seen[name] {
			return errors.New("duplicate rate field")
		}
		seen[name] = true
		var value json.RawMessage
		if err = keys.Decode(&value); err != nil {
			return err
		}
	}
	for _, required := range []string{"Input", "CacheRead", "CacheWrite", "Output"} {
		found := 0
		for key, value := range fields {
			if strings.EqualFold(key, required) {
				found++
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return errors.New("rate prices cannot be null")
				}
			}
		}
		if found != 1 {
			return errors.New("rate requires explicit Input, CacheRead, CacheWrite and Output prices")
		}
	}
	var value wire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*r = Rate(value)
	return nil
}

type Estimate struct {
	Components         *store.CostComponents `json:"components,omitempty"`
	Cost               float64               `json:"cost"`
	RecordedTokens     int64                 `json:"recordedTokens"`
	PricedTokens       int64                 `json:"pricedTokens"`
	UnpricedTokens     int64                 `json:"unpricedTokens"`
	UnattributedTokens int64                 `json:"unattributedTokens"`
	Coverage           float64               `json:"coverage"`
	RateIDs            []string              `json:"rateIds"`
}

func Validate(rates []Rate) error {
	ids := map[string]bool{}
	for _, r := range rates {
		if r.ID == "" || ids[r.ID] || r.Model == "" || r.Context == "" || r.Source == "" || r.EffectiveFrom.IsZero() {
			return errors.New("rate requires unique ID, exact model, billing context, effective date and source")
		}
		ids[r.ID] = true
		switch r.Tier {
		case "", "unknown", "flagship", "premium", "mid", "cheap":
		default:
			return errors.New("rate tier must be flagship, premium, mid, cheap or unknown")
		}
		if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
			return errors.New("invalid rate interval")
		}
		for _, n := range []float64{r.Input, r.CacheRead, r.CacheWrite, r.Output} {
			if n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
				return errors.New("invalid price")
			}
		}
	}
	// Ambiguous overlapping cards are rejected, never first-match wins.
	for i, a := range rates {
		for _, b := range rates[i+1:] {
			if a.Context != b.Context || (a.EffectiveTo != nil && !a.EffectiveTo.After(b.EffectiveFrom)) || (b.EffectiveTo != nil && !b.EffectiveTo.After(a.EffectiveFrom)) {
				continue
			}
			for _, m := range append([]string{a.Model}, a.Aliases...) {
				for _, n := range append([]string{b.Model}, b.Aliases...) {
					if m == n {
						return errors.New("overlapping model/alias rate intervals")
					}
				}
			}
		}
	}
	return nil
}

func Price(observations []store.UsageObservation, rates []Rate, context string, comparisonAt *time.Time) Estimate {
	var e Estimate
	used := map[string]bool{}
	for _, u := range observations {
		tokens := u.TokensIn + u.TokensCache + u.TokensCacheWrite + u.TokensOut
		e.RecordedTokens += tokens
		if u.Model == "" || u.Kind == "incomplete-attribution" || u.Kind == "message-without-id" || u.Kind == "message-partial" {
			e.UnattributedTokens += tokens
		}
		at := u.Timestamp
		if comparisonAt != nil {
			at = *comparisonAt
		}
		var selected *Rate
		for i := range rates {
			r := &rates[i]
			if r.Context != context || at.Before(r.EffectiveFrom) || (r.EffectiveTo != nil && !at.Before(*r.EffectiveTo)) {
				continue
			}
			exact := u.Model == r.Model
			for _, alias := range r.Aliases {
				if alias == u.Model {
					exact = true
				}
			}
			if exact {
				if selected != nil {
					selected = nil
					break
				}
				selected = r
			}
		}
		if selected == nil || u.Kind == "incomplete-attribution" || u.Kind == "message-partial" {
			e.UnpricedTokens += tokens
			continue
		}
		e.PricedTokens += tokens
		used[selected.ID] = true
		c := store.CostComponents{Input: float64(u.TokensIn) * selected.Input / 1e6, CacheRead: float64(u.TokensCache) * selected.CacheRead / 1e6, CacheWrite: float64(u.TokensCacheWrite) * selected.CacheWrite / 1e6, Output: float64(u.TokensOut) * selected.Output / 1e6, PricedTokens: tokens}
		e.Cost += c.Input + c.CacheRead + c.CacheWrite + c.Output
		addComponents(&e, &c)
	}
	if e.RecordedTokens > 0 {
		e.Coverage = float64(e.PricedTokens) / float64(e.RecordedTokens)
	}
	for id := range used {
		e.RateIDs = append(e.RateIDs, id)
	}
	sort.Strings(e.RateIDs)
	return e
}

func addComponents(e *Estimate, c *store.CostComponents) {
	if c == nil {
		return
	}
	if e.Components == nil {
		e.Components = &store.CostComponents{}
	}
	e.Components.Input += c.Input
	e.Components.CacheRead += c.CacheRead
	e.Components.CacheWrite += c.CacheWrite
	e.Components.Output += c.Output
	e.Components.PricedTokens += c.PricedTokens
}
