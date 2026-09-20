package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func fixtureFleetMeasurement() store.EconomicsMeasurement {
	m := store.EconomicsMeasurement{Version: 1, MeasuredAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	m.Costs.Sessions = 400
	m.Costs.RecordedTokens = 100
	m.Costs.UnpricedOrUnmeasuredTokens = 100
	m.Lifetimes.Scope = "claude-child-usage-messages-v1"
	m.Lifetimes.ChildSources = 200
	for _, label := range []string{"1–2", "3–5", "6–10", "11–20", "21–40", "41–80", "81–160", "161+"} {
		m.Lifetimes.Buckets = append(m.Lifetimes.Buckets, store.LifetimeBucket{Label: label})
	}
	m.Lifetimes.Buckets[0].Agents = 200
	m.Lifetimes.Buckets[0].Messages = 400
	return m
}

func TestHubRemeasurementPreservesPolicyAndUnknownPrices(t *testing.T) {
	sample := fixtureFleetMeasurement()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error(r.Method)
		}
		json.NewEncoder(w).Encode(sample)
	}))
	defer h.Close()
	policy := "Owner policy: choose models deliberately.\n\n"
	footer := "When a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**, not intended."
	body := policy + "_Measured 2026-01-01 by Agent Mission Control from 2 sessions (200 Claude subagents). These figures refresh from the Economics view; do not hand-edit them:_\n\n- Old numeric claim.\n\n" + footer
	got, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions != 400 || got.Subs != 200 || !strings.HasPrefix(got.Body, policy) || !strings.HasSuffix(got.Body, footer) || strings.Contains(got.Body, "Old numeric claim") {
		t.Fatal(got)
	}
	if !strings.Contains(got.Body, "Known cost estimate: unavailable") || !strings.Contains(got.Body, "0 / 100 recorded tokens priced") || !strings.Contains(got.Body, "not establish a causal benefit") {
		t.Fatal(got.Body)
	}
	again, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, got.Body)
	if err != nil || again.Body != got.Body {
		t.Fatal("repeat drifted policy or whitespace", err)
	}
}

func TestHubRemeasurementRejectsIncompleteEvidenceAndAmbiguousPolicy(t *testing.T) {
	sample := fixtureFleetMeasurement()
	raw, _ := json.Marshal(sample)
	status := 200
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write(raw) }))
	defer h.Close()
	body := "Policy\n" + measurementStart + "\nold\n" + measurementEnd + "\nFooter"
	for _, bad := range []string{"unmarked policy", measurementEnd + measurementStart, body + measurementStart} {
		if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, bad); err == nil {
			t.Fatal("ambiguous body accepted")
		}
	}
	for _, bad := range []string{`{}`, `{"version":1`, strings.Repeat("x", 65537)} {
		raw = []byte(bad)
		if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body); err == nil {
			t.Fatal("invalid measurement accepted")
		}
	}
	sample.Lifetimes.ChildSources = 201
	raw, _ = json.Marshal(sample)
	if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body); err == nil {
		t.Fatal("population gap accepted")
	}
	status = 503
	if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body); err == nil {
		t.Fatal("hub error accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RemeasureFromHub(ctx, h.Client(), h.URL, body); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestHubMeasurementPreviewAppliesOnlyReviewedBodyToDisposableFile(t *testing.T) {
	f := newFixture(t)
	sample := fixtureFleetMeasurement()
	calls := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; json.NewEncoder(w).Encode(sample) }))
	defer h.Close()
	f.m.opts.RemeasureDirective = func(ctx context.Context, body string) (Measurement, error) {
		return RemeasureFromHub(ctx, h.Client(), h.URL, body)
	}
	body := "Preserve this owner policy.\n" + measurementStart + "\nold values\n" + measurementEnd + "\nPreserve this footer."
	mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "plant", "topic": "model-tiering", "body": body, "targets": []string{idFor(f.file)}})
	items, err := f.m.directiveItems()
	if err != nil {
		t.Fatal(err)
	}
	d := items[0]
	before, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	preview := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "remeasure-preview", "id": d.ID, "expectedHash": viewDirective(d).StateHash})
	after, err := os.ReadFile(f.file)
	if err != nil || string(before) != string(after) {
		t.Fatal("preview changed file", err)
	}
	if calls != 1 || !strings.Contains(preview["body"].(string), "0 / 100 recorded tokens priced") {
		t.Fatal(preview, calls)
	}
	sample.Costs.RecordedTokens = 999
	sample.Costs.UnpricedOrUnmeasuredTokens = 999
	result := mustRequest(t, f.m, "POST", "/api/directives", map[string]any{"op": "remeasure", "id": d.ID, "key": preview["previewId"], "expectedHash": preview["stateHash"]})
	if calls != 1 || result["body"] != preview["body"] || result["complete"] != true {
		t.Fatal("apply did not use sealed preview", result, calls)
	}
	after, err = os.ReadFile(f.file)
	if err != nil || !strings.Contains(string(after), "Preserve this owner policy.") || !strings.Contains(string(after), "Preserve this footer.") || strings.Contains(string(after), "999 recorded") {
		t.Fatal("policy or reviewed evidence changed", err)
	}
}

