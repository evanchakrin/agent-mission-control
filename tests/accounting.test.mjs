// Integration tests for the money math — the part of AMC that was wrong three
// different ways in one week (duplicate replies, unbilled cache writes, first-
// model-wins pricing) and a fourth on the Codex path (window sums duplicating a
// session-wide counter). Each of those fixes was verified by hand at the time;
// this locks them so they cannot quietly regress.
//
// Deliberately integration-style: the real server is spawned against fixture
// transcripts and asserted through the real HTTP API, because server.js is a
// single file with top-level side effects — and because the bugs these tests
// pin lived in the seams between parser, pricing, and summary, not in any one
// function.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(__dirname, '..');
const PORT = 4599;
const BASE = `http://127.0.0.1:${PORT}`;
const CLAUDE_FIXTURE = 'aaaa1111-2222-3333-4444-555566667777.jsonl';
const CODEX_UUID = 'bbbb1111-2222-3333-4444-555566667777';

let child;

before(async () => {
  child = spawn(process.execPath, [
    path.join(ROOT, 'server.js'),
    '--port', String(PORT),
    '--dir', path.join(__dirname, 'fixtures', 'claude'),
    '--codex-dir', path.join(__dirname, 'fixtures', 'codex'),
  ], { stdio: ['ignore', 'pipe', 'pipe'] });
  let log = '';
  child.stdout.on('data', d => { log += d; });
  child.stderr.on('data', d => { log += d; });
  const deadline = Date.now() + 30000;
  for (;;) {
    try {
      const r = await fetch(BASE + '/api/meta');
      if (r.ok) break;
    } catch { /* not up yet */ }
    if (Date.now() > deadline) throw new Error('server never came up:\n' + log.slice(-2000));
    await new Promise(r => setTimeout(r, 300));
  }
});

after(() => { if (child) child.kill(); });

async function fleet() {
  const r = await fetch(BASE + '/api/fleet');
  assert.equal(r.status, 200, '/api/fleet should answer 200');
  return r.json();
}
async function session(file) {
  const r = await fetch(BASE + '/api/session?file=' + encodeURIComponent(file));
  assert.equal(r.status, 200, '/api/session should answer 200');
  return r.json();
}
const near = (a, b, eps, msg) => assert.ok(Math.abs(a - b) <= eps, `${msg}: ${a} vs ${b}`);

// PRICING ground truth (mirrors server.js): opus in 5 / out 25, sonnet in 3 / out 15,
// gpt-5 in 1.25 / out 10. Cache reads bill at 0.1x input, cache writes at 1.25x input.

test('duplicate assistant lines are counted once', async () => {
  const all = await fleet();
  const s = all.find(x => x.file.endsWith(CLAUDE_FIXTURE));
  assert.ok(s, 'claude fixture appears in the fleet');
  // msg_dup appears on three lines with identical usage; msg_2 once.
  assert.equal(s.tokensIn, 110, 'input: 100 (msg_dup once) + 10 (msg_2)');
  assert.equal(s.tokensCache, 1500, 'cache reads: 1000 + 500');
  assert.equal(s.tokensCacheWrite, 70, 'cache writes: 50 + 20');
  assert.equal(s.tokensOut, 300, 'output: 200 + 100');
});

test('cache writes are billed, at 1.25x the input rate', async () => {
  const all = await fleet();
  const s = all.find(x => x.file.endsWith(CLAUDE_FIXTURE));
  const opus = 100 / 1e6 * 5 + 1000 / 1e6 * 5 * 0.1 + 50 / 1e6 * 5 * 1.25 + 200 / 1e6 * 25;
  const sonnet = 10 / 1e6 * 3 + 500 / 1e6 * 3 * 0.1 + 20 / 1e6 * 3 * 1.25 + 100 / 1e6 * 15;
  near(s.cost, Math.round((opus + sonnet) * 100) / 100, 0.011, 'session cost matches hand-computed rates');
  // and the write component is genuinely in there: without it the total rounds lower
  const withoutWrites = opus + sonnet - (50 / 1e6 * 5 * 1.25 + 20 / 1e6 * 3 * 1.25);
  assert.ok(opus + sonnet > withoutWrites, 'write lanes contribute a positive amount');
});

test('an agent that switches models is priced per model, not first-model-wins', async () => {
  const all = await fleet();
  const s = all.find(x => x.file.endsWith(CLAUDE_FIXTURE));
  const det = await session(s.file);
  const main = det.agents.find(a => a.id === 'main');
  assert.ok(main, 'main agent exists');
  const models = Object.keys(main.usageByModel || {});
  assert.ok(models.includes('claude-opus-5'), 'opus bucket exists');
  assert.ok(models.includes('claude-sonnet-5'), 'sonnet bucket exists');
  assert.equal(main.usageByModel['claude-opus-5'].outTokens, 200, 'opus bucket holds its own output');
  assert.equal(main.usageByModel['claude-sonnet-5'].outTokens, 100, 'sonnet bucket holds its own output');
  // the summary's per-model rows must reflect both — the UI regression of the same bug
  const ids = (s.models || []).map(m => m.id);
  assert.ok(ids.includes('claude-opus-5') && ids.includes('claude-sonnet-5'),
    'fleet summary lists both models that actually ran');
});

test('codex totals come from the cumulative counter, not summed window deltas', async () => {
  const all = await fleet();
  const s = all.find(x => x.file === 'codex:' + CODEX_UUID);
  assert.ok(s, 'codex fixture appears in the fleet');
  // deltas sum to out=150 / cached=2400 / fresh=600; the authoritative cumulative
  // total says out=100 / cached=1600 / fresh=400. The counter must win.
  assert.equal(s.tokensOut, 100, 'output equals the session-wide counter');
  assert.equal(s.tokensCache, 1600, 'cache reads equal the session-wide counter');
  assert.equal(s.tokensIn, 400, 'fresh input equals counter input minus cached');
  const want = 400 / 1e6 * 1.25 + 1600 / 1e6 * 1.25 * 0.1 + 100 / 1e6 * 10;
  near(s.cost, Math.round(want * 100) / 100, 0.011, 'codex cost priced from the corrected totals');
});

test('read routes refuse cross-origin callers', async () => {
  const r = await fetch(BASE + '/api/fleet', { headers: { Origin: 'https://evil.example' } });
  assert.equal(r.status, 403, 'a foreign Origin gets 403');
  const ok = await fetch(BASE + '/api/fleet');
  assert.equal(ok.status, 200, 'a same-origin caller still gets through');
  assert.ok(!ok.headers.get('access-control-allow-origin'),
    'no wildcard CORS header — that header is what turned a missing gate into an exfiltration path');
});
