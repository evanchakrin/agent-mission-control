#!/usr/bin/env node
// Demo: simulates a CrewAI-style agent team emitting OpenTelemetry gen_ai spans
// to Agent Mission Control's OTLP endpoint. Run the dashboard, then:
//   node demo/simulate-crew.js [endpoint]   (default http://localhost:4173)
// Watch the "content-crew" session appear live in the Fleet view.
'use strict';
const ENDPOINT = (process.argv[2] || 'http://localhost:4173') + '/v1/traces';

const A = (key, v) => typeof v === 'number'
  ? { key, value: { intValue: v } }
  : { key, value: { stringValue: String(v) } };
const nano = ms => String(BigInt(Math.round(ms)) * 1000000n);
let spanN = 0;
const hex = (n) => (++spanN).toString(16).padStart(n, '0');

function span(agent, name, attrs, startMs, durMs, isError) {
  return {
    traceId: 'a1b2c3d4e5f60718a9b0c1d2e3f40516', spanId: hex(16), name,
    startTimeUnixNano: nano(startMs), endTimeUnixNano: nano(startMs + durMs),
    attributes: [A('gen_ai.agent.name', agent), ...attrs],
    status: { code: isError ? 2 : 1 },
  };
}

async function post(spans) {
  const body = {
    resourceSpans: [{
      resource: { attributes: [A('service.name', 'content-crew')] },
      scopeSpans: [{ scope: { name: 'demo' }, spans }],
    }],
  };
  const r = await fetch(ENDPOINT, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  console.log('posted', spans.length, 'spans →', r.status, await r.text());
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  console.log('Simulating a 3-agent content crew →', ENDPOINT);
  const t = () => Date.now();

  // Researcher works first
  await post([
    span('Researcher', 'web_search', [A('gen_ai.tool.name', 'web_search'), A('gen_ai.tool.call.arguments', 'query: AI agent observability market 2026')], t() - 4000, 2600),
  ]);
  await sleep(2500);
  await post([
    span('Researcher', 'chat claude-opus-5', [
      A('gen_ai.operation.name', 'chat'), A('gen_ai.request.model', 'claude-opus-5'),
      A('gen_ai.usage.input_tokens', 5230), A('gen_ai.usage.output_tokens', 812),
      A('gen_ai.completion', 'Research summary: agent observability tooling is fragmented; teams want one pane of glass across frameworks...'),
    ], t() - 3000, 2900),
  ]);
  await sleep(2500);

  // Writer drafts, hits one tool error, retries
  await post([
    span('Writer', 'read_notes', [A('gen_ai.tool.name', 'read_notes'), A('gen_ai.tool.call.arguments', 'notes/research.md')], t() - 1200, 300, true),
    span('Writer', 'read_notes', [A('gen_ai.tool.name', 'read_notes'), A('gen_ai.tool.call.arguments', 'notes/research-summary.md')], t() - 800, 250),
  ]);
  await sleep(2500);
  await post([
    span('Writer', 'chat claude-sonnet-5', [
      A('gen_ai.operation.name', 'chat'), A('gen_ai.request.model', 'claude-sonnet-5'),
      A('gen_ai.usage.input_tokens', 7480), A('gen_ai.usage.output_tokens', 2140),
      A('gen_ai.completion', 'Draft: "One Pane of Glass: Watching Your Agent Teams Work" — 1200 words...'),
    ], t() - 4200, 4100),
  ]);
  await sleep(2500);

  // Reviewer approves
  await post([
    span('Reviewer', 'chat claude-haiku-4-5', [
      A('gen_ai.operation.name', 'chat'), A('gen_ai.request.model', 'claude-haiku-4-5'),
      A('gen_ai.usage.input_tokens', 3150), A('gen_ai.usage.output_tokens', 420),
      A('gen_ai.completion', 'Review: approved with two minor edits. Ship it.'),
    ], t() - 1500, 1400),
  ]);
  console.log('done — check the dashboard');
})();