func TestHubMeasurementRejectsImpossibleChildPopulations(t *testing.T) {
	sample := fixtureFleetMeasurement()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(sample) }))
	defer h.Close()
	body := measurementStart + "\nold\n" + measurementEnd
	for _, mutate := range []func(*store.EconomicsMeasurement){
		func(m *store.EconomicsMeasurement) { m.Costs.Sessions = 199 },
		func(m *store.EconomicsMeasurement) { m.Lifetimes.Buckets[0].Messages = 199 },
		func(m *store.EconomicsMeasurement) {
			m.Lifetimes.Buckets[0].CostEligibleAgents = 2
			m.Lifetimes.Buckets[0].CostEligibleMessages = 1
		},
		func(m *store.EconomicsMeasurement) {
			n := 1.0
			m.Lifetimes.Buckets[0].ComparableCost = &n
			m.Lifetimes.Buckets[0].CostEligibleMessages = 1
		},
	} {
		sample = fixtureFleetMeasurement()
		mutate(&sample)
		if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body); err == nil {
			t.Fatal("impossible population accepted")
		}
	}
}

func TestRemeasureRejectsInlineExamplesBeforeReadingHub(t *testing.T) {
	calls := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(fixtureFleetMeasurement())
	}))
	defer h.Close()
	for _, body := range []string{
		"Example: " + measurementStart + "\npolicy to preserve\n" + measurementEnd,
		measurementStart + "\npolicy to preserve\n" + measurementEnd + " inline explanation",
		"_Measured made up by Agent Mission Control from something\nowner policy\nWhen a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**",
		strings.Repeat("x", 20001),
		"```markdown\n" + measurementStart + "\nexample\n" + measurementEnd + "\n```",
		"   ~~~~markdown\r\n" + measurementStart + "\r\nexample\r\n" + measurementEnd + "\r\n   ~~~~",
		"````\n```\n" + measurementStart + "\nexample\n" + measurementEnd,
		measurementStart + "\n```\nexample\n" + measurementEnd + "\n```",
		"```\n_Measured 2026-01-01 by Agent Mission Control from 2 sessions (200 Claude subagents). These figures refresh from the Economics view; do not hand-edit them:_\nexample\nWhen a workflow finishes, report a per-tier cost breakdown from the models that **actually ran**\n```",
	} {
		if _, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, body); err == nil {
			t.Fatal("ambiguous boundary accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid rule triggered expensive hub measurement", calls)
	}
	valid := "Owner policy\r\n" + measurementStart + "\r\nold\r\n" + measurementEnd + "\r\nOwner footer"
	if got, err := RemeasureFromHub(context.Background(), h.Client(), h.URL, valid); err != nil || !strings.HasPrefix(got.Body, "Owner policy\r\n") || !strings.HasSuffix(got.Body, "\r\nOwner footer") {
		t.Fatal("CRLF policy was not preserved", err)
	}
}

func TestRemeasureAllowsClosedCodeExamplesOutsideRegion(t *testing.T) {
	for _, prefix := range []string{
		"```markdown\nexample\n```\n",
		"~~~\nexample\n~~~~\n",
		"   ````text\nexample\n   `````\n",
		"```bad`info\n",
	} {
		body := prefix + measurementStart + "\nold\n" + measurementEnd + "\nFooter"
		got, err := replaceMeasurementRegion(body, "new\n")
		if err != nil || got != prefix+measurementStart+"\nnew\n"+measurementEnd+"\nFooter" {
			t.Fatalf("closed example changed or rejected: %q, %v", got, err)
		}
	}
}
