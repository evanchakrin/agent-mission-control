#!/usr/bin/env node
// Agent Mission Control v2.0 — live dashboard over Claude Code session transcripts.
// Zero dependencies: node server.js [--port 4173] [--dir <projects dir>]
//
// Data sources per session:
//   <projects>/<slug>/<sessionId>.jsonl                     main (orchestrator) transcript
//   <projects>/<slug>/<sessionId>/subagents/**/agent-*.jsonl  subagent + workflow agent transcripts
//   <projects>/<slug>/<sessionId>/workflows/wf_*.json       workflow metadata (names)
//   legacy: isSidechain:true lines inside the main file     older-style inline subagents
'use strict';
const http = require('http');
const fs = require('fs');
const path = require('path');
const os = require('os');

function argValue(flag) {
  const i = process.argv.indexOf(flag);
  return i > -1 ? process.argv[i + 1] : null;
}
const PORT = Number(argValue('--port') || process.env.PORT || 4173);
const PROJECTS_DIR = argValue('--dir') || process.env.CLAUDE_PROJECTS_DIR || path.join(os.homedir(), '.claude', 'projects');
const PUBLIC_DIR = path.join(__dirname, 'public');

// $/MTok, matched by substring of the model id; cache reads bill at 0.1x input.
const PRICING = [
  { m: 'fable', in: 10, out: 50 }, { m: 'mythos', in: 10, out: 50 },
  { m: 'opus', in: 5, out: 25 }, { m: 'sonnet', in: 3, out: 15 }, { m: 'haiku', in: 1, out: 5 },
];
function costOf(a) {
  const p = PRICING.find(x => (a.model || '').includes(x.m)) || { in: 5, out: 25 };
  return (a.inTokens / 1e6) * p.in + (a.cacheTokens / 1e6) * p.in * 0.1 + (a.outTokens / 1e6) * p.out;
}

// ---------- helpers ----------

function safeParse(line) {
  try { return JSON.parse(line); } catch { return null; }
}

function textOfContent(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.filter(b => b && b.type === 'text').map(b => b.text).join('\n');
  }
  return '';
}

function summarizeInput(input) {
  if (!input || typeof input !== 'object') return '';
  const keys = ['description', 'prompt', 'command', 'file_path', 'pattern', 'query', 'url', 'skill'];
  for (const k of keys) {
    if (typeof input[k] === 'string' && input[k].trim()) return input[k];
  }
  return JSON.stringify(input);
}

function clip(s, n) { s = String(s || ''); return s.length > n ? s.slice(0, n - 1) + '…' : s; }

function newAgent(id, type) {
  return {
    id, name: null, type, task: '', model: null,
    firstTs: null, lastTs: null, events: 0, tools: {},
    inTokens: 0, cacheTokens: 0, outTokens: 0, errors: 0, done: false, lastKind: null,
  };
}

// ---------- transcript parsing ----------

