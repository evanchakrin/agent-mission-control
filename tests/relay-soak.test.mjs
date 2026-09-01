// Relay OOM soak — pins the 2026-08-31 incident: three heap OOMs on a
// production relay, each one a large ACTIVE session being re-parsed and
// re-POSTed whole on every append ("sent ... (14049KB)" back-to-back until
// FATAL at the 1024MB cap).
//
// The test runs a real hub and a real relay, gives the relay a ~15MB live
// transcript, appends to it in a loop, and asserts:
//   1. the relay SURVIVES under a deliberately tiny 96MB heap — impossible if
//      the live path still buffers whole files or parses them locally;
//   2. later sends are DELTAS (a few hundred KB), not 15MB resends;
//   3. the hub's raw mirror converges to the exact local byte size.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(__dirname, '..');
const PORT = 4601;
const BASE = `http://127.0.0.1:${PORT}`;
const SESSION = 'cccc1111-2222-3333-4444-555566667777';

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'amc-soak-'));
const hubState = path.join(tmp, 'hub-state');
const relayState = path.join(tmp, 'relay-state');
const hubProjects = path.join(tmp, 'hub-projects');   // empty: the hub has no local sessions
const relayProjects = path.join(tmp, 'relay-projects');
const emptyCodex = path.join(tmp, 'codex');
const projDir = path.join(relayProjects, 'soakproj');
const transcript = path.join(projDir, SESSION + '.jsonl');
for (const d of [hubState, relayState, hubProjects, projDir, emptyCodex]) fs.mkdirSync(d, { recursive: true });

// a real-shaped assistant line, padded to ~2KB so the file reaches size fast
let seq = 0;
function line() {
  seq++;
  return JSON.stringify({
    type: 'assistant', uuid: 'a' + seq, parentUuid: 'u1', timestamp: new Date(1767225600000 + seq * 1000).toISOString(),
    message: { id: 'msg_' + seq, role: 'assistant', model: 'claude-opus-5',
      usage: { input_tokens: 10, cache_read_input_tokens: 100, cache_creation_input_tokens: 5, output_tokens: 20 },
      content: [{ type: 'text', text: 'soak filler '.repeat(150) + seq }] },
  }) + '\n';
}

let hub, relay, relayLog = '', hubLog = '';

before(async () => {
  // seed ~15MB before the relay ever sees the file
  const chunks = ['{"type":"user","uuid":"u1","timestamp":"2026-01-01T10:00:00.000Z","message":{"role":"user","content":"soak session"}}\n'];
  let bytes = chunks[0].length;
  while (bytes < 15 * 1024 * 1024) { const l = line(); bytes += l.length; chunks.push(l); }
  fs.writeFileSync(transcript, chunks.join(''));

  hub = spawn(process.execPath, [path.join(ROOT, 'server.js'), '--port', String(PORT), '--dir', hubProjects, '--codex-dir', emptyCodex],
    { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, AMC_STATE_DIR: hubState } });
  hub.stdout.on('data', d => { hubLog += d; });
  hub.stderr.on('data', d => { hubLog += d; });
  const deadline = Date.now() + 30000;
  for (;;) {
    try { if ((await fetch(BASE + '/api/meta')).ok) break; } catch { /* not up yet */ }
    if (Date.now() > deadline) throw new Error('hub never came up:\n' + hubLog.slice(-2000));
    await new Promise(r => setTimeout(r, 300));
  }

  // 96MB heap: parsing or buffering the 15MB transcript even once would die here
  relay = spawn(process.execPath, ['--max-old-space-size=96', path.join(ROOT, 'server.js'),
    '--relay', BASE, '--name', 'soak-test', '--dir', relayProjects, '--codex-dir', emptyCodex],
    { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, AMC_STATE_DIR: relayState, AMC_RELAY_DEBOUNCE_MS: '1000' } });
  relay.stdout.on('data', d => { relayLog += d; });
  relay.stderr.on('data', d => { relayLog += d; });
});

after(() => { for (const c of [relay, hub]) if (c) c.kill(); try { fs.rmSync(tmp, { recursive: true, force: true }); } catch { /* Windows file locks */ } });

function relayRssMB(pid) {
  try {
    if (process.platform === 'win32') {
      const out = execSync(`tasklist /FI "PID eq ${pid}" /FO CSV /NH`, { encoding: 'utf8' });
      const m = /"([\d.,  ]+) K"/.exec(out);
      return m ? Number(m[1].replace(/[^\d]/g, '')) / 1024 : null;
    }
    return Number(execSync(`ps -o rss= -p ${pid}`, { encoding: 'utf8' }).trim()) / 1024;
  } catch { return null; }
}

test('relay survives a chatty 15MB transcript on a 96MB heap and sends deltas', { timeout: 180000 }, async () => {
  // wait for the first (full, streamed) send
  let deadline = Date.now() + 60000;
  while (!/sent Δ/.test(relayLog)) {
    assert.equal(relay.exitCode, null, 'relay died during the first send:\n' + relayLog.slice(-2000));
    if (Date.now() > deadline) throw new Error('first send never happened:\n' + relayLog.slice(-2000) + hubLog.slice(-1000));
    await new Promise(r => setTimeout(r, 500));
  }

  // hammer appends for ~30s — the incident pattern, compressed
  const end = Date.now() + 30000;
  while (Date.now() < end) {
    let chunk = '';
    while (chunk.length < 64 * 1024) chunk += line();
    fs.appendFileSync(transcript, chunk);
    assert.equal(relay.exitCode, null, 'relay died mid-soak:\n' + relayLog.slice(-2000));
    await new Promise(r => setTimeout(r, 500));
  }

  // let the last debounced delta go out
  const finalSize = fs.statSync(transcript).size;
  deadline = Date.now() + 30000;
  const mirror = path.join(hubState, 'archive', 'soak-test', 'claude', 'soakproj', SESSION + '.jsonl');
  for (;;) {
    let ms = 0;
    try { ms = fs.statSync(mirror).size; } catch { /* not yet */ }
    if (ms === finalSize) break;
    if (Date.now() > deadline) throw new Error(`hub mirror never converged (${ms} vs ${finalSize}):\n` + relayLog.slice(-2000) + hubLog.slice(-1000));
    await new Promise(r => setTimeout(r, 500));
  }

  assert.equal(relay.exitCode, null, 'relay OOMd:\n' + relayLog.slice(-2000));

  const sends = [...relayLog.matchAll(/sent Δ .*\(\+(\d+) bytes\)/g)].map(m => Number(m[1]));
  assert.ok(sends.length >= 3, `expected several delta sends, saw ${sends.length}:\n` + relayLog.slice(-2000));
  // every send after the first must be a tail, not a 15MB resend
  for (const n of sends.slice(1)) assert.ok(n < 5 * 1024 * 1024, `a follow-up send moved ${n} bytes — that is a full resend, not a delta`);
  assert.ok(sends.slice(1).some(n => n > 0), 'no follow-up delta actually carried bytes');

  // the hub shows the relayed session in the fleet
  const fleet = await fetch(BASE + '/api/fleet').then(r => r.json());
  assert.ok(JSON.stringify(fleet).includes('relay:soak-test'), 'relayed session missing from /api/fleet');

  // RSS sanity where the platform lets us read it (heap cap already enforces the real bound)
  const rss = relayRssMB(relay.pid);
  if (rss != null) assert.ok(rss < 350, `relay RSS ${Math.round(rss)}MB — not the flat profile the delta path promises`);
});
