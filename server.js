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
  const p = PRICING.find(x => (a.model || '').includes(x.m));
  if (!p) return 0; // unknown model (e.g. non-Claude via OTLP): no estimate rather than a wrong one
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
    inTokens: 0, cacheTokens: 0, outTokens: 0, errors: 0, cost: 0, done: false, lastKind: null,
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

// ---------- OTLP/HTTP trace ingestion (OpenTelemetry) ----------
// POST /v1/traces with OTLP-JSON (set OTEL_EXPORTER_OTLP_PROTOCOL=http/json and
// OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4173 in any instrumented agent
// framework). Spans using the gen_ai.* semantic conventions map onto the same
// agent/event model as Claude Code transcripts, keyed "otel:<service.name>".
const otelSessions = new Map(); // id -> {id,name,started,version,agents:Map,events:[]}

function otelVal(v) {
  if (v == null) return null;
  if (v.stringValue !== undefined) return v.stringValue;
  if (v.intValue !== undefined) return Number(v.intValue);
  if (v.doubleValue !== undefined) return v.doubleValue;
  if (v.boolValue !== undefined) return v.boolValue;
  if (v.arrayValue) return (v.arrayValue.values || []).map(otelVal).join(', ');
  return null;
}
function otelAttrs(list) {
  const o = {};
  for (const a of list || []) o[a.key] = otelVal(a.value);
  return o;
}

function ingestTraces(body) {
  let spans = 0;
  for (const rs of body.resourceSpans || []) {
    const res = otelAttrs(rs.resource && rs.resource.attributes);
    const service = res['service.name'] || 'otel-agent';
    const sid = 'otel:' + service;
    if (!otelSessions.has(sid)) {
      const s = { id: sid, name: service, started: Date.now(), version: 0, agents: new Map(), events: [] };
      s.agents.set('main', { ...newAgent('main', 'main'), name: service });
      otelSessions.set(sid, s);
    }
    const sess = otelSessions.get(sid);
    sess.version++;
    for (const ss of rs.scopeSpans || []) {
      for (const span of ss.spans || []) {
        spans++;
        const at = otelAttrs(span.attributes);
        const ts = span.startTimeUnixNano ? new Date(Number(span.startTimeUnixNano) / 1e6).toISOString() : null;
        const endTs = span.endTimeUnixNano ? new Date(Number(span.endTimeUnixNano) / 1e6).toISOString() : null;
        const agentName = at['gen_ai.agent.name'] || at['agent.name'] || at['crewai.agent.role'] || null;
        const agentId = agentName ? 'sub:' + agentName : 'main';
        if (!sess.agents.has(agentId)) sess.agents.set(agentId, { ...newAgent(agentId, 'subagent'), name: agentName });
        const agent = sess.agents.get(agentId);
        if (!agent.firstTs || (ts && ts < agent.firstTs)) agent.firstTs = ts;
        if (!agent.lastTs || (endTs || ts) > agent.lastTs) agent.lastTs = endTs || ts;
        agent.events++;
        const model = at['gen_ai.request.model'] || at['gen_ai.response.model'];
        if (model && !agent.model) agent.model = model;
        const error = !!(span.status && span.status.code === 2);
        if (error) agent.errors++;
        const op = String(at['gen_ai.operation.name'] || span.name || '').toLowerCase();
        if (model || /chat|completion|generate/.test(op)) {
          agent.inTokens += Number(at['gen_ai.usage.input_tokens'] || at['gen_ai.usage.prompt_tokens'] || 0);
          agent.outTokens += Number(at['gen_ai.usage.output_tokens'] || at['gen_ai.usage.completion_tokens'] || 0);
          const prompt = at['gen_ai.prompt'] || at['gen_ai.input.messages'];
          if (prompt) sess.events.push({ ts, agent: agentId, kind: 'user-text', text: clip(prompt, 240), full: clip(prompt, 2500) });
          const completion = at['gen_ai.completion'] || at['gen_ai.output.messages'] || at['gen_ai.response.text'] || `${span.name} (${model || 'LLM'})`;
          sess.events.push({ ts: endTs || ts, agent: agentId, kind: 'assistant-text', error, text: clip(completion, 240), full: clip(completion, 2500) });
          agent.lastKind = 'assistant-text';
          if (model) agent.tools[model] = (agent.tools[model] || 0) + 1;
        } else {
          const tool = at['gen_ai.tool.name'] || span.name || 'span';
          agent.tools[tool] = (agent.tools[tool] || 0) + 1;
          agent.lastKind = 'tool-call';
          const args = at['gen_ai.tool.call.arguments'] || at['gen_ai.tool.description'] || '';
          sess.events.push({ ts, agent: agentId, kind: 'tool-call', tool, error, endTs, text: clip(args, 240), full: clip(args, 2500) });
        }
        agent.cost = Math.round(costOf(agent) * 10000) / 10000;
      }
    }
  }
  return spans;
}