// Extract normalized events from one user/assistant line into ctx.
function processLine(o, agentId, ctx) {
  const { agents, events, spawnCalls, pending } = ctx;
  const ts = o.timestamp || null;
  if (!agents.has(agentId)) agents.set(agentId, newAgent(agentId, agentId === 'main' ? 'main' : 'subagent'));
  const agent = agents.get(agentId);
  if (!agent.firstTs) agent.firstTs = ts;
  agent.lastTs = ts;
  agent.events++;

  const msg = o.message || {};
  const content = msg.content;

  if (o.type === 'assistant') {
    const usage = msg.usage || {};
    agent.inTokens += usage.input_tokens || 0;
    agent.cacheTokens += usage.cache_read_input_tokens || 0;
    agent.outTokens += usage.output_tokens || 0;
    if (msg.model && !agent.model) agent.model = msg.model;

    const text = textOfContent(content);
    if (text.trim()) {
      events.push({ ts, agent: agentId, kind: 'assistant-text', text: clip(text, 240), full: clip(text, 2500) });
      agent.lastKind = 'assistant-text';
    }
    if (Array.isArray(content)) {
      for (const b of content) {
        if (b && b.type === 'tool_use') {
          const isSpawn = agentId === 'main' && (b.name === 'Task' || b.name === 'Agent');
          const summary = summarizeInput(b.input);
          if (isSpawn) {
            const label = (b.input && (b.input.description || b.input.subagent_type)) || 'subagent';
            spawnCalls.push({ toolUseId: b.id, name: label, prompt: clip(summary, 200), ts, resolved: false, agentId: null });
          }
          agent.tools[b.name] = (agent.tools[b.name] || 0) + 1;
          agent.lastKind = 'tool-call';
          const evt = {
            ts, agent: agentId, kind: isSpawn ? 'spawn' : 'tool-call',
            tool: b.name, toolUseId: b.id, text: clip(summary, 240), full: clip(summary, 2500),
          };
          events.push(evt);
          if (b.id) pending.set(b.id, evt); // paired with its tool_result for duration
        }
      }
    }
  } else { // user line: real user text or tool results
    if (Array.isArray(content)) {
      for (const b of content) {
        if (b && b.type === 'tool_result') {
          const rtext = textOfContent(b.content);
          const spawn = spawnCalls.find(s => s.toolUseId === b.tool_use_id);
          if (spawn) {
            spawn.resolved = true;
            const m = /agentId:\s*([a-z0-9]+)/i.exec(rtext);
            if (m) spawn.agentId = m[1];
          }
          const start = pending.get(b.tool_use_id);
          if (start && !start.endTs && ts) start.endTs = ts;
          if (b.is_error) agent.errors++;
          events.push({
            ts, agent: agentId, kind: spawn ? 'spawn-result' : 'tool-result',
            toolUseId: b.tool_use_id, error: !!b.is_error, text: clip(rtext, 240), full: clip(rtext, 2500),
          });
        }
      }
      const text = textOfContent(content);
      if (text.trim() && !o.isMeta) events.push({ ts, agent: agentId, kind: 'user-text', text: clip(text, 240), full: clip(text, 2500) });
    } else if (typeof content === 'string' && content.trim() && !o.isMeta) {
      events.push({ ts, agent: agentId, kind: 'user-text', text: clip(content, 240), full: clip(content, 2500) });
    }
  }
}

