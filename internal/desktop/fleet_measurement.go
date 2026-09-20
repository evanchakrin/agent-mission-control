package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

const measurementStart = "<!-- amc:economics-measurement -->"
const measurementEnd = "<!-- /amc:economics-measurement -->"

var legacyMeasurementHeader = regexp.MustCompile(`^_Measured [0-9]{4}-[0-9]{2}-[0-9]{2} by Agent Mission Control from [0-9]+ sessions \([0-9,]+ Claude subagents\)\. These figures refresh from the Economics view; do not hand-edit them:_$`)

// RemeasureFromHub reads a bounded consistent measurement over the owner pipe.
// Only the explicit generated region is replaced; policy is not regenerated.
func RemeasureFromHub(ctx context.Context, client *http.Client, endpoint, body string) (Measurement, error) {
	if len(body) > 20000 {
		return Measurement{}, fail(400, "standing order is too long to remeasure")
	}
	if _, err := replaceMeasurementRegion(body, ""); err != nil {
		return Measurement{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Measurement{}, err
	}
	response, err := client.Do(req)
	if err != nil {
		return Measurement{}, fail(503, "hub measurements are unavailable; the standing order was not changed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Measurement{}, fail(503, "hub measurement could not complete; the standing order was not changed")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return Measurement{}, fail(502, "hub measurement is incomplete or exceeds its size bound")
	}
	var sample store.EconomicsMeasurement
	if json.Unmarshal(raw, &sample) != nil || sample.Version != 1 || sample.MeasuredAt.IsZero() || sample.Lifetimes.Scope != "claude-child-usage-messages-v1" || len(sample.Lifetimes.Buckets) != 8 {
		return Measurement{}, fail(502, "unsupported hub measurement")
	}
	c, l := sample.Costs, sample.Lifetimes
	if c.Sessions < 0 || c.RecordedTokens < 0 || c.PricedTokens < 0 || c.UnpricedOrUnmeasuredTokens < 0 || c.PricedTokens > c.RecordedTokens || c.UnpricedOrUnmeasuredTokens != c.RecordedTokens-c.PricedTokens || l.ChildSources < 0 || l.ChildSources > c.Sessions || l.WithoutMessageObservations < 0 || l.WithoutMessageObservations > l.ChildSources {
		return Measurement{}, fail(502, "inconsistent hub measurement counts")
	}
	price := func(n *float64) string {
		if n == nil {
			return "unavailable"
		}
		return fmt.Sprintf("~$%.6f", *n)
	}
	validCost := func(n *float64) bool { return n == nil || !math.IsNaN(*n) && !math.IsInf(*n, 0) && *n >= 0 }
	if !validCost(c.KnownCost) {
		return Measurement{}, fail(502, "invalid hub cost estimate")
	}
	var evidence strings.Builder
	fmt.Fprintf(&evidence, "Measured %s by Agent Mission Control from %d indexed sessions, including archived history.\n\n", sample.MeasuredAt.UTC().Format(time.RFC3339Nano), c.Sessions)
	fmt.Fprintf(&evidence, "Known cost estimate: %s; %d / %d recorded tokens priced; %d unpriced or awaiting measurement. Estimates are not invoices. Unpriced usage is not free.\n\n", price(c.KnownCost), c.PricedTokens, c.RecordedTokens, c.UnpricedOrUnmeasuredTokens)
	if l.WithUnstableMessageIdentity < 0 || l.WithUnstableMessageIdentity > l.ChildSources {
		return Measurement{}, fail(502, "invalid child identity counts")
	}
	fmt.Fprintf(&evidence, "%d evidenced Claude child sources; %d without usage-bearing message observations; %d with unstable message identity. Counts below are deduplicated usage-bearing messages, not every chat turn or elapsed lifetime.\n\n", l.ChildSources, l.WithoutMessageObservations, l.WithUnstableMessageIdentity)
	evidence.WriteString("| Observed messages | Child sources | Comparable sources | Comparable messages | Estimate per comparable message |\n|---|---:|---:|---:|---:|\n")
	var counted int64
	labels := []string{"1–2", "3–5", "6–10", "11–20", "21–40", "41–80", "81–160", "161+"}
	for i, b := range l.Buckets {
		if b.Label != labels[i] || b.Agents < 0 || b.Agents > l.ChildSources-counted || b.Messages < b.Agents || b.CostEligibleAgents < 0 || b.CostEligibleAgents > b.Agents || b.CostEligibleMessages < b.CostEligibleAgents || b.CostEligibleMessages > b.Messages || (b.CostEligibleAgents > 0) != (b.CostEligibleMessages > 0) || !validCost(b.ComparableCost) || (b.ComparableCost != nil) != (b.CostEligibleMessages > 0) {
			return Measurement{}, fail(502, "inconsistent lifetime measurement")
		}
		counted += b.Agents
		var per *float64
		if b.ComparableCost != nil {
			n := *b.ComparableCost / float64(b.CostEligibleMessages)
			per = &n
		}
		fmt.Fprintf(&evidence, "| %s | %d | %d | %d | %s |\n", labels[i], b.Agents, b.CostEligibleAgents, b.CostEligibleMessages, price(per))
	}
	if counted != l.ChildSources-l.WithoutMessageObservations {
		return Measurement{}, fail(502, "incomplete lifetime population")
	}
	evidence.WriteString("\nOnly fully priced and attributed sources enter comparable costs. Missing attribution is not redistributed. These observations do not establish a causal benefit from a standing order or a model-tier choice. Fleet-wide tier-share comparisons are not yet available in this measurement.\n")
	updated, err := replaceMeasurementRegion(body, evidence.String())
	if err != nil {
		return Measurement{}, err
	}
	return Measurement{Body: updated, Sessions: int(c.Sessions), Subs: int(counted)}, nil
}

func replaceMeasurementRegion(body, evidence string) (string, error) {
	start, end := -1, -1
	separator := ""
	if strings.Count(body, measurementStart) == 1 && strings.Count(body, measurementEnd) == 1 {
		start = strings.Index(body, measurementStart)
		end = strings.Index(body, measurementEnd)
		if end < start || !measurementBoundaryLine(body, start, measurementStart) || !measurementBoundaryLine(body, end, measurementEnd) {
			return "", fail(409, "measurement markers are out of order; review the standing order")
		}
		end += len(measurementEnd)
	} else if !strings.Contains(body, measurementStart) && !strings.Contains(body, measurementEnd) && strings.Count(body, "_Measured ") == 1 {
		separator = "\n\n"
		// Exact legacy generator boundary; preserve its fixed policy and footer.
		start = strings.Index(body, "_Measured ")
		footer := "When a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**"
		if strings.Count(body, footer) == 1 {
			end = strings.Index(body, footer)
		}
		lineEnd := strings.Index(body[start:], "\n")
		if lineEnd < 0 || !legacyMeasurementHeader.MatchString(strings.TrimSuffix(body[start:start+lineEnd], "\r")) || (start > 0 && body[start-1] != '\n') || end < 0 || (end > 0 && body[end-1] != '\n') {
			end = -1
		}
	}
	if start < 0 || end <= start {
		return "", fail(409, "the standing order has no unambiguous generated measurement section; review its boundaries before remeasurement")
	}
	if measurementInsideFence(body, start) || measurementInsideFence(body, end-1) {
		return "", fail(409, "measurement boundaries occur inside a code example; review the standing order")
	}
	return body[:start] + measurementStart + "\n" + evidence + measurementEnd + separator + body[end:], nil
}

// A fenced example is user-authored text, never an editable measurement region.
// Track CommonMark backtick/tilde fences, including longer and unclosed fences.
func measurementInsideFence(body string, offset int) bool {
	var fence byte
	width := 0
	for position := 0; position <= offset; {
		lineEnd := strings.IndexByte(body[position:], '\n')
		if lineEnd < 0 {
			lineEnd = len(body)
		} else {
			lineEnd += position
		}
		if offset <= lineEnd {
			return fence != 0
		}
		line := strings.TrimSuffix(body[position:lineEnd], "\r")
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 3 {
			line = line[indent:]
			if len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
				count := 1
				for count < len(line) && line[count] == line[0] {
					count++
				}
				if fence == 0 && count >= 3 && (line[0] != '`' || !strings.ContainsRune(line[count:], '`')) {
					fence, width = line[0], count
				} else if fence == line[0] && count >= width && strings.Trim(line[count:], " \t") == "" {
					fence, width = 0, 0
				}
			}
		}
		position = lineEnd + 1
	}
	return fence != 0
}

func measurementBoundaryLine(body string, offset int, marker string) bool {
	start := strings.LastIndex(body[:offset], "\n") + 1
	end := strings.Index(body[offset:], "\n")
	if end < 0 {
		end = len(body)
	} else {
		end += offset
	}
	return strings.TrimSuffix(body[start:end], "\r") == marker
}