function otelResult(sess) {
  const events = [...sess.events].sort((a, b) => String(a.ts).localeCompare(String(b.ts)));
  events.forEach((e, i) => { e.seq = i; });
  return { events, agents: [...sess.agents.values()] };
}

function otelList() {
  return [...otelSessions.values()].map(s => ({
    project: 'OTLP (live)', file: s.id, session: s.id, title: s.name + ' · OTLP',
    size: s.events.length, mtime: s.started, agentCount: s.agents.size - 1,
  }));
}

// Resolve either source: "otel:<service>" (in-memory) or a transcript path.
function getResult(fileParam) {
  if (fileParam.startsWith('otel:')) {
    const sess = otelSessions.get(fileParam);
    return sess ? otelResult(sess) : null;
  }
  const full = resolveSessionPath(fileParam);
  if (!full || !fs.existsSync(full)) return null;
  return readSession(full);
}

function readBody(req, cb) {
  let b = '';
  req.on('data', c => { b += c; if (b.length > 50e6) req.destroy(); });
  req.on('end', () => cb(b));
}

function sessionSummary(meta) {
  let r;
  try { r = getResult(meta.file); } catch { return null; }
  if (!r) return null;
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

function buildExport(result, title) {
  const html = fs.readFileSync(path.join(PUBLIC_DIR, 'index.html'), 'utf8');
  const css = fs.readFileSync(path.join(PUBLIC_DIR, 'style.css'), 'utf8');
  const js = fs.readFileSync(path.join(PUBLIC_DIR, 'app.js'), 'utf8');
  const data = { ...result, now: Date.now() };
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

  // OTLP/HTTP trace ingestion — any OpenTelemetry-instrumented agent can POST here
  if (url.pathname === '/v1/traces' && req.method === 'POST') {
    readBody(req, body => {
      try {
        const n = ingestTraces(JSON.parse(body));
        json(res, { partialSuccess: {}, spansIngested: n });
      } catch (e) {
        json(res, { error: 'invalid OTLP-JSON: ' + e.message + ' (use OTEL_EXPORTER_OTLP_PROTOCOL=http/json)' }, 400);
      }
    });
    return;
  }

  if (url.pathname === '/api/sessions') return json(res, [...otelList(), ...listSessions()]);

  if (url.pathname === '/api/fleet') {
    return json(res, [...otelList(), ...listSessions()].map(sessionSummary).filter(Boolean));
  }

  if (url.pathname === '/api/session') {
    const r = getResult(url.searchParams.get('file') || '');
    if (!r) return json(res, { error: 'not found' }, 404);
    return json(res, { ...r, now: Date.now() });
  }

  if (url.pathname === '/api/export') {
    const fileParam = url.searchParams.get('file') || '';
    const r = getResult(fileParam);
    if (!r) { res.writeHead(404); res.end(); return; }
    const name = (url.searchParams.get('title') || path.basename(fileParam, '.jsonl')).replace(/[^\w.-]+/g, '-');
    res.writeHead(200, {
      'Content-Type': 'text/html; charset=utf-8',
      'Content-Disposition': `attachment; filename="mission-control-replay-${name}.html"`,
    });
    res.end(buildExport(r, url.searchParams.get('title')));
    return;
  }

  if (url.pathname === '/api/stream') {
    const fileParam = url.searchParams.get('file') || '';
    const isOtel = fileParam.startsWith('otel:');
    if (!getResult(fileParam)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      'Connection': 'keep-alive',
      'Access-Control-Allow-Origin': '*',
    });
    let lastSig = null;
    const tick = () => {
      const sig = isOtel
        ? String(otelSessions.get(fileParam)?.version || 0)
        : sessionSignature(resolveSessionPath(fileParam));
      if (sig !== lastSig) {
        lastSig = sig;
        try {
          const r = getResult(fileParam);
          if (r) res.write('data: ' + JSON.stringify({ ...r, now: Date.now() }) + '\n\n');
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