// mainLines: JSONL lines of the orchestrator transcript.
// subFiles: [{id, lines, group}] — one per subagents/**/agent-*.jsonl file.
function normalize(mainLines, subFiles, wfNames = new Map()) {
  const ctx = { agents: new Map(), events: [], spawnCalls: [], pending: new Map() };
  ctx.agents.set('main', { ...newAgent('main', 'main'), name: 'Orchestrator' });

  // --- main file (with legacy inline-sidechain grouping) ---
  const raw = [];
  for (const line of mainLines) {
    if (!line.trim()) continue;
    const o = safeParse(line);
    if (o) raw.push(o);
  }
  const byUuid = new Map();
  for (const o of raw) if (o.uuid) byUuid.set(o.uuid, o);
  const rootCache = new Map();
  function sidechainRoot(o) {
    if (rootCache.has(o.uuid)) return rootCache.get(o.uuid);
    let cur = o;
    const seen = [o.uuid];
    while (cur && cur.isSidechain && cur.parentUuid && byUuid.has(cur.parentUuid)) {
      const parent = byUuid.get(cur.parentUuid);
      if (!parent.isSidechain) break;
      cur = parent;
      seen.push(cur.uuid);
    }
    const root = cur.uuid;
    for (const u of seen) if (u) rootCache.set(u, root);
    return root;
  }

  for (const o of raw) {
    if (o.type === 'queue-operation' && o.operation === 'enqueue') {
      ctx.events.push({ ts: o.timestamp || null, agent: 'main', kind: 'user-queued', text: clip(o.content, 240), full: clip(o.content, 2500) });
      continue;
    }
    if (o.type !== 'user' && o.type !== 'assistant') continue;
    const agentId = o.isSidechain ? 'sub:' + sidechainRoot(o) : 'main';
    processLine(o, agentId, ctx);
  }

  // --- separate subagent transcript files ---
  const groupOf = new Map();
  for (const sf of subFiles) {
    const agentId = 'sub:' + sf.id;
    if (sf.group) groupOf.set(agentId, sf.group);
    for (const line of sf.lines) {
      if (!line.trim()) continue;
      const o = safeParse(line);
      if (!o || (o.type !== 'user' && o.type !== 'assistant')) continue;
      processLine(o, agentId, ctx);
    }
  }

  // --- pair subagents with spawn calls: exact by agentId, else by start-time order ---
  const subs = [...ctx.agents.values()].filter(a => a.id !== 'main')
    .sort((a, b) => String(a.firstTs).localeCompare(String(b.firstTs)));
  const spawns = [...ctx.spawnCalls].sort((a, b) => String(a.ts).localeCompare(String(b.ts)));

  const claimedSpawns = new Set();
  const spawnFor = new Map(); // agent.id -> spawn
  for (const a of subs) {
    if (groupOf.has(a.id)) continue; // workflow agents are named from workflow meta, not spawn calls
    const fileId = a.id.slice(4); // strip 'sub:'
    const exact = spawns.find(s => s.agentId && fileId.startsWith(s.agentId));
    if (exact) { spawnFor.set(a.id, exact); claimedSpawns.add(exact); }
  }
  let fi = 0;
  for (const a of subs) {
    if (spawnFor.has(a.id) || groupOf.has(a.id)) continue;
    while (fi < spawns.length && claimedSpawns.has(spawns[fi])) fi++;
    if (fi < spawns.length) { spawnFor.set(a.id, spawns[fi]); claimedSpawns.add(spawns[fi]); }
  }
  const groupCounter = new Map();
  subs.forEach((a, i) => {
    const grp = groupOf.get(a.id);
    if (grp) {
      const n = (groupCounter.get(grp) || 0) + 1;
      groupCounter.set(grp, n);
      a.group = grp;
      a.groupName = wfNames.get(grp) || grp;
      a.name = `${a.groupName} #${n}`;
      const firstUser = ctx.events.find(e => e.agent === a.id && e.kind === 'user-text');
      a.task = firstUser ? firstUser.text : '';
      a.done = a.lastKind === 'assistant-text';
      return;
    }
    const sp = spawnFor.get(a.id);
    a.name = sp ? sp.name : 'Subagent ' + (i + 1);
    a.task = sp ? sp.prompt : '';
    a.done = (sp && sp.resolved) || a.lastKind === 'assistant-text';
  });

  for (const a of ctx.agents.values()) a.cost = Math.round(costOf(a) * 10000) / 10000;

  // one global timeline, then tag spawn events with the agent they created
  ctx.events.sort((a, b) => String(a.ts).localeCompare(String(b.ts)));
  const agentBySpawn = new Map();
  for (const [aid, sp] of spawnFor) agentBySpawn.set(sp.toolUseId, aid);
  for (const e of ctx.events) {
    if (e.kind === 'spawn') e.spawnedAgent = agentBySpawn.get(e.toolUseId) || null;
    if (e.kind === 'spawn-result') e.spawnedAgent = agentBySpawn.get(e.toolUseId) || null;
  }
  ctx.events.forEach((e, i) => { e.seq = i; });

  return { events: ctx.events, agents: [...ctx.agents.values()] };
}

// ---------- session discovery & reading ----------

function listSessions() {
  const out = [];
  let projects = [];
  try { projects = fs.readdirSync(PROJECTS_DIR); } catch { return out; }
  for (const proj of projects) {
    const dir = path.join(PROJECTS_DIR, proj);
    let files = [];
    try { files = fs.readdirSync(dir).filter(f => f.endsWith('.jsonl')); } catch { continue; }
    for (const f of files) {
      const full = path.join(dir, f);
      let st;
      try { st = fs.statSync(full); } catch { continue; }
      let title = null;
      try {
        const fd = fs.openSync(full, 'r');
        const buf = Buffer.alloc(Math.min(8192, st.size));
        fs.readSync(fd, buf, 0, buf.length, 0);
        fs.closeSync(fd);
        for (const line of buf.toString('utf8').split('\n')) {
          const o = safeParse(line);
          if (o && (o.type === 'custom-title' || o.type === 'ai-title')) { title = o.customTitle || o.aiTitle; if (o.type === 'custom-title') break; }
        }
      } catch { /* ignore */ }
      const agentCount = subagentFiles(full).length;
      out.push({ project: proj, file: path.join(proj, f), session: f.replace('.jsonl', ''), title, size: st.size, mtime: st.mtimeMs, agentCount });
    }
  }
  out.sort((a, b) => b.mtime - a.mtime);
  return out;
}

