// The hub must never make a request wait for a transcript parse. Before 7.34
// every poll that followed a write to a large transcript re-parsed it on the
// only thread — 6-9s stalls, measured — and every relay upload in flight waited
// behind it. This spawns a hub over a generated ~9MB transcript, grows the file,
// and asserts that the next request answers at once (with the previous parse)
// while the fresh numbers arrive from the worker shortly after.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(__dirname, '..');
const PORT = 4597;
const BASE = `http://127.0.0.1:${PORT}`;
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), 'amc-offload-'));
const PROJ = path.join(TMP, 'projects', 'bigproj');
const FILE = path.join(PROJ, 'cccc1111-2222-3333-4444-555566667777.jsonl');

// one assistant reply = ~1KB with a fat text block; 200 output tokens each
const reply = n => JSON.stringify({
  type: 'assistant', uuid: 'u' + n, timestamp: new Date(1750000000000 + n * 1000).toISOString(),
  message: { id: 'msg_' + n, role: 'assistant', model: 'claude-sonnet-5',
    content: [{ type: 'text', text: 'reply ' + n + ' ' + 'x'.repeat(900) }],
    usage: { input_tokens: 10, output_tokens: 200, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 } },
}) + '\n';
const user = JSON.stringify({ type: 'user', uuid: 'u0', timestamp: new Date(1750000000000).toISOString(), message: { role: 'user', content: 'go' } }) + '\n';

let child;
before(async () => {
  fs.mkdirSync(PROJ, { recursive: true });
  const w = fs.openSync(FILE, 'w');
  fs.writeSync(w, user);
  for (let n = 1; n <= 9000; n++) fs.writeSync(w, reply(n));   // ≈9MB, well over the inline limit
  fs.closeSync(w);
  child = spawn(process.execPath, [
    path.join(ROOT, 'server.js'), '--port', String(PORT), '--dir', path.join(TMP, 'projects'),
    '--codex-dir', path.join(TMP, 'nocodex'),
  ], { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, AMC_STATE_DIR: path.join(TMP, 'state') } });
  let log = '';
  child.stdout.on('data', d => { log += d; });
  child.stderr.on('data', d => { log += d; });
  const deadline = Date.now() + 30000;
  for (;;) {
    try { if ((await fetch(BASE + '/api/meta')).ok) break; } catch { /* not up yet */ }
    if (Date.now() > deadline) throw new Error('server never came up:\n' + log.slice(-2000));
    await new Promise(r => setTimeout(r, 300));
  }
});
after(() => {
  if (child) child.kill();
  try { fs.rmSync(TMP, { recursive: true, force: true }); } catch { /* windows may still hold it */ }
});

const fleetRow = async () => (await (await fetch(BASE + '/api/fleet')).json()).find(s => s.file.endsWith('cccc1111-2222-3333-4444-555566667777.jsonl'));
const timed = async fn => { const t = process.hrtime.bigint(); const v = await fn(); return { v, ms: Number(process.hrtime.bigint() - t) / 1e6 }; };

test('a grown transcript answers with the last parse at once and refreshes off-thread', async () => {
  // first sight: parsed inline, whatever that costs
  const first = await timed(fleetRow);
  assert.ok(first.v, 'the big transcript is in the fleet');
  assert.equal(first.v.tokensOut, 9000 * 200, 'the cold parse is exact');
  // grow the file: 1000 more replies
  const w = fs.openSync(FILE, 'a');
  for (let n = 9001; n <= 10000; n++) fs.writeSync(w, reply(n));
  fs.closeSync(w);
  // the very next request must not wait for the reparse
  const second = await timed(fleetRow);
  assert.ok(second.ms < 1500, `a changed 10MB transcript answered in ${second.ms.toFixed(0)}ms — it waited for a parse`);
  assert.equal(second.v.tokensOut, 9000 * 200, 'the stale answer is the previous parse, not a half-parse');
  // and the fresh numbers land shortly after
  const deadline = Date.now() + 20000;
  let row = second.v;
  while (row.tokensOut !== 10000 * 200 && Date.now() < deadline) {
    await new Promise(r => setTimeout(r, 250));
    row = await fleetRow();
  }
  assert.equal(row.tokensOut, 10000 * 200, 'the worker\'s parse replaced the stale one');
});

test('a request during the background parse is not blocked either', async () => {
  const w = fs.openSync(FILE, 'a');
  for (let n = 10001; n <= 12000; n++) fs.writeSync(w, reply(n));
  fs.closeSync(w);
  await fleetRow(); // kicks the refresh
  const meta = await timed(() => fetch(BASE + '/api/meta').then(r => r.json()));
  assert.ok(meta.ms < 500, `/api/meta took ${meta.ms.toFixed(0)}ms while a parse was running`);
});
