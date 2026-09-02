// The delta endpoint's replace semantics — the part that, one hub restart after
// 7.33 shipped, rewrote 188 byte-complete mirrors from scratch and left six of
// them cut off mid-line where a five-minute upload died. Each case here is one
// of those failure shapes, asserted on the mirror bytes the hub actually keeps.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import net from 'node:net';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(__dirname, '..');
const PORT = 4598;
const BASE = `http://127.0.0.1:${PORT}`;
const STATE = fs.mkdtempSync(path.join(os.tmpdir(), 'amc-append-'));
const MIRROR = path.join(STATE, 'archive', 'testbox', 'claude', 'testproj', 'x.jsonl');

let child;
before(async () => {
  child = spawn(process.execPath, [
    path.join(ROOT, 'server.js'), '--port', String(PORT),
    '--dir', path.join(__dirname, 'fixtures', 'claude'),
    '--codex-dir', path.join(__dirname, 'fixtures', 'codex'),
  ], { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, AMC_STATE_DIR: STATE } });
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
  try { fs.rmSync(STATE, { recursive: true, force: true }); } catch { /* windows may still hold it */ }
});

const sha1 = b => crypto.createHash('sha1').update(b).digest('hex');
const anchorOf = (buf, off) => sha1(buf.subarray(Math.max(0, off - 64), off));
const append = (offset, body, declared = body.length, extra = {}) => fetch(BASE + '/v1/relay/append', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/octet-stream',
    'x-relay-machine': 'testbox',
    'x-relay-path': encodeURIComponent('claude/testproj/x.jsonl'),
    'x-relay-offset': String(offset),
    'x-relay-bytes': String(declared),
    ...extra,
  },
  body: body.length ? body : undefined,
});
const mirror = () => fs.readFileSync(MIRROR);
const settle = ms => new Promise(r => setTimeout(r, ms));

const A = Buffer.from('{"type":"user","message":{"role":"user","content":"one"}}\n{"type":"user","message":{"role":"user","content":"two"}}\n');
const B = Buffer.from('{"type":"user","message":{"role":"user","content":"three"}}\n');

test('a first upload lands whole', async () => {
  const r = await append(0, A);
  assert.equal(r.status, 200);
  assert.equal((await r.json()).size, A.length);
  assert.deepEqual(mirror(), A);
});

test('a relay that forgot its offsets is resumed from the mirror, not made to re-upload', async () => {
  // offset 0 with a declared size our mirror could be a prefix of → "you have this much"
  const r = await append(0, Buffer.concat([A, B]), A.length + B.length);
  assert.equal(r.status, 409, 'offset 0 over an existing prefix is a resume, not a replace');
  assert.equal((await r.json()).size, A.length, 'the hub reports its own size to resume from');
  assert.deepEqual(mirror(), A, 'the mirror was not touched');
  // the relay's next request: from our size, anchored on the bytes just before it
  const r2 = await append(A.length, B, B.length, { 'x-relay-anchor': anchorOf(Buffer.concat([A, B]), A.length) });
  assert.equal(r2.status, 200);
  assert.deepEqual(mirror(), Buffer.concat([A, B]), 'the tail was appended, nothing duplicated');
});

test('a replace that dies mid-body leaves the existing mirror intact', async () => {
  const before = mirror();
  await new Promise((resolve, reject) => {
    const s = net.connect(PORT, '127.0.0.1', () => {
      s.write([
        'POST /v1/relay/append HTTP/1.1', `Host: 127.0.0.1:${PORT}`,
        'Content-Type: application/octet-stream', 'x-relay-machine: testbox',
        'x-relay-path: ' + encodeURIComponent('claude/testproj/x.jsonl'),
        'x-relay-offset: 0', 'x-relay-bytes: 5000', 'Content-Length: 5000', '', '',
      ].join('\r\n'));
      s.write(Buffer.alloc(120, 0x7a)); // 120 of the promised 5000 bytes, then the wire drops
      setTimeout(() => { s.destroy(); resolve(); }, 150);
    });
    s.on('error', reject);
  });
  await settle(400);
  assert.deepEqual(mirror(), before, 'a mirror is never truncated by an upload that did not finish');
  assert.ok(!fs.existsSync(MIRROR + '.mc-replace'), 'the half-written sibling is cleaned up');
});

test('an anchor mismatch discards the mirror so the next offset-0 upload really replaces it', async () => {
  const cur = mirror().length;
  const r = await append(cur, B, B.length, { 'x-relay-anchor': sha1(Buffer.from('not the bytes we hold')) });
  assert.equal(r.status, 409);
  assert.equal((await r.json()).size, 0, 'size 0 tells the relay to start over');
  assert.ok(!fs.existsSync(MIRROR), 'the suspect mirror is gone, not kept as a prefix to bounce against');
  const r2 = await append(0, B);
  assert.equal(r2.status, 200, 'and the fresh upload is accepted as a replace');
  assert.deepEqual(mirror(), B);
});