function resolveSessionPath(rel) {
  const full = path.resolve(PROJECTS_DIR, rel);
  if (!full.startsWith(path.resolve(PROJECTS_DIR)) || !full.endsWith('.jsonl')) return null;
  return full;
}

// Recursively find every agent-*.jsonl under <session>/subagents. Workflow
// agents live in subagents/workflows/<wf_runId>/agent-*.jsonl — the run id
// becomes the agent's group.
function subagentFiles(sessionPath) {
  const base = path.join(sessionPath.slice(0, -'.jsonl'.length), 'subagents');
  const out = [];
  function walk(dir, group) {
    let entries = [];
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) {
        walk(full, e.name.startsWith('wf_') ? e.name : group);
      } else if (e.name.endsWith('.jsonl')) {
        out.push({ id: e.name.replace(/^agent-/, '').replace('.jsonl', ''), path: full, group });
      }
    }
  }
  walk(base, null);
  return out;
}

// Workflow run id -> human name, parsed from <session>/workflows/*.json metadata.
function workflowNames(sessionPath) {
  const dir = path.join(sessionPath.slice(0, -'.jsonl'.length), 'workflows');
  const names = new Map();
  let files = [];
  try { files = fs.readdirSync(dir).filter(f => f.endsWith('.json')); } catch { return names; }
  for (const f of files) {
    try {
      const o = safeParse(fs.readFileSync(path.join(dir, f), 'utf8'));
      if (o && o.runId) {
        const m = /name:\s*['"]([^'"]+)['"]/.exec(o.script || '');
        names.set(o.runId, m ? m[1] : o.runId);
      }
    } catch { /* ignore */ }
  }
  return names;
}

// Change signature = main size + every subagent file size (detects growth anywhere).
function sessionSignature(sessionPath) {
  let sig = '';
  try { sig += fs.statSync(sessionPath).size; } catch { /* ignore */ }
  for (const sf of subagentFiles(sessionPath)) {
    try { sig += ':' + sf.id + '=' + fs.statSync(sf.path).size; } catch { /* ignore */ }
  }
  return sig;
}

// Parse cache: whole-file reparse only when the signature changes; every open
// SSE client, the REST endpoints, and the fleet view share the parsed result.
const cache = new Map(); // sessionPath -> {sig, result}
function readSession(sessionPath) {
  const sig = sessionSignature(sessionPath);
  const hit = cache.get(sessionPath);
  if (hit && hit.sig === sig) return hit.result;
  const text = fs.readFileSync(sessionPath, 'utf8');
  const subs = subagentFiles(sessionPath).map(sf => {
    let t = '';
    try { t = fs.readFileSync(sf.path, 'utf8'); } catch { /* ignore */ }
    return { id: sf.id, lines: t.split('\n'), group: sf.group };
  });
  const result = normalize(text.split('\n'), subs, workflowNames(sessionPath));
  cache.set(sessionPath, { sig, result });
  if (cache.size > 8) cache.delete(cache.keys().next().value);
  return result;
}

function sessionSummary(meta) {
  const full = resolveSessionPath(meta.file);
  if (!full) return null;
  let r;
  try { r = readSession(full); } catch { return null; }
  const evs = r.events;
  const first = evs.find(e => e.ts), last = [...evs].reverse().find(e => e.ts);
  return {
    file: meta.file, project: meta.project, session: meta.session, title: meta.title, mtime: meta.mtime,
    agents: r.agents.length, events: evs.length,
    toolCalls: evs.filter(e => e.kind === 'tool-call' || e.kind === 'spawn').length,
    errors: evs.filter(e => e.error).length,
    durationMs: first && last ? new Date(last.ts) - new Date(first.ts) : 0,
    tokensIn: r.agents.reduce((n, a) => n + a.inTokens + a.cacheTokens, 0),
    tokensOut: r.agents.reduce((n, a) => n + a.outTokens, 0),
    cost: Math.round(r.agents.reduce((n, a) => n + (a.cost || 0), 0) * 100) / 100,
  };
}

// ---------- standalone replay export ----------

function buildExport(sessionPath, title) {
  const html = fs.readFileSync(path.join(PUBLIC_DIR, 'index.html'), 'utf8');
  const css = fs.readFileSync(path.join(PUBLIC_DIR, 'style.css'), 'utf8');
  const js = fs.readFileSync(path.join(PUBLIC_DIR, 'app.js'), 'utf8');
  const data = { ...readSession(sessionPath), now: Date.now() };
  const baked = `<script>window.__BAKED__=${JSON.stringify({ title: title || 'Session replay', data }).replace(/</g, '\\u003c')}</script>`;
  return html
    .replace('<link rel="stylesheet" href="/style.css">', `<style>\n${css}\n</style>`)
    .replace('<script src="/app.js"></script>', `${baked}\n<script>\n${js}\n</script>`);
}

// ---------- http server ----------

const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml' };

function json(res, obj, code = 200) {
  res.writeHead(code, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*' });
  res.end(JSON.stringify(obj));
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');

  if (url.pathname === '/api/sessions') return json(res, listSessions());

  if (url.pathname === '/api/fleet') {
    return json(res, listSessions().map(sessionSummary).filter(Boolean));
  }

  if (url.pathname === '/api/session') {
    const full = resolveSessionPath(url.searchParams.get('file') || '');
    if (!full || !fs.existsSync(full)) return json(res, { error: 'not found' }, 404);
    return json(res, { ...readSession(full), now: Date.now() });
  }

  if (url.pathname === '/api/export') {
    const full = resolveSessionPath(url.searchParams.get('file') || '');
    if (!full || !fs.existsSync(full)) { res.writeHead(404); res.end(); return; }
    const name = (url.searchParams.get('title') || path.basename(full, '.jsonl')).replace(/[^\w.-]+/g, '-');
    res.writeHead(200, {
      'Content-Type': 'text/html; charset=utf-8',
      'Content-Disposition': `attachment; filename="mission-control-replay-${name}.html"`,
    });
    res.end(buildExport(full, url.searchParams.get('title')));
    return;
  }

  if (url.pathname === '/api/stream') {
    const full = resolveSessionPath(url.searchParams.get('file') || '');
    if (!full || !fs.existsSync(full)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      'Connection': 'keep-alive',
      'Access-Control-Allow-Origin': '*',
    });
    let lastSig = null;
    const tick = () => {
      const sig = sessionSignature(full);
      if (sig !== lastSig) {
        lastSig = sig;
        try {
          res.write('data: ' + JSON.stringify({ ...readSession(full), now: Date.now() }) + '\n\n');
        } catch { /* mid-write read; next tick catches up */ }
      } else {
        res.write(': keepalive\n\n');
      }
    };
    tick();
    const timer = setInterval(tick, 700);
    req.on('close', () => clearInterval(timer));
    return;
  }

  // static files
  const rel = url.pathname === '/' ? '/index.html' : url.pathname;
  const file = path.resolve(PUBLIC_DIR, '.' + rel);
  if (file.startsWith(path.resolve(PUBLIC_DIR)) && fs.existsSync(file) && fs.statSync(file).isFile()) {
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
    res.end(fs.readFileSync(file));
    return;
  }

  res.writeHead(404); res.end('not found');
});

server.listen(PORT, () => {
  console.log(`Agent Mission Control → http://localhost:${PORT}`);
  console.log(`Watching transcripts in ${PROJECTS_DIR}`);
});
