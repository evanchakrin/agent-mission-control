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
const TOKEN = argValue('--token') || process.env.MISSION_CONTROL_TOKEN || null;
const RELAY_TO = argValue('--relay') || null; // relay mode: forward local sessions to a hub instead of serving a UI

// Version: read from package.json beside server.js when present (repo and
// installed copies both ship it now); the literal is only a last-resort
// fallback so a stray copy still reports something truthful-ish.
const APP_VERSION = (() => {
  try { return JSON.parse(fs.readFileSync(path.join(__dirname, 'package.json'), 'utf8')).version || '6.3.0'; }
  catch { return '6.3.0'; }
})();

// $/MTok, matched by substring of the model id; cache reads bill at 0.1x input.
// OpenAI rows are family-level estimates (gpt-5 launch pricing) so Codex
// sessions get a ballpark instead of $0; all UI cost is prefixed "~".
const PRICING = [
  { m: 'fable', in: 10, out: 50 }, { m: 'mythos', in: 10, out: 50 },
  { m: 'opus', in: 5, out: 25 }, { m: 'sonnet', in: 3, out: 15 }, { m: 'haiku', in: 1, out: 5 },
  { m: 'gpt-5', in: 1.25, out: 10 }, { m: 'gpt-4', in: 2.5, out: 10 }, { m: 'o3', in: 2, out: 8 }, { m: 'o4', in: 1.1, out: 4.4 },
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
    // COUNT EACH REPLY ONCE. A single assistant message is written to the
    // transcript once per content block — text, then each tool_use — and every
    // one of those lines carries the SAME usage object. Summing them all counted
    // the same tokens repeatedly: 2.3x on a typical session and over 100x on a
    // subagent that made many tool calls off one reply. Every cost this tool has
    // ever shown was inflated by exactly that duplication.
    const usage = msg.usage || {};
    const id = msg.id;
    const already = id && ctx.seenMsg && ctx.seenMsg.has(id);
    if (id && ctx.seenMsg) ctx.seenMsg.add(id);
    if (!already) {
      agent.inTokens += usage.input_tokens || 0;
      agent.cacheTokens += usage.cache_read_input_tokens || 0;
      agent.outTokens += usage.output_tokens || 0;
    }
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
            // `summary` prefers `description` — a 3-6 word label like "review auth".
            // Keep the VERBATIM prompt separately: anything that matches two runs as
            // "the same job" has to compare the actual instruction, not a short label
            // that a dozen unrelated runs happen to share.
            const verbatim = (b.input && typeof b.input.prompt === 'string') ? b.input.prompt.trim() : '';
            spawnCalls.push({ toolUseId: b.id, name: label, prompt: clip(summary, 200), spawnPrompt: clip(verbatim, 4000), ts, resolved: false, agentId: null });
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

// Lifecycle post-processing, shared by every source. Derives per-event retry
// counts (same agent re-calls the same tool right after an errored result) and
// per-agent pending/failed markers so the UI can show stuck vs retrying vs
// healthy instead of everything looking like "slow".
function postProcessLifecycle(result) {
  const pending = new Map();      // toolUseId -> event
  const lastErrTool = new Map();  // agentId -> tool name that just errored
  const retryCount = new Map();   // agentId:tool -> consecutive retries
  const lastEvt = new Map();      // agentId -> last event
  for (const e of result.events) {
    lastEvt.set(e.agent, e);
    if (e.kind === 'tool-call' || e.kind === 'spawn') {
      if (e.toolUseId) pending.set(e.toolUseId, e);
      const k = e.agent + ':' + e.tool;
      if (lastErrTool.get(e.agent) === e.tool) {
        const n = (retryCount.get(k) || 0) + 1;
        retryCount.set(k, n);
        e.retry = n;
      } else if (!e.retry) {
        retryCount.delete(k);
      }
      lastErrTool.delete(e.agent);
    } else if (e.kind === 'tool-result' || e.kind === 'spawn-result') {
      if (e.toolUseId) pending.delete(e.toolUseId);
      if (e.error) {
        const call = result.events.find(x => x.toolUseId === e.toolUseId && (x.kind === 'tool-call' || x.kind === 'spawn'));
        if (call) lastErrTool.set(e.agent, call.tool);
      } else {
        lastErrTool.delete(e.agent);
      }
    }
  }
  const pendingByAgent = new Map();
  for (const e of pending.values()) pendingByAgent.set(e.agent, e); // last unresolved call wins
  for (const a of result.agents) {
    const p = pendingByAgent.get(a.id);
    a.pendingTool = p ? { tool: p.tool, since: p.ts } : null;
    const le = lastEvt.get(a.id);
    a.lastErrored = !!(le && le.error);
    a.retrying = lastErrTool.has(a.id); // errored and hasn't succeeded since
  }
  return result;
}

// mainLines: JSONL lines of the orchestrator transcript.
// subFiles: [{id, lines, group}] — one per subagents/**/agent-*.jsonl file.
function normalize(mainLines, subFiles, wfNames = new Map()) {
  const ctx = { agents: new Map(), events: [], spawnCalls: [], pending: new Map(), seenMsg: new Set() };
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
    a.spawnPrompt = sp ? (sp.spawnPrompt || '') : '';  // verbatim instruction, for run-to-run matching
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

  return postProcessLifecycle({ events: ctx.events, agents: [...ctx.agents.values()] });
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
        const buf = Buffer.alloc(Math.min(65536, st.size));
        fs.readSync(fd, buf, 0, buf.length, 0);
        fs.closeSync(fd);
        const head = buf.toString('utf8');
        for (const line of head.split('\n')) {
          const o = safeParse(line);
          if (o && (o.type === 'custom-title' || o.type === 'ai-title')) { title = o.customTitle || o.aiTitle; if (o.type === 'custom-title') break; }
        }
        if (!title) {
          // untitled session: fall back to the first real user prompt
          const m = /"operation":"enqueue"[^\n]*?"content":"((?:[^"\\]|\\.){1,300})/.exec(head);
          if (m) {
            try {
              const text = JSON.parse('"' + m[1].replace(/\\$/, '') + '"');
              if (!text.startsWith('<')) title = clip(text.replace(/\s+/g, ' ').trim(), 70);
            } catch { /* partial escape */ }
          }
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

// ---------- Codex CLI / Desktop sessions ----------
// Codex writes rollouts to ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
// Lines are {timestamp, type, payload}. We map messages, tool calls (paired by
// call_id for durations), token_count events, and sub_agent_activity lanes.
const CODEX_DIR = argValue('--codex-dir') || process.env.CODEX_DIR || path.join(os.homedir(), '.codex', 'sessions');
const CODEX_EVENT_CAP = 15000;

function codexFiles() {
  const out = [];
  function walk(dir) {
    let entries = [];
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) walk(full);
      else if (/^rollout-.*\.jsonl$/.test(e.name)) {
        let st;
        try { st = fs.statSync(full); } catch { continue; }
        if (!st.size) continue;
        const uuid = e.name.replace(/\.jsonl$/, '').split('-').slice(-5).join('-');
        out.push({ uuid, path: full, mtime: st.mtimeMs, size: st.size });
      }
    }
  }
  walk(CODEX_DIR);
  return out;
}

const codexMetaCache = new Map(); // path -> {size, meta}
function codexMeta(file) {
  const hit = codexMetaCache.get(file.path);
  if (hit && hit.size === file.size) return hit.meta;
  let meta = { cwd: null, threadSource: 'user', title: null };
  try {
    const fd = fs.openSync(file.path, 'r');
    const buf = Buffer.alloc(Math.min(524288, file.size));
    fs.readSync(fd, buf, 0, buf.length, 0);
    fs.closeSync(fd);
    const head = buf.toString('utf8');
    // session_meta lines can exceed any fixed read (huge base_instructions), so
    // regex the fields we need rather than requiring a complete JSON line
    const cwd = /"cwd":"((?:[^"\\]|\\.)*)"/.exec(head);
    const src = /"thread_source":"([^"]*)"/.exec(head);
    // title: first real user prompt (skip injected <...> meta blocks)
    let title = null;
    const userMsg = /"role":"user","content":\[\{"type":"input_text","text":"((?:[^"\\]|\\.){1,300})/g;
    let m;
    while ((m = userMsg.exec(head))) {
      let text;
      try { text = JSON.parse('"' + m[1].replace(/\\$/, '') + '"'); } catch { continue; }
      if (!text.startsWith('<') && !text.startsWith('# AGENTS.md') && !text.startsWith('Caveat:')) { title = clip(text.replace(/\s+/g, ' ').trim(), 70); break; }
    }
    meta = { cwd: cwd ? JSON.parse('"' + cwd[1] + '"') : null, threadSource: src ? src[1] : 'user', title };
  } catch { /* ignore */ }
  codexMetaCache.set(file.path, { size: file.size, meta });
  return meta;
}

function codexList() {
  return codexFiles()
    .filter(f => codexMeta(f).threadSource === 'user')
    .sort((a, b) => b.mtime - a.mtime)
    .map(f => {
      const meta = codexMeta(f);
      const proj = meta.cwd ? path.basename(meta.cwd) : 'unknown';
      return { project: 'Codex · ' + proj, file: 'codex:' + f.uuid, session: f.uuid, title: meta.title || proj, size: f.size, mtime: f.mtime, agentCount: 0 };
    });
}

function parseCodexLines(lines, agentId, ctx) {
  const { agents, events, pending } = ctx;
  const MODEL_RE = /"model":"([a-z0-9][\w.-]{1,40})"/;
  if (!agents.has(agentId)) agents.set(agentId, newAgent(agentId, agentId === 'main' ? 'main' : 'subagent'));
  const agent = agents.get(agentId);
  for (const line of lines) {
    if (!line.trim()) continue;
    if (!ctx.model && line.includes('"model":"')) { const mm = MODEL_RE.exec(line); if (mm) ctx.model = mm[1]; }
    const o = safeParse(line);
    if (!o || !o.payload) continue;
    const p = o.payload, ts = o.timestamp || null;
    const touch = (a) => { if (!a.firstTs) a.firstTs = ts; a.lastTs = ts; a.events++; };

    if (o.type === 'response_item') {
      if (p.type === 'message') {
        const text = (p.content || []).map(c => c.text || '').join('\n');
        if (!text.trim()) continue;
        if (p.role === 'user' && !text.startsWith('<')) {
          touch(agent);
          events.push({ ts, agent: agentId, kind: 'user-text', text: clip(text, 240), full: clip(text, 2500) });
        } else if (p.role === 'assistant') {
          touch(agent); agent.lastKind = 'assistant-text';
          events.push({ ts, agent: agentId, kind: 'assistant-text', text: clip(text, 240), full: clip(text, 2500) });
        }
      } else if (p.type === 'custom_tool_call' || p.type === 'function_call') {
        touch(agent); agent.lastKind = 'tool-call';
        const tool = p.name || 'tool';
        agent.tools[tool] = (agent.tools[tool] || 0) + 1;
        const argText = p.input || p.arguments || '';
        const evt = { ts, agent: agentId, kind: 'tool-call', tool, toolUseId: p.call_id, text: clip(argText, 240), full: clip(argText, 2500) };
        events.push(evt);
        if (p.call_id) pending.set(p.call_id, evt);
      } else if (p.type === 'custom_tool_call_output' || p.type === 'function_call_output') {
        const start = pending.get(p.call_id);
        if (start && !start.endTs && ts) start.endTs = ts;
        const out = typeof p.output === 'string' ? p.output : JSON.stringify(p.output || '');
        const error = /"(failed|error)"|^error/i.test(String(out).slice(0, 200));
        if (error) agent.errors++;
        events.push({ ts, agent: agentId, kind: 'tool-result', toolUseId: p.call_id, error, text: clip(out, 240), full: clip(out, 2500) });
      }
    } else if (o.type === 'event_msg') {
      if (p.type === 'token_count' && p.info && p.info.last_token_usage) {
        const u = p.info.last_token_usage;
        agent.inTokens += Math.max(0, (u.input_tokens || 0) - (u.cached_input_tokens || 0));
        agent.cacheTokens += u.cached_input_tokens || 0;
        agent.outTokens += u.output_tokens || 0;
      } else if (p.type === 'sub_agent_activity' && p.agent_path && p.agent_path !== '/root') {
        const name = p.agent_path.split('/').filter(Boolean).pop();
        const subId = 'sub:' + p.agent_path;
        if (!agents.has(subId)) {
          agents.set(subId, { ...newAgent(subId, 'subagent'), name });
          events.push({ ts, agent: 'main', kind: 'spawn', tool: 'sub_agent', spawnedAgent: subId, text: clip(p.agent_path, 240), full: clip(p.agent_path, 2500) });
        }
        const sub = agents.get(subId);
        touch(sub);
        if (p.kind === 'started') sub.task = name;
        if (p.kind === 'completed' || p.kind === 'interrupted') sub.done = true;
        ctx.subThreads.set(p.agent_thread_id, subId);
      }
    }
  }
}

// Rollout files can reach hundreds of MB. Cap what we load: the newest 24MB
// (aligned to a line boundary). Early token_count events are lost for huge
// sessions, but events/agents/tools come from the recent tail, which is what
// the dashboard shows anyway.
const CODEX_READ_CAP = 24 * 1024 * 1024;
function readCappedLines(filePath, size) {
  if (size <= CODEX_READ_CAP) return fs.readFileSync(filePath, 'utf8').split('\n');
  const fd = fs.openSync(filePath, 'r');
  const buf = Buffer.alloc(CODEX_READ_CAP);
  fs.readSync(fd, buf, 0, buf.length, size - buf.length);
  fs.closeSync(fd);
  const lines = buf.toString('utf8').split('\n');
  lines.shift(); // first line is almost certainly a partial JSON record
  return lines;
}

function readCodexSession(uuid) {
  const files = codexFiles();
  const file = files.find(f => f.uuid === uuid);
  if (!file) return null;
  const ctx = { agents: new Map(), events: [], pending: new Map(), subThreads: new Map(), seenMsg: new Set() };
  const meta = codexMeta(file);
  ctx.agents.set('main', { ...newAgent('main', 'main'), name: (meta.cwd ? path.basename(meta.cwd) : 'Codex') + ' /root' });
  parseCodexLines(readCappedLines(file.path, file.size), 'main', ctx);
  // subagent threads have their own rollout files, keyed by agent_thread_id
  for (const [threadId, subId] of ctx.subThreads) {
    const tf = files.find(f => f.uuid === threadId);
    if (tf) {
      try { parseCodexLines(readCappedLines(tf.path, tf.size), subId, ctx); } catch { /* ignore */ }
    }
  }
  for (const a of ctx.agents.values()) { if (!a.model && ctx.model) a.model = ctx.model; a.cost = Math.round(costOf(a) * 10000) / 10000; }
  ctx.events.sort((a, b) => String(a.ts).localeCompare(String(b.ts)));
  const events = ctx.events.length > CODEX_EVENT_CAP ? ctx.events.slice(-CODEX_EVENT_CAP) : ctx.events;
  events.forEach((e, i) => { e.seq = i; });
  return postProcessLifecycle({ events, agents: [...ctx.agents.values()] });
}

function codexSignature(uuid) {
  const file = codexFiles().find(f => f.uuid === uuid);
  return file ? String(file.size) : '';
}

const codexCache = new Map(); // uuid -> {sig, result}
function readCodexCached(uuid) {
  const sig = codexSignature(uuid);
  const hit = codexCache.get(uuid);
  if (hit && hit.sig === sig) return hit.result;
  const result = readCodexSession(uuid);
  if (result) {
    codexCache.set(uuid, { sig, result });
    if (codexCache.size > 6) codexCache.delete(codexCache.keys().next().value);
  }
  return result;
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
        // agent identity: gen_ai conventions, OpenInference (Phoenix), and common framework attrs
        const agentName = at['gen_ai.agent.name'] || at['agent.name'] || at['crewai.agent.role']
          || at['openinference.agent.name'] || at['llm.agent.name'] || at['traceloop.entity.name'] || null;
        const agentId = agentName ? 'sub:' + agentName : 'main';
        if (!sess.agents.has(agentId)) sess.agents.set(agentId, { ...newAgent(agentId, 'subagent'), name: agentName });
        const agent = sess.agents.get(agentId);
        if (!agent.firstTs || (ts && ts < agent.firstTs)) agent.firstTs = ts;
        if (!agent.lastTs || (endTs || ts) > agent.lastTs) agent.lastTs = endTs || ts;
        agent.events++;
        // model: gen_ai + OpenInference (llm.model_name) + Traceloop (llm.request.model)
        const model = at['gen_ai.request.model'] || at['gen_ai.response.model']
          || at['llm.model_name'] || at['llm.request.model'] || at['llm.response.model'];
        if (model && !agent.model) agent.model = model;
        const error = !!(span.status && span.status.code === 2);
        if (error) agent.errors++;
        // OpenInference span kind (LLM/TOOL/AGENT/CHAIN) is authoritative when present
        const oiKind = String(at['openinference.span.kind'] || at['span.kind'] || '').toUpperCase();
        const op = String(at['gen_ai.operation.name'] || span.name || '').toLowerCase();
        const isLLM = oiKind === 'LLM' || (!oiKind.match(/TOOL|RETRIEVER|EMBEDDING/) && (model || /chat|completion|generate/.test(op)));
        if (isLLM) {
          agent.inTokens += Number(at['gen_ai.usage.input_tokens'] || at['gen_ai.usage.prompt_tokens'] || at['llm.token_count.prompt'] || 0);
          agent.outTokens += Number(at['gen_ai.usage.output_tokens'] || at['gen_ai.usage.completion_tokens'] || at['llm.token_count.completion'] || 0);
          const prompt = at['gen_ai.prompt'] || at['gen_ai.input.messages'] || at['input.value'] || at['llm.input_messages.0.message.content'];
          if (prompt) sess.events.push({ ts, agent: agentId, kind: 'user-text', text: clip(prompt, 240), full: clip(prompt, 2500) });
          const completion = at['gen_ai.completion'] || at['gen_ai.output.messages'] || at['gen_ai.response.text']
            || at['output.value'] || at['llm.output_messages.0.message.content'] || `${span.name} (${model || 'LLM'})`;
          sess.events.push({ ts: endTs || ts, agent: agentId, kind: 'assistant-text', error, text: clip(completion, 240), full: clip(completion, 2500) });
          agent.lastKind = 'assistant-text';
          if (model) agent.tools[model] = (agent.tools[model] || 0) + 1;
        } else {
          const tool = at['gen_ai.tool.name'] || at['tool.name'] || span.name || 'span';
          agent.tools[tool] = (agent.tools[tool] || 0) + 1;
          agent.lastKind = 'tool-call';
          const args = at['gen_ai.tool.call.arguments'] || at['tool.parameters'] || at['input.value'] || at['gen_ai.tool.description'] || '';
          const retry = Number(at['retry.count'] || at['gen_ai.request.retry_count'] || 0);
          sess.events.push({ ts, agent: agentId, kind: 'tool-call', tool, error, endTs, retry: retry || undefined, text: clip(args, 240), full: clip(args, 2500) });
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
  return postProcessLifecycle({ events, agents: [...sess.agents.values()] });
}

function otelList() {
  return [...otelSessions.values()].map(s => ({
    project: 'OTLP (live)', file: s.id, session: s.id, title: s.name + ' · OTLP',
    size: s.events.length, mtime: s.started, agentCount: s.agents.size - 1,
  }));
}

// ---------- relay (hub side) ----------
// Remote machines run `--relay http://hub:4173 --token <secret>`: they tail
// their own transcripts and POST parsed sessions here. Stored in memory,
// keyed "relay:<machine>:<file>", and shown in the fleet like local sessions.
const relaySessions = new Map(); // id -> {id, meta, version, result, machine, ips, at}
const BOOT_ID = Math.random().toString(36).slice(2); // hub identity; new relays resend when this changes

// Relayed sessions are cached to disk so a hub restart does NOT lose them.
// This decouples RECEPTION from the relay's version/behavior: even an old relay
// that never resends after a restart keeps its last-delivered data visible.
// Bounded: newest RELAY_KEEP sessions, each trimmed to RELAY_EVENT_CAP events.
const RELAY_DIR = () => path.join(STATE_DIR, 'relay');
const RELAY_KEEP = 400;
const RELAY_EVENT_CAP = 4000;
function relayFileFor(id) { return path.join(RELAY_DIR(), crypto.createHash('sha1').update(id).digest('hex') + '.json'); }

const RELAY_MAX_BYTES = 4 * 1024 * 1024; // per cached session file
function persistRelay(rec) {
  try {
    fs.mkdirSync(RELAY_DIR(), { recursive: true });
    let events = rec.result.events.length > RELAY_EVENT_CAP ? rec.result.events.slice(-RELAY_EVENT_CAP) : rec.result.events;
    let payload = JSON.stringify({ id: rec.id, machine: rec.machine, meta: rec.meta, proj: rec.proj, ips: rec.ips, at: rec.at, result: { ...rec.result, events } });
    while (payload.length > RELAY_MAX_BYTES && events.length > 200) { // keep the disk cache bounded
      events = events.slice(Math.ceil(events.length / 2));
      payload = JSON.stringify({ id: rec.id, machine: rec.machine, meta: rec.meta, proj: rec.proj, ips: rec.ips, at: rec.at, result: { ...rec.result, events } });
    }
    const tmp = relayFileFor(rec.id) + '.tmp';
    fs.writeFileSync(tmp, payload);
    fs.renameSync(tmp, relayFileFor(rec.id));
    // enforce count cap: evict oldest files
    const dir = RELAY_DIR();
    const files = fs.readdirSync(dir).filter(f => f.endsWith('.json')).map(f => ({ f, m: fs.statSync(path.join(dir, f)).mtimeMs }));
    if (files.length > RELAY_KEEP) {
      files.sort((a, b) => a.m - b.m);
      for (const { f } of files.slice(0, files.length - RELAY_KEEP)) { try { fs.unlinkSync(path.join(dir, f)); } catch { /* ignore */ } }
    }
  } catch (e) { console.error('relay persist failed:', e.message); }
}

// ---------- full-transcript archive (raw .jsonl from remote machines) ----------
// Relays with --archive upload their raw session transcripts here so the hub
// holds a complete copy — not just the trimmed relay payloads. Path-guarded,
// per-file and total-size capped.
const ARCHIVE_DIR = () => path.join(STATE_DIR, 'archive');
const ARCHIVE_FILE_CAP = 120 * 1024 * 1024;   // skip any single file bigger than this
const ARCHIVE_TOTAL_CAP = 6 * 1024 * 1024 * 1024; // ~6GB total budget
function safeMachine(m) { return String(m || '').replace(/[^\w.-]+/g, '_').slice(0, 60); }
function archiveMachineDir(machine) { return path.join(ARCHIVE_DIR(), safeMachine(machine)); }
function archiveManifest(machine) {
  const base = archiveMachineDir(machine);
  const out = {};
  function walk(dir, rel) {
    let entries = [];
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const full = path.join(dir, e.name), r = rel ? rel + '/' + e.name : e.name;
      if (e.isDirectory()) walk(full, r);
      else { try { out[r] = fs.statSync(full).size; } catch { /* skip */ } }
    }
  }
  walk(base, '');
  return out;
}
function archiveDirSize(dir) {
  let total = 0;
  function walk(d) { let en = []; try { en = fs.readdirSync(d, { withFileTypes: true }); } catch { return; } for (const e of en) { const f = path.join(d, e.name); if (e.isDirectory()) walk(f); else { try { total += fs.statSync(f).size; } catch { /* skip */ } } } }
  walk(dir);
  return total;
}
function storeArchiveFile(machine, relPath, buf) {
  const base = path.resolve(archiveMachineDir(machine));
  const dest = path.resolve(base, relPath.replace(/\\/g, '/'));
  if (!dest.startsWith(base + path.sep) && dest !== base) throw new Error('path escape');
  if (!/\.jsonl$/.test(dest)) throw new Error('only .jsonl');
  if (buf.length > ARCHIVE_FILE_CAP) throw new Error('file too large');
  if (archiveDirSize(ARCHIVE_DIR()) + buf.length > ARCHIVE_TOTAL_CAP) throw new Error('archive full');
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  const tmp = dest + '.tmp';
  fs.writeFileSync(tmp, buf);
  fs.renameSync(tmp, dest);
  return dest;
}
function archiveSummary() {
  const out = [];
  let machines = [];
  try { machines = fs.readdirSync(ARCHIVE_DIR()); } catch { return out; }
  for (const m of machines) {
    const dir = path.join(ARCHIVE_DIR(), m);
    try { if (!fs.statSync(dir).isDirectory()) continue; } catch { continue; }
    const man = archiveManifest(m);
    out.push({ machine: m, files: Object.keys(man).length, bytes: Object.values(man).reduce((a, b) => a + b, 0) });
  }
  return out;
}

function loadRelayCache() {
  let files = [];
  try { files = fs.readdirSync(RELAY_DIR()).filter(f => f.endsWith('.json')); } catch { return; }
  let n = 0;
  for (const f of files) {
    try {
      const r = JSON.parse(fs.readFileSync(path.join(RELAY_DIR(), f), 'utf8'));
      if (!r.id || !r.result) continue;
      // r.proj is absent in every file written before the project field existed —
      // null is correct there, and relayProjLabel() falls back on its own.
      relaySessions.set(r.id, { id: r.id, machine: r.machine, meta: r.meta || {}, proj: sanitizeProj(r.proj), version: 1, result: r.result, ips: r.ips, at: r.at });
      if (r.machine) {
        // max(), not last-wins: readdir order is arbitrary, and this value now drives
        // both the quiet verdict and the number shown to the owner
        const prev = machines.get(r.machine);
        const at = r.at || 0;
        if (!prev || at > (prev.lastData || 0)) {
          machines.set(r.machine, { name: r.machine, ips: Array.isArray(r.ips) ? r.ips : [], lastSeen: at || Date.now(), lastData: at || undefined, remote: true, cached: true });
        }
      }
      n++;
    } catch { /* skip bad file */ }
  }
  if (n) console.log(`Restored ${n} relayed sessions from cache (${RELAY_DIR()})`);
}

// body.proj — the relay's own view of WHICH PROJECT this session belongs to.
// Sanitised here because this is the network trust boundary: control bytes go,
// both halves are hard length-capped, and "absent" stays a first-class answer
// (null) rather than something guessed at — an older relay simply omits it.
function sanitizeProj(p) {
  if (!p || typeof p !== 'object') return null;
  const slug = clean(p.slug, 200), cwd = clean(p.cwd, 400);
  return slug || cwd ? { slug: slug || null, cwd: cwd || null } : null;
}

function ingestRelay(body) {
  const id = 'relay:' + body.machine + ':' + body.file;
  const prev = relaySessions.get(id);
  const rec = {
    id, machine: body.machine, meta: body.meta || {}, proj: sanitizeProj(body.proj),
    version: (prev ? prev.version : 0) + 1, result: body.result,
    ips: Array.isArray(body.ips) ? body.ips : [], at: Date.now(),
  };
  relaySessions.set(id, rec);
  // lastData is the ONLY signal the quiet detector may use. lastSeen is liveness
  // (the 5s heartbeat) and stays fresh while the relay process is alive even if it
  // has stopped sending anything — which is exactly the failure this must catch.
  const prevM = machines.get(body.machine);
  machines.set(body.machine, { name: body.machine, ips: rec.ips, lastSeen: Date.now(), lastData: Date.now(), remote: true, version: body.version || (prevM && prevM.version) || null });
  persistRelay(rec);
  return id;
}

// ---------- machines registry (read-only telemetry) ----------
const machines = new Map(); // name -> {name, ips, lastSeen, remote}

function localIPs() {
  return Object.values(os.networkInterfaces()).flat()
    .filter(i => i && i.family === 'IPv4' && !i.internal)
    .map(i => i.address);
}

// WHY: DESKTOP-OCVTL8Q means nothing to a human, and a renamed machine must
// never touch the relay (WATCH-ONLY/OUTBOUND-ONLY — the hub never reaches out
// to a relay machine, so it can't rename anything there even if it wanted to).
// So the friendly name lives ONLY in hub metadata (metaState.machineNames),
// keyed by the machine's real name — the real name stays the join key
// everywhere sessions get matched to a machine, and the rename can't ever
// desync a relay from what the hub calls it.
function machineDisplayName(name) {
  const dn = metaState && metaState.machineNames && metaState.machineNames[name];
  return (typeof dn === 'string' && dn) ? dn : null;
}

function machineList() {
  const localName = os.hostname();
  const local = { name: localName, ips: localIPs(), lastSeen: Date.now(), remote: false, version: APP_VERSION };
  const list = [local, ...[...machines.values()].filter(m => m.name !== localName)];
  // Bucket check-in timestamps per machine from each session's OWN recorded
  // mtime (s.meta.mtime — the transcript's real last-activity time, organic
  // and spread across the machine's whole history). Deliberately NOT
  // relaySessions' `.at` (hub receipt time): a relay does a full resend of
  // every session it holds whenever it notices the hub rebooted (boot-ID
  // check on /v1/boot), which stamps `.at` on ALL of a machine's sessions
  // within the same few minutes — that would make "this machine's rhythm"
  // reset to near-zero on every hub restart. A session with no meta.mtime
  // (pre-dates that field) is skipped rather than defaulted to "now", which
  // would inject a fake, ever-shifting data point into real history.
  const byMachine = {};
  for (const s of relaySessions.values()) {
    const t = s.meta && s.meta.mtime;
    if (!s.machine || !t) continue;
    (byMachine[s.machine] = byMachine[s.machine] || []).push(t);
  }
  return list.map(m => ({ ...m, displayName: machineDisplayName(m.name), quiet: machineQuietState(m, byMachine[m.name] || []) }));
}

// ---------- machine check-in rhythm (learned quiet-machine detection) ----------
// WHY: a flat "no ping in N minutes" test can't tell a machine that normally
// reports every few minutes and just went dark from one that only ever checks
// in once a day — a threshold tight enough to catch the first flags the second
// as broken every single day, and a threshold loose enough to spare the second
// misses the first for hours (this is exactly what happened: trifecta-erp's
// relay died and the dashboard just showed "idle", indistinguishable from a
// machine that's merely quiet, for two days). So: learn each machine's own
// rhythm from the gaps between the timestamps of its OWN past sessions — not
// the ~5s relay-tick /v1/boot heartbeat, which is nearly constant whenever the
// relay process is merely alive and says nothing about how often work actually
// happens (a constant liveness ping, not a signal of how often WORK happens)
// — then flag only when live silence is far outside that machine's own
// history, and only once there's enough history to trust the number.
//
// The median gap alone is NOT a safe basis for the threshold: real usage is
// bursty (many short gaps during a working stretch, a long natural gap
// overnight or over a weekend) — a machine actually reports on real fleet
// history a median gap of ~27min but a 90th-percentile gap over 18h, so
// "8x the median" would flag perfectly ordinary weekend silence as broken.
// The median stays the right number for the human sentence ("usually every
// X") — typical is exactly what "usually" means — but the ALARM threshold is
// built from the 90th-percentile gap instead: "far outside anything this
// machine's own history has shown, with a safety margin," which tolerates
// its normal long pauses while still catching truly unprecedented silence.
const RHYTHM_MIN_SESSIONS = 5;        // need at least this many sessions (4 gaps) before either statistic means anything
const RHYTHM_P90_MULTIPLIER = 2;      // flag once silence exceeds 2x this machine's own 90th-percentile gap
const RHYTHM_FLOOR_MS = 30 * 60e3;    // ...but never below 30min, so a tight rhythm can't cry wolf over ordinary jitter
const RHYTHM_CEILING_MS = 6 * 3600e3; // ...and never above 6h: past that the alarm is too late to be worth having
const RHYTHM_MIN_SPAN_MS = 7 * 864e5; // a week of history before any rhythm claim — 5 sessions in one afternoon is not a rhythm
function median(nums) {
  const a = [...nums].sort((x, y) => x - y);
  const n = a.length;
  if (!n) return 0;
  return n % 2 ? a[(n - 1) / 2] : (a[n / 2 - 1] + a[n / 2]) / 2;
}
// nearest-rank percentile; sortedAsc must already be sorted ascending.
function percentile(sortedAsc, p) {
  if (!sortedAsc.length) return 0;
  const idx = Math.min(sortedAsc.length - 1, Math.max(0, Math.ceil(p * sortedAsc.length) - 1));
  return sortedAsc[idx];
}
// m: an entry from the `machines` map ({name, lastSeen, remote, ...}).
// mtimes: this machine's known session check-in timestamps, any order.
function machineQuietState(m, mtimes) {
  if (!m.remote) return { enoughHistory: true, quiet: false }; // the hub is never "quiet" on itself
  // silence = time since DATA last arrived; falls back to lastSeen only when this
  // machine has never delivered anything in this hub's lifetime
  const silenceMs = Date.now() - (m.lastData || m.lastSeen || 0);
  if (mtimes.length < RHYTHM_MIN_SESSIONS) {
    return { enoughHistory: false, sessions: mtimes.length, needed: RHYTHM_MIN_SESSIONS, silenceMs, quiet: false };
  }
  const sorted = [...mtimes].sort((a, b) => a - b);
  const gaps = [];
  for (let i = 1; i < sorted.length; i++) gaps.push(sorted[i] - sorted[i - 1]);
  const gapsSorted = [...gaps].sort((a, b) => a - b);
  const medianGapMs = median(gaps);                    // for the sentence: "usually reports every X"
  const p90GapMs = percentile(gapsSorted, 0.9);         // for the alarm: this machine's own worst-normal gap
  // Ceiling matters: 2x a machine's near-largest gap reached ~36h on real data,
  // barely better than the two-day miss that prompted this. Learned rhythm sets the
  // floor of sensitivity; RHYTHM_CEILING_MS caps how long it may ever stay silent.
  const thresholdMs = Math.min(RHYTHM_CEILING_MS, Math.max(RHYTHM_FLOOR_MS, RHYTHM_P90_MULTIPLIER * p90GapMs));
  // Count is not history. Five sessions from one afternoon says nothing about a
  // machine's rhythm, and would pin a permanent red banner on a box used once.
  const spanMs = sorted[sorted.length - 1] - sorted[0];
  if (spanMs < RHYTHM_MIN_SPAN_MS) {
    return { enoughHistory: false, sessions: mtimes.length, needed: RHYTHM_MIN_SESSIONS, spanMs, needSpanMs: RHYTHM_MIN_SPAN_MS, silenceMs, quiet: false };
  }
  return { enoughHistory: true, sessions: mtimes.length, medianGapMs, p90GapMs, thresholdMs, silenceMs, quiet: silenceMs > thresholdMs };
}

// Which agent kind produced a session, from its source-prefixed file id.
function agentKindOf(file) {
  if (file.startsWith('codex:') || file.includes(':codex:')) return 'codex';
  if (file.startsWith('otel:')) return 'otel';
  return 'claude';
}

// ---------- session/project metadata (durable, loopback-only) ----------
// User-owned organization (projects, tags, archive, pin, note) kept parallel to
// the read-only parse layer. Stored at ~/.claude/mission-control/state.json.
// Bound to a stableKey that survives relay re-sends and hub restarts, NEVER the
// volatile source-prefixed file id. Mutations are loopback + origin + CSRF gated.
const crypto = require('crypto');
const STATE_DIR = path.join(os.homedir(), '.claude', 'mission-control');
const STATE_FILE = path.join(STATE_DIR, 'state.json');
const META_CSRF = crypto.randomUUID();
const LIM = { note: 2000, tagsPerSession: 24, projects: 200, tags: 200, sessions: 5000, bulk: 500, name: 120 };
let metaState = null;   // { v, metaVersion, machineId, projects, tags, savedFilters, sessions }
let metaReadOnly = false;

function defaultState() {
  return { v: 1, metaVersion: 0, machineId: crypto.randomUUID(), projects: [], tags: [], savedFilters: [], sessions: {}, machineNames: {} };
}
function loadState() {
  try { fs.mkdirSync(STATE_DIR, { recursive: true }); } catch { /* ignore */ }
  // clean stale temp files from a crashed write
  try { for (const f of fs.readdirSync(STATE_DIR)) if (/\.tmp$/.test(f)) fs.unlinkSync(path.join(STATE_DIR, f)); } catch { /* ignore */ }
  if (!fs.existsSync(STATE_FILE)) { metaState = defaultState(); try { saveStateRaw(metaState); } catch { /* ignore */ } return; }
  try {
    const s = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
    if (!s || typeof s !== 'object' || !s.sessions) throw new Error('shape');
    s.machineId = s.machineId || crypto.randomUUID();
    metaState = { ...defaultState(), ...s };
  } catch (e) {
    // never overwrite a corrupt file with an empty doc — preserve + go read-only
    try { fs.renameSync(STATE_FILE, path.join(STATE_DIR, `state.corrupt-${Date.now()}.json`)); } catch { /* ignore */ }
    console.error('mission-control: state.json unreadable — entering read-only metadata mode.', e.message);
    metaState = defaultState(); metaReadOnly = true;
  }
}
function saveStateRaw(next) {
  const tmp = path.join(STATE_DIR, `state.${process.pid}.${crypto.randomBytes(4).toString('hex')}.tmp`);
  const fd = fs.openSync(tmp, 'w');
  try { fs.writeSync(fd, JSON.stringify(next)); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
  let lastErr;
  for (let i = 0; i < 5; i++) {
    try { fs.renameSync(tmp, STATE_FILE); return; } catch (e) { lastErr = e; } // rare Windows EPERM/EBUSY: retry immediately
  }
  try { fs.unlinkSync(tmp); } catch { /* ignore */ }
  throw lastErr;
}
// clone → mutate/validate on the clone → durable write → swap live state
function commit(mutator) {
  if (metaReadOnly) { const e = new Error('metadata is read-only (corrupt state recovered)'); e.code = 503; throw e; }
  const next = JSON.parse(JSON.stringify(metaState));
  mutator(next);                       // may throw (400) — live state untouched
  if (Object.keys(next.sessions).length > LIM.sessions) {
    const entries = Object.entries(next.sessions).sort((a, b) => (a[1].updatedAt || 0) - (b[1].updatedAt || 0));
    for (const [k] of entries.slice(0, entries.length - LIM.sessions)) delete next.sessions[k];
  }
  next.metaVersion = (metaState.metaVersion || 0) + 1;
  saveStateRaw(next);                  // may throw (500)
  metaState = next;
  return metaState.metaVersion;
}

const enc = encodeURIComponent;
// Stable, source-independent key for a listing item. L: hub-local, R: relayed.
function stableKeyForItem(it) {
  const f = it.file || '';
  if (f.startsWith('relay:')) {
    const parts = f.split(':');
    const machine = parts[1] || '';
    const kind = agentKindOf(parts.slice(2).join(':'));
    let native = String(it.session || '');
    if (native.startsWith('codex:')) native = native.slice(6);
    if (native.startsWith('otel:')) native = native.slice(5);
    native = native.replace(/^.*[\\/]/, '').replace(/\.jsonl$/, '');
    if (!machine || !native) return null;
    return `R:${enc(machine)}:${kind}:${enc(native)}`;
  }
  const mid = metaState ? metaState.machineId : 'local';
  if (f.startsWith('codex:')) return `L:${enc(mid)}:codex:${enc(f.slice(6))}`;
  if (f.startsWith('otel:')) return `L:${enc(mid)}:otel:${enc(f.slice(5))}`;
  const native = String(it.session || '').replace(/\.jsonl$/, '');
  if (!native) return null;
  return `L:${enc(mid)}:claude:${enc(native)}`;
}
const STABLEKEY_RE = /^[LR]:[^:]+:(claude|codex|otel):.+$/;

function clean(s, max) { return String(s == null ? '' : s).replace(/[\u0000-\u001f\u007f]/g, '').slice(0, max); }
function knownStableKey(k) {
  if (!STABLEKEY_RE.test(k)) return false;
  for (const it of [...relayList(), ...otelList(), ...listSessions(), ...codexList()]) if (stableKeyForItem(it) === k) return true;
  return false;
}

// ---------- audit log (immutable record of every state-changing action) ----------
// Anything that mutates on-disk state (config writes today; launches/approvals
// once a control tier exists) appends here; nothing bypasses it. Read-only over
// HTTP, gated like all mutation-adjacent routes.
const AUDIT_FILE = path.join(STATE_DIR, 'audit.jsonl');
function appendAudit(entry) {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    fs.appendFileSync(AUDIT_FILE, JSON.stringify({ at: Date.now(), ...entry }) + '\n');
  } catch (e) { console.error('audit append failed:', e.message); }
}
function readAudit(limit = 400) {
  try {
    const lines = fs.readFileSync(AUDIT_FILE, 'utf8').trim().split('\n').filter(Boolean);
    return lines.slice(-limit).map(safeParse).filter(Boolean).reverse();
  } catch { return []; }
}

// ---------- insight triage (resolve/dismiss fleet issues, persisted) ----------
const TRIAGE_FILE = path.join(STATE_DIR, 'triage.json');
function loadTriage() { try { return JSON.parse(fs.readFileSync(TRIAGE_FILE, 'utf8')); } catch { return {}; } }
function saveTriage(next) {
  fs.mkdirSync(STATE_DIR, { recursive: true });
  const tmp = TRIAGE_FILE + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(next, null, 2));
  fs.renameSync(tmp, TRIAGE_FILE);
}

// ---------- playbook library (durable, editable) ----------
// The long-term home for orchestration plays and agentic-doc snippets. JSON
// store; gated CRUD; every change audited. Studio can save a generated play
// straight into the library, and you can author custom ones.
const PLAYBOOK_FILE = path.join(STATE_DIR, 'playbooks.json');
function loadPlaybooks() {
  try { return JSON.parse(fs.readFileSync(PLAYBOOK_FILE, 'utf8')); } catch { return { v: 1, items: [] }; }
}
function savePlaybooks(next) {
  fs.mkdirSync(STATE_DIR, { recursive: true });
  const tmp = PLAYBOOK_FILE + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(next, null, 2));
  fs.renameSync(tmp, PLAYBOOK_FILE);
}

// ---------- directive registry (standing orders planted into guidance files) ----------
// A "directive" is a marker-wrapped block AMC appends to a project's CLAUDE.md
// or AGENTS.md — a standing rule every future agent session in that repo reads.
// The registry remembers what was planted where, so drift is detectable and any
// directive is retirable (the block is removed cleanly, never the whole file).
// Targets come from a server-side allowlist; the client only ever sends ids.
const DIRECTIVE_FILE = path.join(STATE_DIR, 'directives.json');
function loadDirectives() {
  try { return JSON.parse(fs.readFileSync(DIRECTIVE_FILE, 'utf8')); } catch { return { v: 1, items: [] }; }
}
function saveDirectives(next) {
  fs.mkdirSync(STATE_DIR, { recursive: true });
  const tmp = DIRECTIVE_FILE + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(next, null, 2));
  fs.renameSync(tmp, DIRECTIVE_FILE);
}
// ---------- owner-added folders (the only client-supplied part of the allowlist) ----------
// The auto-discovered list only knows repos an agent has actually run inside, so
// most of the owner's folders were unreachable. They can now name a folder here.
// That makes a client string reach the filesystem, so it is validated to death and
// re-validated on every use: it must resolve to a real existing directory, it may
// never live under a .claude tree or AMC's own state dir, and the only thing ever
// joined onto it is a literal filename from ROOT_FILES below — never a client one.
const DIRECTIVE_ROOTS_FILE = path.join(STATE_DIR, 'directive-roots.json');
const ROOT_FILES = ['CLAUDE.md', 'AGENTS.md']; // FIXED basename allowlist. Never widen from a request.
const ROOT_MAX_LEN = 400, ROOT_LIMIT = 50;
function loadDirectiveRoots() {
  try {
    const v = JSON.parse(fs.readFileSync(DIRECTIVE_ROOTS_FILE, 'utf8'));
    return { v: 1, roots: (Array.isArray(v.roots) ? v.roots : []).filter(r => r && typeof r.path === 'string') };
  } catch { return { v: 1, roots: [] }; }
}
function saveDirectiveRoots(next) {
  fs.mkdirSync(STATE_DIR, { recursive: true });
  const tmp = DIRECTIVE_ROOTS_FILE + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(next, null, 2));
  fs.renameSync(tmp, DIRECTIVE_ROOTS_FILE);
}
// Windows paths compare case-insensitively — otherwise the de-dupe is bypassable.
function normRoot(p) { return process.platform === 'win32' ? String(p).toLowerCase() : String(p); }
// The single gate. Returns the real resolved directory, or null if it is not usable.
// Used for both storing a root and re-checking a stored one, so a folder that was
// deleted, replaced by a file, or re-pointed by a symlink stops being a target.
function safeRoot(raw) {
  if (typeof raw !== 'string') return null;
  if (!raw.trim() || raw.length > ROOT_MAX_LEN) return null;   // reject, never truncate — a shorter path is a different folder
  if (clean(raw, ROOT_MAX_LEN) !== raw) return null;           // NUL/control bytes: reject outright, never scrub into a different path
  const s = raw.trim();
  if (!path.isAbsolute(s)) return null;                        // full folder paths only — never relative to wherever the hub was started
  // A network share is NOT this machine. isAbsolute() and realpath both happily
  // accept \\server\share, which would let a "standing order" write to another
  // host over SMB — git-push has to stay the only outward action there is.
  // (It is also what froze the hub for 21s when a share was unreachable.)
  if (/^(\\\\|\/\/)/.test(s)) return null;
  let p;
  try { p = fs.realpathSync(path.resolve(s)); } catch { return null; }  // must EXIST; realpath defeats symlink games
  if (/^(\\\\|\/\/)/.test(p)) return null;                     // re-check: a local path can still resolve onto a share
  try { if (!fs.statSync(p).isDirectory()) return null; } catch { return null; }
  if (p.split(/[\\/]+/).some(seg => seg.toLowerCase() === '.claude')) return null; // agent config trees are off limits
  const state = normRoot(path.resolve(STATE_DIR));
  if (normRoot(p) === state || normRoot(p).startsWith(state + path.sep)) return null; // and AMC's own state
  // A whole drive is not a project folder, and neither is a system tree — nobody
  // means "create C:\Windows\System32\CLAUDE.md" when they paste a path.
  if (normRoot(p) === normRoot(path.parse(p).root)) return null; // p IS the drive / filesystem root
  const np = normRoot(p);
  const banned = [process.env.SystemRoot, process.env.ProgramFiles, process.env['ProgramFiles(x86)'], process.env.ProgramData,
    '/etc', '/usr', '/bin', '/sbin', '/System', '/Library'].filter(Boolean).map(d => normRoot(path.resolve(d)));
  if (banned.some(d => np === d || np.startsWith(d + path.sep))) return null;
  return p;
}
// Plantable guidance files: one CLAUDE.md per known Claude project cwd
// (creatable if absent), the global CLAUDE.md, any existing AGENTS.md, plus
// CLAUDE.md/AGENTS.md in each folder the owner added by hand.
function directiveTargets() {
  const home = os.homedir();
  const targets = [];
  const seen = new Set();
  const add = (label, p, name) => {
    if (seen.has(p)) return; seen.add(p);
    let exists = false, size = 0;
    try { const st = fs.statSync(p); exists = st.isFile(); size = st.size; } catch { /* creatable */ }
    if (exists && size > BRAIN_MAX) return; // refuse to touch oversized files
    targets.push({ id: enc(p), label, name, path: p, exists });
  };
  add('Global — every session on this machine', path.join(home, '.claude', 'CLAUDE.md'), 'CLAUDE.md');
  try {
    for (const proj of fs.readdirSync(PROJECTS_DIR)) {
      const cwd = claudeProjectCwd(path.join(PROJECTS_DIR, proj));
      if (!cwd) continue;
      const label = proj.replace(/^[Cc]--Users-[^-]+-/, '').slice(0, 40);
      add(label, path.join(cwd, 'CLAUDE.md'), 'CLAUDE.md');
      try { if (fs.statSync(path.join(cwd, 'AGENTS.md')).isFile()) add(label + ' (Codex)', path.join(cwd, 'AGENTS.md'), 'AGENTS.md'); } catch { /* absent */ }
    }
  } catch { /* ignore */ }
  // Folders the owner added. Re-gated here, not trusted from the store, and the
  // filename is always a literal out of ROOT_FILES — the root only supplies a dir.
  for (const r of loadDirectiveRoots().roots) {
    const root = safeRoot(r.path);
    if (!root) continue; // moved, deleted, or no longer allowed — silently not offered
    const label = (path.basename(root) || root).slice(0, 40);
    for (const name of ROOT_FILES) add(name === 'AGENTS.md' ? label + ' (Codex)' : label, path.join(root, name), name);
  }
  return targets;
}
const dirMarker = id => ({ start: `<!-- mission-control:directive:${id} -->`, end: `<!-- /mission-control:directive:${id} -->` });
function directiveBlock(d) {
  const m = dirMarker(d.id);
  return `\n\n${m.start}\n## 🛰 Standing order: ${d.title}\n_Planted by Agent Mission Control on ${new Date(d.createdAt).toISOString().slice(0, 10)}. Retire it from the dashboard rather than hand-editing this block._\n\n${d.body.trim()}\n${m.end}\n`;
}
// A guidance file that is a symlink or a hardlink is really some OTHER file
// wearing this name: backing it up would copy that file's contents into this
// folder, and writing through it edits a target we never validated. Refuse.
function linkedAway(p) {
  try {
    const st = fs.lstatSync(p);
    if (st.isSymbolicLink()) return 'symbolic link';
    if (st.nlink > 1) return 'hard link';
  } catch { /* absent = fine, we're creating it */ }
  return null;
}
function plantIntoFile(p, d) {
  const link = linkedAway(p);
  if (link) return { status: 'error', error: `that file is a ${link} to somewhere else — edit it directly instead` };
  let cur = '';
  try { cur = fs.readFileSync(p, 'utf8'); } catch { /* new file */ }
  const m = dirMarker(d.id);
  if (cur.includes(m.start)) return { status: 'already' };
  const next = cur + directiveBlock(d);
  if (Buffer.byteLength(next) > BRAIN_MAX) return { status: 'too-big' };
  try {
    if (cur) { fs.copyFileSync(p, p + '.mc-backup'); snapshotBrain({ path: p, name: path.basename(p) }); }
    else fs.mkdirSync(path.dirname(p), { recursive: true });
    const tmp = p + '.mc-tmp';
    fs.writeFileSync(tmp, next);
    fs.renameSync(tmp, p);
    return { status: 'planted' };
  } catch (e) { return { status: 'error', error: e.message }; }
}
function retireFromFile(p, id) {
  const link = linkedAway(p);
  if (link) return { status: 'error', error: `that file is a ${link} to somewhere else — edit it directly instead` };
  const m = dirMarker(id);
  let cur;
  try { cur = fs.readFileSync(p, 'utf8'); } catch { return { status: 'missing-file' }; }
  const i = cur.indexOf(m.start), j = cur.indexOf(m.end);
  if (i < 0 || j < 0) return { status: 'not-present' };
  try {
    fs.copyFileSync(p, p + '.mc-backup');
    snapshotBrain({ path: p, name: path.basename(p) });
    const next = cur.slice(0, i).replace(/\n+$/, '\n') + cur.slice(j + m.end.length).replace(/^\n+/, '\n');
    const tmp = p + '.mc-tmp';
    fs.writeFileSync(tmp, next);
    fs.renameSync(tmp, p);
    return { status: 'retired' };
  } catch (e) { return { status: 'error', error: e.message }; }
}
function checkDirective(d) {
  return (d.targets || []).map(t => {
    try { return { path: t.path, label: t.label, status: fs.readFileSync(t.path, 'utf8').includes(dirMarker(d.id).start) ? 'ok' : 'drifted' }; }
    catch { return { path: t.path, label: t.label, status: 'missing-file' }; }
  });
}

// ---------- git as transport (how a planted rule reaches other machines) ----------
// AMC never writes to a remote machine. Instead, when a guidance file lives in a
// git repo, the owner commits the planted block and pushes it — the other machines
// receive it the normal way, with `git pull`, and `git revert` undoes it everywhere.
// Rules for everything below: execFile with an explicit argv array (never a shell
// string, never shell:true), cwd pinned to the repo root, a hard timeout, and every
// failure returned as text instead of thrown. The only paths ever handed to git are
// ones already recorded in a directive's own targets — the client sends ids, and a
// path it sends is only accepted after matching a recorded target exactly.
const { execFile } = require('child_process');
const GIT_TIMEOUT = 15000;
function git(cwd, args) {
  return new Promise(resolve => {
    execFile('git', args, {
      cwd, timeout: GIT_TIMEOUT, windowsHide: true, maxBuffer: 1024 * 1024,
      // never let git stop and wait for a password: no credential handling here, ever
      env: { ...process.env, GIT_TERMINAL_PROMPT: '0', GIT_OPTIONAL_LOCKS: '0' },
    }, (err, stdout, stderr) => {
      if (!err) return resolve({ ok: true, out: String(stdout).trim(), error: null });
      const raw = err.code === 'ENOENT' ? 'git is not installed on this machine'
        : err.killed ? 'git took longer than 15 seconds and was stopped'
          : (stderr || err.message || 'git failed');
      resolve({ ok: false, out: String(stdout || '').trim(), error: clean(String(raw).replace(/\s+/g, ' ').trim(), 400) });
    });
  });
}
// Same contract as git(), but feeds paths on stdin. Needed because some git
// subcommands (notably `check-ignore -z`) REFUSE an argv path list and only
// accept --stdin. Exit 1 is a normal "no matches" answer for check-ignore, so it
// is reported as ok with empty output rather than as a failure.
function gitStdin(cwd, args, input) {
  return new Promise(resolve => {
    const child = execFile('git', args, {
      cwd, timeout: GIT_TIMEOUT, windowsHide: true, maxBuffer: 1024 * 1024,
      env: { ...process.env, GIT_TERMINAL_PROMPT: '0', GIT_OPTIONAL_LOCKS: '0' },
    }, (err, stdout, stderr) => {
      if (!err) return resolve({ ok: true, out: String(stdout), error: null });
      if (err.code === 1) return resolve({ ok: true, out: String(stdout || ''), error: null }); // "none matched"
      const raw = err.code === 'ENOENT' ? 'git is not installed on this machine'
        : err.killed ? 'git took longer than 15 seconds and was stopped'
          : (stderr || err.message || 'git failed');
      resolve({ ok: false, out: '', error: clean(String(raw).replace(/\s+/g, ' ').trim(), 400) });
    });
    try { child.stdin.end(input); } catch { /* child already gone */ }
  });
}
// Repo root for a planted file. Returns the REASON on failure too — "git isn't
// installed" and "this folder isn't a repo" are very different sentences to show
// someone, and collapsing both to null told owners their repos were broken.
async function gitRepoRootInfo(filePath) {
  const dir = path.dirname(filePath);
  try { if (!fs.statSync(dir).isDirectory()) return { root: null, gitMissing: false }; } catch { return { root: null, gitMissing: false }; }
  const r = await git(dir, ['rev-parse', '--show-toplevel']);
  if (r.ok && r.out) return { root: path.normalize(r.out.split('\n')[0].trim()), gitMissing: false };
  return { root: null, gitMissing: /not installed/i.test(r.error || '') };
}
async function gitRepoRoot(filePath) { return (await gitRepoRootInfo(filePath)).root; }
// Read-only picture of one target: is it a repo, which branch, is the rule
// uncommitted, and how many commits are sitting here unsent.
async function gitTargetState(t, d) {
  const base = { path: t.path, label: t.label, name: t.name };
  const { root, gitMissing } = await gitRepoRootInfo(t.path);
  if (!root) return { ...base, isRepo: false, gitMissing };
  const [branch, st, ahead, ignored] = await Promise.all([
    // --show-current, not rev-parse: it still answers on a repo with no commits yet,
    // and returns empty (rather than the literal "HEAD") when the repo is detached
    git(root, ['branch', '--show-current']),
    git(root, ['status', '--porcelain', '--', t.path]),
    git(root, ['rev-list', '--count', '@{upstream}..HEAD']),
    git(root, ['check-ignore', '-q', '--', t.path]), // exit 0 = git deliberately ignores this file
  ]);
  // Whether THIS order is committed, not whether the file is clean. Two orders
  // commonly share one CLAUDE.md, so a file-level check mislabels both of them.
  let planted = null;
  const rel = path.relative(root, t.path).split(path.sep).join('/');
  const show = await git(root, ['show', 'HEAD:' + rel]);
  if (show.ok && d) planted = show.out.includes(dirMarker(d.id).start);
  return {
    ...base, isRepo: true, root, gitMissing: false,
    ignored: ignored.ok,                                    // true = in .gitignore, can never be shared
    branch: (branch.ok && branch.out) ? branch.out : null,  // null = detached, i.e. not on a branch
    fileDirty: st.ok ? st.out !== '' : false,               // the file differs from the last commit
    committed: planted,                                     // null = unknown (no commits yet / new file)
    ahead: ahead.ok ? (parseInt(ahead.out, 10) || 0) : null, // null = no remote branch set up
  };
}
async function gitStatesFor(d) {
  const list = (d.targets || []).slice(0, 40);
  const out = [];
  // small batches: a 40-target order shouldn't spawn 160 git processes at once
  for (let i = 0; i < list.length; i += 4) out.push(...await Promise.all(list.slice(i, i + 4).map(t => gitTargetState(t, d))));
  if ((d.targets || []).length > list.length) out.push({ truncated: (d.targets || []).length - list.length });
  return out;
}
// Stage ONLY this one guidance file and commit ONLY that path (the `-- <path>`
// pathspec means anything else the owner had staged stays staged, untouched).
async function gitCommitDirective(d, t) {
  const { root, gitMissing } = await gitRepoRootInfo(t.path);
  if (gitMissing) return { done: false, note: 'git is not installed on this computer, so nothing can be shared from here.' };
  if (!root) return { done: false, note: 'Not a git repo — nothing to commit.' };
  // A commit made while not on a branch is unreachable: the next branch switch
  // silently throws it away, taking the rule with it. Refuse rather than report success.
  const br = await git(root, ['branch', '--show-current']);
  if (br.ok && !br.out) return { done: false, note: 'This repo is not on a branch right now, so a commit here would be lost the moment you switch branches. Switch to a branch first, then commit.' };
  const add = await git(root, ['add', '--', t.path]);
  if (!add.ok) {
    if (/ignored by/i.test(add.error)) return { done: false, note: 'This file is listed in .gitignore, so git deliberately does not track it — it cannot be shared this way.' };
    return { done: false, note: 'Could not stage the file: ' + add.error };
  }
  const staged = await git(root, ['diff', '--cached', '--name-only', '--', t.path]);
  if (staged.ok && !staged.out) return { done: false, note: 'Nothing to commit — this file already matches the last commit.' };
  const subject = 'AMC standing order: ' + clean(d.title, 72);
  // Careful wording: one guidance file can hold several orders, so this commit may
  // carry more than the one named in the subject. Say that rather than misattribute.
  const body = `Updates ${path.basename(t.path)} with standing orders planted by Agent Mission Control, including: ${clean(d.title, 200)}.\nEvery agent session in this repo reads them. To undo this change everywhere: git revert this commit.\n\nMission-Control-Directive: ${clean(d.id, 40)}`;
  const c = await git(root, ['commit', '-m', subject, '-m', body, '--', t.path]);
  if (!c.ok) return { done: false, note: 'Commit failed: ' + c.error };
  const sha = await git(root, ['rev-parse', '--short', 'HEAD']);
  return { done: true, note: `Committed${sha.ok && sha.out ? ' as ' + sha.out : ''} on this machine. Nothing has been sent anywhere yet.` };
}
// The one outward step. Deliberate, separate, never bundled into plant or commit.
async function gitPushRepo(t) {
  const root = await gitRepoRoot(t.path);
  if (!root) return { done: false, note: 'Not a git repo — nothing to send.' };
  const r = await git(root, ['push']);
  if (!r.ok) return { done: false, note: 'Sending failed: ' + r.error };
  return { done: true, note: 'Sent. Your other machines get it the next time they run git pull.' };
}

// ---------- brain version history ----------
// Every Brain save snapshots the PRIOR content, so config/memory/hooks edits
// are recoverable — real version control, not a single .mc-backup.
const BRAIN_HIST = path.join(STATE_DIR, 'brain-history');
function brainHistDir(p) { return path.join(BRAIN_HIST, crypto.createHash('sha1').update(p).digest('hex')); }
function snapshotBrain(item) {
  try {
    const dir = brainHistDir(item.path);
    fs.mkdirSync(dir, { recursive: true });
    const stamp = String(Date.now());
    fs.copyFileSync(item.path, path.join(dir, stamp + '.snap'));
    // keep a small index with the display name + size
    const idxFile = path.join(dir, 'index.json');
    let idx = [];
    try { idx = JSON.parse(fs.readFileSync(idxFile, 'utf8')); } catch { /* new */ }
    idx.push({ stamp, name: item.name, size: fs.statSync(item.path).size });
    idx = idx.slice(-40); // cap history depth
    fs.writeFileSync(idxFile, JSON.stringify(idx));
    // prune snapshots not in the index
    const keep = new Set(idx.map(x => x.stamp + '.snap'));
    for (const f of fs.readdirSync(dir)) if (f.endsWith('.snap') && !keep.has(f)) { try { fs.unlinkSync(path.join(dir, f)); } catch { /* ignore */ } }
  } catch (e) { console.error('brain snapshot failed:', e.message); }
}
function brainHistory(p) {
  try { return JSON.parse(fs.readFileSync(path.join(brainHistDir(p), 'index.json'), 'utf8')).reverse(); } catch { return []; }
}
function brainSnapshotContent(p, stamp) {
  if (!/^\d+$/.test(String(stamp))) return null;
  try { return fs.readFileSync(path.join(brainHistDir(p), stamp + '.snap'), 'utf8'); } catch { return null; }
}

// ---------- brain center (local memories, hooks, agent configs) ----------
// Read/write the agent "brains" on THIS machine only: Claude global memory,
// per-project memory stores, hook settings, Codex AGENTS.md/config. Same
// loopback+origin+CSRF gating as metadata — these files steer your agents,
// so they are never exposed to the LAN and never editable remotely.
const BRAIN_MAX = 512 * 1024;
// Resolve a Claude project's real working directory from its transcript (the
// slug is lossy, but transcripts record the true cwd).
const cwdCache = new Map();
function claudeProjectCwd(projDir) {
  if (cwdCache.has(projDir)) return cwdCache.get(projDir);
  let cwd = null;
  try {
    const files = fs.readdirSync(projDir).filter(f => f.endsWith('.jsonl'));
    for (const f of files.slice(0, 3)) {
      const fd = fs.openSync(path.join(projDir, f), 'r');
      const buf = Buffer.alloc(Math.min(16384, fs.statSync(path.join(projDir, f)).size));
      fs.readSync(fd, buf, 0, buf.length, 0); fs.closeSync(fd);
      const m = /"cwd":"((?:[^"\\]|\\.)+)"/.exec(buf.toString('utf8'));
      if (m) { try { cwd = JSON.parse('"' + m[1] + '"'); break; } catch { /* keep looking */ } }
    }
  } catch { /* ignore */ }
  cwdCache.set(projDir, cwd);
  return cwd;
}

function brainInventory() {
  const home = os.homedir();
  const items = [];
  const seen = new Set();
  const add = (category, p, name) => {
    try {
      if (seen.has(p)) return;
      const st = fs.statSync(p);
      if (st.isFile() && st.size <= BRAIN_MAX) { seen.add(p); items.push({ id: enc(p), category, path: p, name: name || path.basename(p), size: st.size, mtime: st.mtimeMs }); }
    } catch { /* absent */ }
  };
  add('Claude · global', path.join(home, '.claude', 'CLAUDE.md'), 'CLAUDE.md (global memory)');
  add('Claude · hooks & settings', path.join(home, '.claude', 'settings.json'), 'settings.json (hooks, permissions)');
  add('Claude · hooks & settings', path.join(home, '.claude', 'settings.local.json'), 'settings.local.json');
  try {
    for (const proj of fs.readdirSync(PROJECTS_DIR)) {
      const projDir = path.join(PROJECTS_DIR, proj);
      const label = proj.replace(/^[Cc]--Users-[^-]+-/, '').slice(0, 30);
      // memory store
      try {
        for (const f of fs.readdirSync(path.join(projDir, 'memory'))) {
          if (f.endsWith('.md')) add('Claude · project memory (' + label + ')', path.join(projDir, 'memory', f));
        }
      } catch { /* none */ }
      // guidance docs in the actual project working directory
      const cwd = claudeProjectCwd(projDir);
      if (cwd) {
        add('Guidance · ' + label, path.join(cwd, 'CLAUDE.md'), 'CLAUDE.md');
        add('Guidance · ' + label, path.join(cwd, 'CLAUDE.local.md'), 'CLAUDE.local.md');
        add('Guidance · ' + label, path.join(cwd, 'AGENTS.md'), 'AGENTS.md');
        add('Guidance · ' + label, path.join(cwd, '.claude', 'settings.json'), 'project settings.json (hooks)');
        add('Guidance · ' + label, path.join(cwd, '.cursorrules'), '.cursorrules');
      }
    }
  } catch { /* ignore */ }
  add('Codex', path.join(home, '.codex', 'AGENTS.md'), 'AGENTS.md (Codex global instructions)');
  add('Codex', path.join(home, '.codex', 'config.toml'), 'config.toml');
  return items;
}
function brainResolve(id) {
  // an id is only valid if it names a file the inventory would list right now
  const p = decodeURIComponent(id || '');
  return brainInventory().find(i => i.path === p) || null;
}

// gate applied to EVERY /api/meta* route (reads included — note is free text)
function metaGate(req, res) {
  const ra = req.socket.remoteAddress || '';
  if (!['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(ra)) { json(res, { error: 'loopback only' }, 403); return false; }
  const host = (req.headers.host || '').toLowerCase();
  if (!/^(localhost|127\.0\.0\.1)(:\d+)?$/.test(host)) { json(res, { error: 'bad host' }, 421); return false; }
  const sfs = req.headers['sec-fetch-site'];
  if (sfs && !['same-origin', 'none'].includes(sfs)) { json(res, { error: 'cross-site' }, 403); return false; }
  const origin = req.headers.origin;
  if (origin && origin !== 'http://' + host) { json(res, { error: 'bad origin' }, 403); return false; }
  return true;
}
function writeGate(req, res) {
  if (!metaGate(req, res)) return false;
  if ((req.headers['content-type'] || '').indexOf('application/json') !== 0) { json(res, { error: 'json only' }, 415); return false; }
  if (req.headers['x-mc-csrf'] !== META_CSRF) { json(res, { error: 'bad csrf' }, 403); return false; }
  return true;
}
function metaErr(res, e) { json(res, { error: e.message }, e.code || (e.status409 ? 409 : 400)); }

function applySessionPatch(next, key, patch) {
  if (!knownStableKey(key)) { const e = new Error('unknown session'); throw e; }
  const cur = next.sessions[key] || { tags: [] };
  if ('projectId' in patch) {
    if (patch.projectId !== null && !next.projects.some(p => p.id === patch.projectId)) throw new Error('no such project');
    cur.projectId = patch.projectId;
  }
  if ('archived' in patch) cur.archived = !!patch.archived;
  if ('pinned' in patch) cur.pinned = !!patch.pinned;
  if ('note' in patch) cur.note = clean(patch.note, LIM.note);
  if ('tags' in patch) {
    if (!Array.isArray(patch.tags)) throw new Error('tags must be array');
    const valid = patch.tags.filter(t => next.tags.some(x => x.id === t)).slice(0, LIM.tagsPerSession);
    cur.tags = [...new Set(valid)];
  }
  cur.tags = cur.tags || [];
  cur.updatedAt = metaClock();
  next.sessions[key] = cur;
}
// monotonic-ish clock without Date.now() at module top (allowed inside handlers)
function metaClock() { return Date.now(); }

// WHY: a relayed session used to be labelled with its MACHINE, because that was
// the only identity the hub had — one relay's 205 sessions all read
// "⇄ trifecta-erp" and no remote session could be told apart by folder, while
// local ones showed real project slugs. The relay now sends the project it
// already knows (proj.cwd / proj.slug); this reduces that to one folder name a
// person can read. Three sources, best first, so nothing is ever orphaned:
//   1. proj.cwd  — the true working directory, so "BukkakERP" not a slug
//   2. proj.slug — the project-directory slug, when the cwd was unresolvable
//   3. meta.project — what EVERY relay ever built already sends inside meta, so
//      sessions cached before this field existed get real names too
//   4. nothing   — label stays exactly what it is today: just the machine
function relayProjLabel(s) {
  const p = s.proj || {};
  const cwd = clean(p.cwd, 400).replace(/[\\/]+$/, '');
  if (cwd) { const base = cwd.split(/[\\/]+/).pop(); if (base) return base.slice(0, 60); }
  const slug = clean(p.slug || (s.meta && s.meta.project) || '', 200);
  const label = slug.replace(/^Codex\s*·\s*/, '').replace(/^[Cc]--Users-[^-]+-/, '').slice(0, 60);
  // Some relayed Codex sessions carry no project at all, and their meta.project is
  // derived from the session TITLE — which produced labels like "can" out of "can i
  // use this model…". The tell is that the label is just the start of the title; a
  // real folder name almost never is. A meaningless word is worse than admitting we
  // don't know, so those are refused.
  if (!label || /\s/.test(label)) return '';
  const title = String((s.meta && s.meta.title) || '').toLowerCase();
  if (label.length < 12 && title.startsWith(label.toLowerCase())) return '';
  return label;
}

function relayList() {
  return [...relaySessions.values()].map(s => {
    let title = s.meta.title;
    if (!title) {
      const firstUser = s.result.events.find(e => (e.kind === 'user-text' || e.kind === 'user-queued') && e.text && !e.text.startsWith('<'));
      title = firstUser ? clip(firstUser.text.replace(/\s+/g, ' ').trim(), 70) : s.id.split(':').pop().slice(0, 12);
    }
    // The machine stays in FRONT of the project: it is what keeps two machines
    // that both have a folder called "web" — or a remote folder that matches a
    // local one — from collapsing into a single project everywhere the UI
    // groups by this string. Machine also remains its own field + filter.
    const proj = relayProjLabel(s);
    return {
      project: '⇄ ' + s.machine + (proj ? ' · ' + proj : ''), projPath: (s.proj && s.proj.cwd) || null,
      file: s.id, session: s.meta.session || s.id, machine: s.machine,
      title: title + ' · ' + s.machine,
      size: s.result.events.length, mtime: s.meta.mtime || Date.now(), agentCount: s.result.agents.length - 1,
    };
  });
}

// Resolve any source: "otel:<service>" / "relay:<machine>:<file>" (in-memory)
// or a local transcript path.
function getResult(fileParam) {
  if (fileParam.startsWith('otel:')) {
    const sess = otelSessions.get(fileParam);
    return sess ? otelResult(sess) : null;
  }
  if (fileParam.startsWith('relay:')) {
    const sess = relaySessions.get(fileParam);
    return sess ? sess.result : null;
  }
  if (fileParam.startsWith('codex:')) return readCodexCached(fileParam.slice(6));
  if (fileParam.startsWith('archive:')) {
    // archive:<machine>:<relPath> — a full raw transcript pulled from a relay.
    // The subagent tree is preserved alongside it, so readSession works as-is.
    const rest = fileParam.slice(8);
    const ci = rest.indexOf(':');
    const machine = rest.slice(0, ci), rel = rest.slice(ci + 1);
    const base = path.resolve(archiveMachineDir(machine));
    const full = path.resolve(base, rel.replace(/\\/g, '/'));
    if (!full.startsWith(base + path.sep) || !full.endsWith('.jsonl') || !fs.existsSync(full)) return null;
    return readSession(full);
  }
  const full = resolveSessionPath(fileParam);
  if (!full || !fs.existsSync(full)) return null;
  return readSession(full);
}

// The hub refuses any body past this. ARCHIVE_SEND_CAP is derived from it rather
// than hardcoded, because they drifted apart once and cost a production server 6GB
// of RAM: the relay happily accepted a 107MB transcript, base64 inflated it past
// this limit, the hub destroyed the request, and the relay retried it forever.
const BODY_MAX_BYTES = 50e6;
function readBody(req, cb) {
  let b = '';
  req.on('data', c => { b += c; if (b.length > BODY_MAX_BYTES) req.destroy(); });
  req.on('end', () => cb(b));
}

// ---------- which models actually ran (fleet-wide) ----------
// WHY: /api/fleet reported cost and agent counts but nothing about WHICH models
// did the work, so a session that put nearly all of its money on the top tier
// because a model was left unset looked exactly like a cheap one — the only way
// to find it was opening sessions one at a time. Everything below is derived
// from the r.agents/r.events sessionSummary has ALREADY parsed: no extra file
// read, no git, no second pass, so /api/fleet costs the same as before.
// The full model id is kept on purpose ('claude-opus-4-8', never 'Opus') — when
// a default quietly changes underneath you, the version IS the story.
// Order matters, and it used to be wrong: family names were tested BEFORE size
// qualifiers, so `gpt-5-mini` matched /gpt-5/ and got billed as premium while the
// /mini/ rule sat unreachable. That inflates "top-tier spend" — the one number
// this whole feature exists to report honestly. Size qualifiers first, always.
function modelTier(model) {
  const m = String(model || '').toLowerCase();
  if (/mini|nano|flash|haiku|lite/.test(m)) return 'cheap';
  if (/fable|mythos/.test(m)) return 'flagship';
  if (/sonnet|codex|gpt-4/.test(m)) return 'mid';
  if (/opus|gpt-5|o3|o1/.test(m)) return 'premium';
  if (/gpt-3/.test(m)) return 'cheap';
  return 'unknown';
}
const TIERS = ['flagship', 'premium', 'mid', 'cheap', 'unknown'];
const TOP_TIERS = ['flagship', 'premium'];        // the two that make a bill hurt
const LIVE_WINDOW_MS = 600000;                    // "active recently" — the same window `stalled` uses
const LIVE_AGENTS_MAX = 6;                        // a glance at who's working, not a roster
const dollars = n => Math.round(n * 10000) / 10000; // agent costs are already 4dp; don't invent precision

// One row per model id that actually ran, dearest first, plus the same money
// split by tier. Agents we never saw a model line for are counted separately
// instead of being folded into a tier they might not belong to.
function modelBreakdown(agents) {
  const by = new Map();
  const tierMix = { flagship: 0, premium: 0, mid: 0, cheap: 0, unknown: 0 };
  let agentsNoModel = 0;
  for (const a of agents) {
    if (!a.model) { agentsNoModel++; continue; }
    const tier = modelTier(a.model);
    const row = by.get(a.model) || { id: a.model, tier, agents: 0, cost: 0, outTokens: 0 };
    row.agents++;
    row.cost += a.cost || 0;
    row.outTokens += a.outTokens || 0;
    by.set(a.model, row);
    tierMix[tier] += a.cost || 0;
  }
  const total = TIERS.reduce((n, t) => n + tierMix[t], 0);
  const top = TOP_TIERS.reduce((n, t) => n + tierMix[t], 0);
  // No money means no split to report. A 0 here would render as "0% on the
  // expensive tiers" — i.e. all clear — which is the opposite of "can't tell".
  const topTierShare = total > 0 ? Math.round((top / total) * 1000) / 1000 : null;
  for (const t of TIERS) tierMix[t] = dollars(tierMix[t]);
  const models = [...by.values()]
    .sort((x, y) => y.cost - x.cost || y.agents - x.agents)
    .map(r => ({ ...r, cost: dollars(r.cost) }));
  return { models, tierMix, topTierShare, agentsNoModel };
}

// Who is working right now. Gated on the same 10-minute recency `stalled` uses,
// then per agent: sitting on an unresolved tool call, or its own last line is
// inside that window. Agents mid-call are listed first so the cap never hides
// the one that is stuck, and liveAgentCount carries the real total so a capped
// list can never be mistaken for the whole crew.
function liveAgentsOf(agents, mtime, now) {
  if (now - mtime > LIVE_WINDOW_MS) return [];
  const out = [];
  for (const a of agents) {
    let pend = a.pendingTool && a.pendingTool.since ? a.pendingTool : null;
    // An unresolved tool call is not proof of life: a call that went out four hours
    // ago and never came back means STUCK, not "working". Showing those first, with
    // no age, made a dead session look like the busiest thing on the screen.
    const pendAge = pend ? now - Date.parse(pend.since) : NaN;
    const stuck = pend && Number.isFinite(pendAge) && pendAge > LIVE_WINDOW_MS;
    if (stuck) pend = null;
    const lastTs = a.lastTs ? new Date(a.lastTs).getTime() : NaN;
    const fresh = Number.isFinite(lastTs) && now - lastTs <= LIVE_WINDOW_MS;
    if (a.done || (!pend && !fresh)) continue;
    out.push({
      name: a.name || (a.id === 'main' ? 'Main agent' : a.id),
      model: a.model || null,
      tool: pend ? pend.tool : null,
      since: pend ? pend.since : null,
      waitingMs: pend && Number.isFinite(pendAge) ? Math.max(0, Math.round(pendAge)) : null,
    });
  }
  out.sort((x, y) => (y.tool ? 1 : 0) - (x.tool ? 1 : 0));
  return out;
}

function sessionSummary(meta) {
  let r;
  try { r = getResult(meta.file); } catch { return null; }
  if (!r) return null;
  const now = Date.now();
  const mix = modelBreakdown(r.agents);
  const live = liveAgentsOf(r.agents, meta.mtime, now);
  const evs = r.events;
  const first = evs.find(e => e.ts), last = [...evs].reverse().find(e => e.ts);
  const machine = meta.file.startsWith('relay:') ? meta.file.split(':')[1] : os.hostname();
  return {
    file: meta.file, project: meta.project, projPath: meta.projPath || null, session: meta.session, title: meta.title, mtime: meta.mtime,
    kind: agentKindOf(meta.file), machine, stableKey: stableKeyForItem(meta),
    agents: r.agents.length, events: evs.length,
    toolCalls: evs.filter(e => e.kind === 'tool-call' || e.kind === 'spawn').length,
    errors: evs.filter(e => e.error).length,
    durationMs: first && last ? new Date(last.ts) - new Date(first.ts) : 0,
    tokensIn: r.agents.reduce((n, a) => n + a.inTokens, 0),           // fresh input only
    tokensCache: r.agents.reduce((n, a) => n + (a.cacheTokens || 0), 0), // cache reads (re-billed prefix, 0.1x rate)
    tokensOut: r.agents.reduce((n, a) => n + a.outTokens, 0),
    cost: Math.round(r.agents.reduce((n, a) => n + (a.cost || 0), 0) * 100) / 100,
    retrying: r.agents.some(a => a.retrying),
    stalled: r.agents.some(a => a.pendingTool && a.pendingTool.since && now - new Date(a.pendingTool.since) > 120000) && now - meta.mtime < LIVE_WINDOW_MS,
    // model identity — see modelBreakdown()/liveAgentsOf() above
    models: mix.models,                 // [{id, tier, agents, cost, outTokens}] dearest first
    tierMix: mix.tierMix,               // {flagship, premium, mid, cheap, unknown} in dollars
    topTierShare: mix.topTierShare,     // 0..1 of this session's money on flagship+premium, null when cost is 0
    agentsNoModel: mix.agentsNoModel,   // agents whose model was never recorded (so counts add up honestly)
    liveAgents: live.slice(0, LIVE_AGENTS_MAX), // [{name, model, tool, since}] — empty unless active recently
    liveAgentCount: live.length,        // real total, so "3 of 18" is always tellable
  };
}

// ---------- did it stick (session -> git) ----------
// A session can finish GREEN and still leave nothing behind: the edit was undone
// later, made in a different copy of the folder, or never saved. Nothing else in
// this dashboard catches that, so this compares the files a session SAID it wrote
// against what git actually shows changed since the session began.
// Rules: read-only git only (log, diff, status) through the argv-only git()
// helper above, never a path the browser sent, and deliberately timid — anything
// that cannot be proven from evidence comes back 'unknown' with a plain sentence,
// because a badge that is sometimes wrong is worse than no badge at all.
const STICK_TOOLS = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit']); // the write-style tools
const STICK_MAX_PATHS = 60;   // hard cap: one git call, never an unbounded argv
const stickCache = new Map(); // file@mtime -> result (the answer can only change when the file does)

// The file one write-style call targeted, recovered from the event summary.
// summarizeInput() already reduces Edit/Write/MultiEdit to their file_path;
// NotebookEdit has no matching key so it arrives as (possibly clipped) JSON.
// Anything that is not a plain absolute path is dropped rather than guessed at.
function stickPathOf(evt) {
  const raw = String(evt.full || evt.text || '').trim();
  if (!raw) return null;
  if (raw[0] === '{') {
    const m = /"(?:file_path|notebook_path)"\s*:\s*"((?:[^"\\]|\\.)*)"/.exec(raw);
    if (!m) return null;
    try {
      const p = JSON.parse('"' + m[1] + '"');
      return path.isAbsolute(p) ? path.normalize(p) : null;
    } catch { return null; }
  }
  if (raw.endsWith('…')) return null; // clip() truncated it: not a path we can trust
  return path.isAbsolute(raw) ? path.normalize(raw) : null;
}

// The folder a session actually worked in. The project slug is lossy, so prefer
// the cwd this transcript recorded and only fall back to the project's.
function sessionCwd(sessionPath) {
  try {
    const size = fs.statSync(sessionPath).size;
    const fd = fs.openSync(sessionPath, 'r');
    const buf = Buffer.alloc(Math.min(16384, size));
    fs.readSync(fd, buf, 0, buf.length, 0); fs.closeSync(fd);
    const m = /"cwd":"((?:[^"\\]|\\.)+)"/.exec(buf.toString('utf8'));
    if (m) return JSON.parse('"' + m[1] + '"');
  } catch { /* fall through to the project-level answer */ }
  return claudeProjectCwd(path.dirname(sessionPath));
}

// Compare on Windows the way Windows does: same file, different capitalisation
// must not read as "gone". Everywhere else the filesystem is case-sensitive and
// folding case would invent matches that are not there.
// macOS is case-insensitive by default too, so folding only on Windows made a
// path whose capitalisation differed from git's index read as a missing file.
const stickKey = p => (process.platform === 'win32' || process.platform === 'darwin' ? p.toLowerCase() : p);

async function computeStickiness(sessionPath) {
  const r = readSession(sessionPath);
  const wrote = [...new Set(r.events
    .filter(e => e.kind === 'tool-call' && STICK_TOOLS.has(e.tool))
    .map(stickPathOf).filter(Boolean))];
  // Checked before any git runs at all: a research or question-answering session
  // edited nothing, so there is nothing to score and it must never be badged.
  if (!wrote.length) return { status: 'not-scored', reason: 'This session did not edit any files.' };

  const startEvt = r.events.find(e => e.ts && !isNaN(new Date(e.ts).getTime()));
  if (!startEvt) return { status: 'unknown', reason: 'This session has no recorded start time, so there is nothing to compare it against.' };
  const startIso = new Date(startEvt.ts).toISOString(); // rebuilt from a real Date: never raw transcript text

  const cwd = sessionCwd(sessionPath);
  if (!cwd) return { status: 'unknown', reason: 'Could not tell which folder this session was working in.' };
  // gitRepoRootInfo takes a FILE path and looks at the folder around it, so hand
  // it a nominal child of the working directory. Nothing is read or written there.
  const { root, gitMissing } = await gitRepoRootInfo(path.join(cwd, '.amc-stickiness-probe'));
  if (gitMissing) return { status: 'unknown', reason: 'git is not installed on this computer, so there is no history to check this session against.' };
  if (!root) {
    const gone = !fs.existsSync(cwd);
    return {
      status: 'unknown',
      reason: gone
        ? `The folder this session worked in (${clean(cwd, 160)}) is not on this computer any more, so its work cannot be checked.`
        : 'This session worked in a folder that git does not track, so there is no saved history to compare its work against.',
    };
  }

  // Only files inside the repo can be checked against its history. Editing files
  // elsewhere (global settings, notes) is normal, so this is "cannot tell", not a fail.
  const inRepo = wrote.filter(p => stickKey(p) === stickKey(root) || stickKey(p).startsWith(stickKey(root + path.sep)));
  if (!inRepo.length) {
    return { status: 'unknown', reason: 'Every file this session edited sits outside the project folder git tracks, so there is no history to check it against.' };
  }
  const paths = inRepo.slice(0, STICK_MAX_PATHS);
  const capped = inRepo.length - paths.length;

  // The commit that was current when the session started. No commit before that
  // moment means there is no "before" picture, so refuse rather than guess.
  const start = await git(root, ['log', '--before=' + startIso, '-1', '--format=%H']);
  if (!start.ok) return { status: 'unknown', reason: 'Could not read this project\'s saved history, so this session cannot be checked.' };
  const startCommit = start.out.split('\n')[0].trim();
  if (!/^[0-9a-f]{7,40}$/.test(startCommit)) {
    return { status: 'unknown', reason: 'This project had no saved history yet when the session started, so there is no "before" picture to compare against.' };
  }

  // ONE diff for every path at once — never one git per file.
  // -c core.quotepath=false so accented/non-ASCII names come back readable
  // instead of escaped, which would never match the paths we are holding.
  const diff = await git(root, ['-c', 'core.quotepath=false', 'diff', '--name-only', startCommit + '..HEAD', '--', ...paths]);
  if (!diff.ok) return { status: 'unknown', reason: 'Could not compare this session\'s files against your saved history, so it cannot be scored.' };
  const landedSet = new Set(diff.out.split('\n').map(s => s.trim()).filter(Boolean)
    .map(rel => stickKey(path.normalize(path.join(root, rel)))));

  const landedPaths = paths.filter(p => landedSet.has(stickKey(p)));
  let rest = paths.filter(p => !landedSet.has(stickKey(p)));

  // Two things can still explain a file that is missing from that diff, and both
  // of them mean the badge would otherwise be WRONG, so one more read-only call
  // settles them together:
  //   · uncommitted — the change is sitting on disk, not yet saved into history.
  //     It is in your code right now, which is exactly what was asked.
  //   · ignored — git was told never to track this file, so no history could
  //     ever show it. Unknowable, so it is dropped from the score entirely.
  // Ignored FOLDERS collapse to "dir/" in this output, hence the prefix branch.
  let pending = [], ignored = [], droppedDeleted = [];
  if (rest.length) {
    // -z, NOT plain --porcelain: git C-QUOTES any path containing a space
    // ("src/two words.js"), and keeping those quotes made the path match nothing,
    // which reported a file sitting safely in the working tree as vanished. The -z
    // form is never quoted and separates records with NUL.
    const st = await git(root, ['-c', 'core.quotepath=false', 'status', '--porcelain', '-z', '--ignored', '--', ...rest]);
    if (!st.ok) return { status: 'unknown', reason: 'Could not check the current state of this project\'s files, so this session cannot be scored.' };
    const dirty = new Set(), deleted = new Set(), ignoredExact = new Set(), ignoredDirs = [];
    const recs = st.out.split('\0').filter(s => s !== '');
    for (let i = 0; i < recs.length; i++) {
      const rec = recs[i];
      if (rec.length < 4) continue;
      const code = rec.slice(0, 2), body = rec.slice(3);
      // A rename emits the NEW name in this record and the OLD name in the next
      // one; consume that extra record so it is not read as its own entry.
      if (code[0] === 'R' || code[1] === 'R') i++;
      const abs = path.normalize(path.join(root, body.replace(/\/$/, '')));
      if (code === '!!') {
        if (/\/$/.test(body)) ignoredDirs.push(stickKey(abs + path.sep));
        else ignoredExact.add(stickKey(abs));
      } else if (code.includes('D')) {
        deleted.add(stickKey(abs));   // gone from disk: NOT an unsaved change
      } else dirty.add(stickKey(abs));
    }
    const isIgnored = p => ignoredExact.has(stickKey(p)) || ignoredDirs.some(d => stickKey(p).startsWith(d));
    ignored = rest.filter(isIgnored);
    rest = rest.filter(p => !isIgnored(p));
    pending = rest.filter(p => dirty.has(stickKey(p)));
    rest = rest.filter(p => !dirty.has(stickKey(p)));
    droppedDeleted = rest.filter(p => deleted.has(stickKey(p)));
    rest = rest.filter(p => !deleted.has(stickKey(p)));
  }

  // Before accusing anyone of losing work, look everywhere else it could be.
  // Work committed on a branch that is not checked out, or a file that was
  // renamed after the session wrote it, are both SAFE — and both looked identical
  // to "vanished" when we only compared startCommit..HEAD along a fixed path.
  let elsewhere = [];
  if (rest.length) {
    const anyRef = await git(root, ['log', '--all', '--format=%H', '-1', '--since=' + startIso, '--', ...rest]);
    const renamed = await git(root, ['log', '--format=%H', '-1', '--follow', '--diff-filter=R', startCommit + '..HEAD', '--', rest[0]]);
    if (!anyRef.ok || !renamed.ok) {
      return { status: 'unknown', reason: 'Could not check every place this work might be saved, so this session was not scored.' };
    }
    if (anyRef.out.trim() || renamed.out.trim()) {
      elsewhere = rest.slice();
      rest = [];
    }
  }

  const rel = p => path.relative(root, p).split(path.sep).join('/');
  const changed = paths.length - ignored.length;
  if (!changed) {
    return { status: 'unknown', reason: 'Every file this session edited is one git was told to ignore, so nothing recorded its work and it cannot be checked.' };
  }
  const present = landedPaths.length + pending.length + elsewhere.length;
  // "gone" is an ACCUSATION, so it needs proof, not just absence of evidence.
  // Anything we could not positively account for — a file deleted from disk after
  // the run, work found on another branch, a rename we resolved — makes this
  // "unknown" instead. A badge that is sometimes wrong is worse than no badge.
  let status = present === 0 ? 'gone' : rest.length === 0 ? 'stuck' : 'partial';
  if (status === 'gone' && droppedDeleted.length) {
    return { status: 'unknown', reason: 'Some files this session wrote are no longer on your computer, so there is no way to tell whether its work survived.' };
  }
  const notes = [];
  // Honest about the method, not just the verdict: this compares whole files, so
  // an edit that was later changed back is indistinguishable from no edit at all.
  if (status === 'gone') notes.push('Common causes: the work was undone afterwards, or it was done in a different copy of this folder. Unsaved changes on your computer, work saved on another branch, and renamed files are all counted as present, so none of those is the explanation.');
  if (elsewhere.length) notes.push(`${elsewhere.length === 1 ? 'One file was' : elsewhere.length + ' files were'} saved somewhere other than the branch you have open, or renamed since — counted as present.`);
  if (droppedDeleted.length) notes.push(`${droppedDeleted.length} ${droppedDeleted.length === 1 ? 'file is' : 'files are'} no longer on your computer, so ${droppedDeleted.length === 1 ? 'it was' : 'they were'} left out.`);
  if (pending.length) notes.push(pending.length === 1 && changed === 1
    ? 'That change is on your computer but not yet saved into the project\'s history.'
    : `${pending.length} of them ${pending.length === 1 ? 'is' : 'are'} changed on your computer but not yet saved into the project's history.`);
  if (status === 'partial') notes.push(`${rest.length} of ${rest.length === 1 ? 'them looks' : 'them look'} exactly as ${rest.length === 1 ? 'it' : 'they'} did before the session ran.`);
  if (ignored.length) notes.push(`${ignored.length} more ${ignored.length === 1 ? 'file was' : 'files were'} left out because git is told to ignore ${ignored.length === 1 ? 'it' : 'them'}.`);
  if (capped) notes.push(`Only the first ${STICK_MAX_PATHS} files were checked; this session touched ${capped} more.`);
  // The headline only ever speaks about this project — files written elsewhere
  // were never checked, so it must not imply they are gone too.
  const outside = wrote.length - inRepo.length;
  if (outside > 0) notes.push(`${outside} other ${outside === 1 ? 'file was' : 'files were'} written outside this project folder and ${outside === 1 ? 'was' : 'were'} not checked.`);

  return {
    status,
    reason: notes.join(' '),
    changed,                        // files this session wrote that git can actually check
    landed: present,                // ...that are different now from how they started
    missing: rest.map(rel),         // ...that are not
    pending: pending.map(rel),      // ...that changed but are not saved into history yet
    ignored: ignored.map(rel),      // ...that git is told to ignore, so they were not scored
    wrote: wrote.length,            // every file it wrote, project or not
    capped,
    repoRoot: root,
    startCommit,
  };
}

// Public entry point. Cached per session file + its mtime: the start commit never
// moves, so the only thing that can change the answer is the transcript growing.
async function sessionStickiness(file) {
  const key = String(file || '');
  if (/^(otel|relay|archive|codex):/.test(key)) {
    return { status: 'unknown', reason: 'Only sessions recorded by Claude Code on this computer can be checked against your code.' };
  }
  const full = resolveSessionPath(key);
  if (!full || !fs.existsSync(full)) return { status: 'unknown', reason: 'That session is not on this computer, so its work cannot be checked.' };
  let mtime = 0;
  try { mtime = fs.statSync(full).mtimeMs; } catch { /* treated as uncached */ }
  // Keyed on the transcript alone, a "gone" verdict could never clear: the owner
  // commits the work, the session file never changes again, and the red bar stays
  // forever. Time-bound the entry so the answer can catch up with the repo.
  const ck = key + '@' + mtime;
  const hit = stickCache.get(ck);
  if (hit && Date.now() - hit.at < 20000) return hit.value;
  const out = await computeStickiness(full);
  stickCache.set(ck, { value: out, at: Date.now() });
  if (stickCache.size > 60) stickCache.delete(stickCache.keys().next().value);
  return out;
}

// ---------- unsaved work (files agents made that git has never seen) ----------
// WHY: "did it stick" asks that about ONE session you already opened. This asks the
// only question that outlives every run — what did my agents make that isn't safely
// saved anywhere? A file counts only when git calls it UNTRACKED *and* no commit on
// any branch has ever mentioned it: the copy on this disk is then the only copy, and
// nothing but this list would ever tell you it exists.
// --ignored is deliberately NOT passed, so .gitignore silently removes node_modules,
// build output and logs for free — the owner already said those don't matter.
// READ-ONLY, permanently: this walks transcripts and runs `git status` / `git log`.
// It never stages, commits, moves, or writes anything, and the UI offers no way to.
// This is the most expensive read in the app (transcript parses + one git fork per
// repo, plus one per candidate), so it is capped, time-bounded, and cached.
const UNSAVED_MAX_SESSIONS = 80;   // most recent transcripts scanned, newest first
const UNSAVED_MAX_PATHS = 400;     // distinct candidate files across every repo
const UNSAVED_MAX_PER_REPO = 120;  // paths in the single `git status` argv per repo
const UNSAVED_MAX_HISTORY = 150;   // `git log` probes total — one fork each
const UNSAVED_SCAN_BUDGET = 6000;  // ms spent reading transcripts before stopping
const UNSAVED_TTL = 30000;         // this answer moves slowly; the cost does not
const unsavedCache = { at: 0, value: null };
let unsavedInflight = null;

// git() trims stdout, which eats the leading space off the FIRST porcelain record
// (' M file' arrives as 'M file') and made its status column unreadable. A code is
// always two characters then a space, so a first record whose third character is not
// a space lost exactly one leading space — put it back rather than mis-read the code.
// -z, never plain --porcelain: the unescaped form is the only one where a path
// containing a space survives, and quoted paths matched nothing.
function porcelainRecords(out) {
  const raw = String(out || '').split('\0').filter(s => s !== '');
  const recs = [];
  for (let i = 0; i < raw.length; i++) {
    let rec = raw[i];
    if (i === 0 && rec.length > 2 && rec[1] === ' ' && rec[2] !== ' ') rec = ' ' + rec;
    if (rec.length < 4 || rec[2] !== ' ') continue;
    const code = rec.slice(0, 2);
    if (code[0] === 'R' || code[0] === 'C') i++; // rename/copy emits the OLD name next
    recs.push({ code, body: rec.slice(3) });
  }
  return recs;
}

async function computeUnsaved() {
  const all = listSessions();                     // already newest-first
  const recent = all.slice(0, UNSAVED_MAX_SESSIONS);
  const cand = new Map();                         // stickKey(path) -> candidate
  const t0 = Date.now();
  let scanned = 0;

  for (const meta of recent) {
    // Reading 80 transcripts can be seconds of work on one thread. Stop on either
    // budget and SAY SO below, rather than freezing the dashboard for the owner.
    if (Date.now() - t0 > UNSAVED_SCAN_BUDGET || cand.size >= UNSAVED_MAX_PATHS) break;
    const full = resolveSessionPath(meta.file);
    if (!full || !fs.existsSync(full)) continue;
    let r;
    try { r = readSession(full); } catch { continue; } // one unreadable transcript must not sink the scan
    scanned++;
    let n = 0;
    for (const p of new Set(r.events
      .filter(e => e.kind === 'tool-call' && STICK_TOOLS.has(e.tool))
      .map(stickPathOf).filter(Boolean))) {
      if (n++ >= STICK_MAX_PATHS || cand.size >= UNSAVED_MAX_PATHS) break;
      const k = stickKey(p);
      if (cand.has(k)) continue; // sessions run newest-first, so the newest writer wins
      cand.set(k, { path: p, sessionFile: meta.file, sessionTitle: meta.title || meta.session });
    }
  }
  const capped = scanned < all.length;

  // A path that is no longer on disk is not unsaved work — it is simply gone, and
  // there is nothing here for the owner to act on.
  const alive = [];
  for (const c of cand.values()) {
    let st;
    try { st = fs.statSync(c.path); } catch { continue; }
    if (!st.isFile()) continue;
    alive.push({ ...c, sizeBytes: st.size, mtime: st.mtimeMs });
  }

  // Group by repo root so each project costs ONE `git status`, never one per file.
  // Roots are cached per folder: most of these paths are siblings.
  const rootByDir = new Map(), byRepo = new Map();
  for (const e of alive) {
    const dk = stickKey(path.dirname(e.path));
    if (!rootByDir.has(dk)) {
      const info = await gitRepoRootInfo(e.path);
      if (info.gitMissing) {
        return { files: [], gitMissing: true, scannedSessions: scanned, totalSessions: all.length, maxSessions: UNSAVED_MAX_SESSIONS, capped, candidates: alive.length, repos: 0, pathsCapped: false, historyCapped: false };
      }
      rootByDir.set(dk, info.root);
    }
    const root = rootByDir.get(dk);
    if (!root) continue;                      // outside every git project: unknowable, so never listed
    e.repoRoot = root;
    if (!byRepo.has(root)) byRepo.set(root, []);
    byRepo.get(root).push(e);
  }

  const files = [];
  let historyChecks = 0, pathsCapped = cand.size >= UNSAVED_MAX_PATHS, historyCapped = false;
  for (const [root, list] of byRepo) {
    const batch = list.slice(0, UNSAVED_MAX_PER_REPO);
    if (batch.length < list.length) pathsCapped = true;
    const st = await git(root, ['-c', 'core.quotepath=false', 'status', '--porcelain', '-z', '--', ...batch.map(e => e.path)]);
    if (!st.ok) continue;                     // can't read this project right now: say nothing rather than guess
    // Untracked FOLDERS collapse to "dir/" in this output, hence the prefix branch.
    const untrackedExact = new Set(), untrackedDirs = [];
    for (const rec of porcelainRecords(st.out)) {
      if (rec.code !== '??') continue;
      const abs = path.normalize(path.join(root, rec.body.replace(/\/$/, '')));
      if (/\/$/.test(rec.body)) untrackedDirs.push(stickKey(abs + path.sep));
      else untrackedExact.add(stickKey(abs));
    }
    const isUntracked = p => untrackedExact.has(stickKey(p)) || untrackedDirs.some(d => stickKey(p).startsWith(d));
    for (const e of batch) {
      if (!isUntracked(e.path)) continue;     // tracked: history has seen it, so it is not a ghost
      if (historyChecks >= UNSAVED_MAX_HISTORY) { historyCapped = true; break; }
      historyChecks++;
      // --all, not HEAD: work committed on a branch that isn't checked out is SAFE,
      // and calling it lost would be the worst kind of wrong answer.
      const log = await git(root, ['log', '--all', '--format=%H', '-1', '--', e.path]);
      if (!log.ok || log.out.trim()) continue;
      files.push(e);
    }
  }
  files.sort((a, b) => b.mtime - a.mtime);    // newest first: the work still fresh in mind

  return {
    files: files.map(e => ({
      path: e.path,
      repoRoot: e.repoRoot,
      sizeBytes: e.sizeBytes,
      mtime: e.mtime,
      sessionFile: e.sessionFile,
      sessionTitle: clean(e.sessionTitle, 140),
    })),
    gitMissing: false,
    scannedSessions: scanned,
    totalSessions: all.length,
    maxSessions: UNSAVED_MAX_SESSIONS,
    capped,                                   // older sessions existed but were not looked at
    candidates: alive.length,                 // files written by those sessions that still exist
    repos: byRepo.size,
    pathsCapped,                              // more written files than this scan would check
    historyCapped,                            // more untracked files than history probes allowed
  };
}

// Public entry point. Cached for 30s, and a single in-flight scan is shared: two
// clicks a second apart must not fork git twice over the same repos.
async function unsavedWork() {
  if (unsavedCache.value && Date.now() - unsavedCache.at < UNSAVED_TTL) return unsavedCache.value;
  if (unsavedInflight) return unsavedInflight;
  unsavedInflight = computeUnsaved()
    .then(v => { unsavedCache.at = Date.now(); unsavedCache.value = v; return v; })
    .finally(() => { unsavedInflight = null; });
  return unsavedInflight;
}

// ---------- trouble files (which files agents keep fighting with) ----------
// WHAT IT ANSWERS: which files do my agents keep fighting with? NOT "did this
// file ever see an error" — a boolean OR over an all-time bucket only ever goes
// up, so the files touched the most would turn red forever regardless of how
// well those sessions actually went. This is a RATE, gated on a minimum sample,
// the same shape as the Rings/Rhythm "share of runs that went badly" views: a
// file touched twice, once badly, is two data points, not a 50% problem file.
// "Went badly" is the exact definition sessionSummary() already scores the whole
// fleet by (errors > 0 || retrying || stalled) — recomputed inline per session
// here rather than calling sessionSummary(), since that would reparse a
// transcript this scan already has open.
// READ-ONLY, permanently: pure transcript parsing, same STICK_TOOLS/stickPathOf
// spine "did it stick" and "unsaved work" already use above. No git involved at
// all — nothing here needs to know what's saved, only what went wrong.
const TROUBLE_MAX_SESSIONS = 150;  // most recent transcripts scanned, newest first
const TROUBLE_MIN_SESSIONS = 5;    // gate: a file needs at least this many touches before its rate is shown
const TROUBLE_MAX_ROWS = 20;       // rows returned to the UI
const TROUBLE_SCAN_BUDGET = 6000;  // ms spent reading transcripts before stopping
const TROUBLE_TTL = 30000;         // this answer moves slowly; the scan cost does not
const troubleCache = { at: 0, value: null };

function computeTroubleFiles() {
  const all = listSessions();                     // already newest-first
  const recent = all.slice(0, TROUBLE_MAX_SESSIONS);
  const t0 = Date.now();
  let scanned = 0;
  const byPath = new Map();                        // stickKey(path) -> { path, touches: [...] }

  for (const meta of recent) {
    // Reading 150 transcripts is real work on one thread. Stop on budget and SAY
    // SO below, rather than freezing the dashboard for the owner.
    if (Date.now() - t0 > TROUBLE_SCAN_BUDGET) break;
    const full = resolveSessionPath(meta.file);
    if (!full || !fs.existsSync(full)) continue;
    let r;
    try { r = readSession(full); } catch { continue; } // one unreadable transcript must not sink the scan
    scanned++;
    const touched = new Set(r.events
      .filter(e => e.kind === 'tool-call' && STICK_TOOLS.has(e.tool))
      .map(stickPathOf).filter(Boolean));
    if (!touched.size) continue; // a session that wrote nothing has no file to blame or clear
    const bad = r.events.some(e => e.error)
      || r.agents.some(a => a.retrying)
      || (r.agents.some(a => a.pendingTool && a.pendingTool.since && Date.now() - new Date(a.pendingTool.since) > 120000) && Date.now() - meta.mtime < 600000);
    for (const p of touched) {
      const k = stickKey(p);
      if (!byPath.has(k)) byPath.set(k, { path: p, touches: [] });
      byPath.get(k).touches.push({ file: meta.file, title: clean(meta.title || meta.session, 140), mtime: meta.mtime, bad });
    }
  }
  const capped = scanned < all.length;

  const files = [...byPath.values()].map(f => {
    const sessions = f.touches.length;
    const badSessions = f.touches.filter(t => t.bad).length;
    // THE GATE: below TROUBLE_MIN_SESSIONS, no rate is computed at all — the row
    // is shown by volume only, with the UI expected to say "not enough runs to
    // say yet" rather than print a percentage nobody should trust.
    const gated = sessions >= TROUBLE_MIN_SESSIONS;
    f.touches.sort((a, b) => b.mtime - a.mtime); // newest first, for the drill-down
    return {
      path: f.path,
      sessions,
      badSessions,
      rate: gated ? Math.round(badSessions / sessions * 100) : null,
      lastTouched: f.touches[0].mtime,
      sessionRefs: f.touches,
    };
  })
    // Ranked by volume, never by rate over the whole population — sorting by
    // rate first is exactly the trap this feature exists to avoid: it would
    // float a 1-of-1 failure above a file with 40 clean runs and one flub.
    .sort((a, b) => b.sessions - a.sessions || b.lastTouched - a.lastTouched)
    .slice(0, TROUBLE_MAX_ROWS);

  return {
    files,
    scannedSessions: scanned,
    totalSessions: all.length,
    maxSessions: TROUBLE_MAX_SESSIONS,
    minSessions: TROUBLE_MIN_SESSIONS,
    capped,             // older sessions existed but were not looked at
  };
}

// Public entry point. Cached 30s: this walks up to TROUBLE_MAX_SESSIONS
// transcripts on every call, so repeated clicks must not repeat that cost.
function troubleFiles() {
  if (troubleCache.value && Date.now() - troubleCache.at < TROUBLE_TTL) return troubleCache.value;
  const out = computeTroubleFiles();
  troubleCache.at = Date.now();
  troubleCache.value = out;
  return out;
}

// ---------- recent tool calls (the evidence a "blast radius" preview judges) ----------
// WHAT IT ANSWERS: if I add this permission rule, what would it actually have hit?
// WHY IT IS A SEPARATE READ instead of reusing the parsed events every other view
// runs on: summarizeInput() deliberately prefers a call's human `description` over
// its `command`, so the text a session view shows for a Bash call is a sentence
// like "List files in current directory" — NOT the string Claude Code's permission
// matcher looks at. Matching a rule against that would be quietly, invisibly wrong
// for exactly the two tools that matter most here (Bash and WebFetch). So this
// re-reads the raw tool_use blocks and returns the argument THE RULE WOULD SEE,
// tagged with what kind of argument it is, so the UI can say "couldn't check this
// one" instead of scoring a rule against the wrong string.
// READ-ONLY, permanently: transcripts in, JSON out. No git, no writes, and never a
// path from the browser — the scan always walks the newest sessions itself.
const CALLS_MAX = 200;            // tool calls returned: the sample the preview judges
const CALLS_MIN_SESSIONS = 10;    // keep reading past CALLS_MAX so the sample spans sessions, not one busy night
const CALLS_MAX_SESSIONS = 30;    // hard ceiling on transcripts opened in one pass
const CALLS_SCAN_BUDGET = 5000;   // ms spent reading before stopping (and saying so)
const CALLS_ARG_MAX = 300;        // per-call argument kept; longer than this is clipped and flagged
const CALLS_TTL = 30000;
const callsCache = { at: 0, value: null };

// The argument a permission rule is matched against, and what kind it is. Order
// matters: `path` is tested before `pattern` so a Grep rule scores against the
// folder it searched, not the regex it searched for. Anything not on this list
// comes back kind 'none' — the UI then refuses to judge that call rather than
// matching a rule against whatever string happened to be there.
const CALL_ARG_KEYS = [
  ['command', 'command'],        // Bash, PowerShell — what Bash(...) matches
  ['file_path', 'path'],         // Read, Write, Edit, MultiEdit
  ['notebook_path', 'path'],     // NotebookEdit
  ['path', 'path'],              // Glob, Grep
  ['url', 'url'],                // WebFetch and friends
  ['query', 'query'],            // WebSearch
  ['pattern', 'query'],          // a search expression, never a path
];
function permArgOf(input) {
  if (!input || typeof input !== 'object') return { arg: '', argKind: 'none' };
  for (const [k, kind] of CALL_ARG_KEYS) {
    if (typeof input[k] === 'string' && input[k].trim()) return { arg: input[k].trim(), argKind: kind };
  }
  return { arg: '', argKind: 'none' };
}

// Pull every tool_use block out of one transcript's lines. The indexOf prefilter
// skips the ~90% of lines that cannot contain one without paying for a JSON parse.
function collectToolCalls(lines, out) {
  for (const line of lines) {
    if (!line || line.indexOf('"tool_use"') < 0) continue;
    const o = safeParse(line);
    if (!o || o.type !== 'assistant') continue;
    const c = o.message && o.message.content;
    if (!Array.isArray(c)) continue;
    for (const b of c) {
      if (!b || b.type !== 'tool_use' || typeof b.name !== 'string') continue;
      const { arg, argKind } = permArgOf(b.input);
      const t = o.timestamp ? new Date(o.timestamp).getTime() : NaN;
      out.push({
        tool: clean(b.name, 80),
        // clean() drops control characters, so fold newlines to spaces FIRST or a
        // multi-line command comes back with its lines glued together
        arg: clean(arg.replace(/\s+/g, ' '), CALLS_ARG_MAX),
        argKind,
        clipped: arg.length > CALLS_ARG_MAX, // a clipped command can still be prefix-matched, never exact-matched
        ts: isNaN(t) ? null : t,
      });
    }
  }
}

function computeRecentToolCalls() {
  const all = listSessions();                    // already newest-first
  const t0 = Date.now();
  let scanned = 0, budgetHit = false;
  const calls = [];
  for (const meta of all.slice(0, CALLS_MAX_SESSIONS)) {
    if (calls.length >= CALLS_MAX && scanned >= CALLS_MIN_SESSIONS) break;
    if (Date.now() - t0 > CALLS_SCAN_BUDGET) { budgetHit = true; break; }
    const full = resolveSessionPath(meta.file);
    if (!full) continue;
    let text;
    try { text = fs.readFileSync(full, 'utf8'); } catch { continue; } // one unreadable transcript must not sink the scan
    scanned++;
    const mine = [];
    collectToolCalls(text.split('\n'), mine);
    // subagents' calls go through the same permission rules the orchestrator's do,
    // so leaving them out would under-report every rule's reach
    for (const sf of subagentFiles(full)) {
      try { collectToolCalls(fs.readFileSync(sf.path, 'utf8').split('\n'), mine); } catch { /* skip */ }
    }
    const title = clean(meta.title || meta.session, 140);
    for (const c of mine) { c.file = meta.file; c.title = title; if (c.ts == null) c.ts = meta.mtime; }
    calls.push(...mine);
  }
  calls.sort((a, b) => b.ts - a.ts);
  const kept = calls.slice(0, CALLS_MAX);
  return {
    calls: kept,
    scannedSessions: scanned,
    totalSessions: all.length,
    sessionsInSample: new Set(kept.map(c => c.file)).size,
    capped: scanned < all.length,
    budgetHit,
    max: CALLS_MAX,
    oldest: kept.length ? kept[kept.length - 1].ts : null,
    newest: kept.length ? kept[0].ts : null,
  };
}

// Public entry point. Cached 30s — the same reason troubleFiles() is: this opens
// real transcripts, and clicking around a preview must not repeat that cost.
function recentToolCalls() {
  if (callsCache.value && Date.now() - callsCache.at < CALLS_TTL) return callsCache.value;
  const out = computeRecentToolCalls();
  callsCache.at = Date.now();
  callsCache.value = out;
  return out;
}

// ---------- the graveyard (work this fleet has already thrown away) ----------
// WHAT IT ANSWERS: what has been tried in this folder and undone? An agent that
// cannot see last week's dead end walks cheerfully back into it.
// WHY IT IS THIS NARROW: the first sketch tried to notice "the run started over
// and did something different", and that was cut on purpose — every long session
// changes approach, so it would fire constantly and mean nothing. This recognises
// FOUR literal git commands and nothing else. Every row is a command an agent
// really ran, at a moment you can click straight to and read.
// WHY IT NEEDS ITS OWN RAW SCAN: summarizeInput() prefers a call's human
// `description` over its `command` (same reason the blast-radius preview above
// re-reads raw blocks), so the text the rest of the dashboard holds for a Bash
// call is a sentence like "Discard local changes" — the command string is nowhere
// in the parsed session. So this re-reads the raw tool_use blocks and joins them
// back to the parsed events on the tool_use id, which is what recovers the seq
// the UI links to.
// COVERAGE IS SMALLER THAN THE FLEET, AND THE PANEL SAYS SO: relayed sessions
// arrive already parsed, so their command text does not exist on this computer at
// all. Only transcripts stored here can be read — the local ones, plus whatever
// has been pulled into the archive from a relay.
// NO RATES, DELIBERATELY: every row is one thing that happened once, with a link
// to it. There is no "this approach fails N% of the time" here and there must
// never be one — the sample is only whatever transcripts happen to be kept.
// READ-ONLY, permanently: transcripts in, JSON out. No git runs at all, nothing
// written, and never a path the browser sent — the scan enumerates files itself.
const GRAVE_MAX_SESSIONS = 40;                 // newest transcripts opened, newest first
const GRAVE_MAX_BYTES = 400 * 1024 * 1024;     // bytes read in one pass before stopping (and saying so)
const GRAVE_SCAN_BUDGET = 9000;                // ms spent reading before stopping (and saying so)
const GRAVE_MAX_MOMENTS = 60;                  // rows returned to the UI
const GRAVE_MAX_FILES_PER = 8;                 // files named per moment
const GRAVE_CMD_MAX = 220;                     // characters of the command kept per row
const GRAVE_TTL = 60000;                       // this answer moves slowly; the scan cost does not
const graveCache = { at: 0, value: null };

// The whole detector. Four literal commands, each unambiguous about what it throws
// away. `git restore --staged` on its own only unstages — the edits survive — so it
// is deliberately NOT on this list. This array is also what the panel prints as its
// own help text, so the promise and the detector cannot drift apart.
const GRAVE_RULES = {
  reset: { id: 'reset', cmd: 'git reset --hard', what: 'threw the whole folder back to a saved point' },
  revert: { id: 'revert', cmd: 'git revert', what: 'undid a change that had already been saved' },
  checkout: { id: 'checkout', cmd: 'git checkout -- <files>', what: 'threw away the edits to named files' },
  restore: { id: 'restore', cmd: 'git restore <files>', what: 'threw away the edits to named files' },
};
// Cheap reject before any JSON parsing: a transcript without one of these strings
// anywhere in it cannot contain one of the four commands.
const GRAVE_NEEDLES = ['reset --hard', 'git revert', 'git checkout', 'git restore'];

// git's global options come BEFORE the subcommand (`git -C <dir> reset --hard`),
// so they are stripped first or the subcommand never lines up.
const GRAVE_GIT_HEAD = /^git\s+(?:(?:-C|-c|--git-dir|--work-tree|--exec-path)(?:=\S+|\s+\S+)\s+|--no-pager\s+|--literal-pathspecs\s+)*([a-z][a-z-]*)([\s\S]*)$/;
// Split an argument string into tokens, honouring simple quoting so a path with a
// space in it stays one path.
function graveTokens(s) {
  const out = [];
  const re = /"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+)/g;
  let m;
  while ((m = re.exec(s))) out.push(m[1] != null ? m[1] : m[2] != null ? m[2] : m[3]);
  return out;
}
// Paths named after a `--` separator, which is git's own "everything past here is a
// file" marker — the one place a pathspec is not guessable from a flag.
function gravePathsAfterSep(rest) {
  const i = rest.search(/(^|\s)--(\s|$)/);
  if (i < 0) return [];
  return graveTokens(rest.slice(i).replace(/^\s*--\s*/, '')).filter(t => t[0] !== '-');
}
// One shell segment -> the undo it performs, or null. Segments are matched from
// their FIRST word, so `grep "git reset --hard" .` (a search for the string) and
// `echo "git revert"` are not mistaken for the command itself.
function graveUndoOf(seg) {
  const m = GRAVE_GIT_HEAD.exec(seg);
  if (!m) return null;
  const sub = m[1], rest = m[2] || '';
  if (sub === 'reset' && /(^|\s)--hard(\s|$)/.test(rest)) return { rule: GRAVE_RULES.reset, paths: [] };
  // --abort/--continue/--quit/--skip are the RECOVERY forms: they get you out of a
  // revert, they do not undo saved work. Reporting them as thrown-away work (and
  // proposing a hook that blocks the escape route) is exactly backwards.
  if (sub === 'revert') {
    if (/(^|\s)--(abort|continue|quit|skip)(\s|$)/.test(rest)) return null;
    return { rule: GRAVE_RULES.revert, paths: [] };
  }
  if (sub === 'checkout' && /(^|\s)--(\s|$)/.test(rest)) return { rule: GRAVE_RULES.checkout, paths: gravePathsAfterSep(rest) };
  if (sub === 'restore' && !/(^|\s)--staged(\s|$)/.test(rest)) {
    const sep = gravePathsAfterSep(rest);
    if (sep.length) return { rule: GRAVE_RULES.restore, paths: sep };
    // no separator: take the bare words, dropping flags and the value of --source
    const toks = graveTokens(rest);
    const paths = [];
    for (let i = 0; i < toks.length; i++) {
      if (toks[i] === '--source' || toks[i] === '-s') { i++; continue; }
      if (toks[i][0] !== '-') paths.push(toks[i]);
    }
    return { rule: GRAVE_RULES.restore, paths };
  }
  return null;
}
// Split a command into shell segments, IGNORING separators inside quotes. This is
// the guard that stopped the first build's only false positives: a line like
// `grep 'reset --hard\|git revert' .` splits on those bare pipes into a fragment
// beginning "git revert", and a naive splitter read a search FOR the command as the
// command. Bare `|` is not a separator here either — nothing pipes into a git undo.
// An unbalanced quote makes the rest of the line look quoted and can hide a real
// undo; that is the right way to be wrong for this feature, the same trade the leak
// scanner makes. Missing one beats inventing one.
function graveSegments(cmd) {
  // A heredoc body is FILE CONTENT, not commands. A doc or script written with
  // `cat <<EOF ... git revert ... EOF` was being read as an agent really running
  // git revert — a fabricated entry in a list whose whole value is being accurate.
  // Everything from the introducer to its terminator is dropped.
  const hd = /<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?/.exec(cmd);
  if (hd) {
    const end = new RegExp('^\\s*' + hd[1] + '\\s*$', 'm');
    const after = cmd.slice(hd.index);
    const m = end.exec(after);
    cmd = cmd.slice(0, hd.index) + (m ? after.slice(m.index + m[0].length) : '');
  }
  const out = [];
  let cur = '', q = null;
  for (let i = 0; i < cmd.length; i++) {
    const c = cmd[i];
    if (q) {
      if (c === '\\' && q === '"') { cur += c + (cmd[++i] || ''); continue; }
      if (c === q) q = null;
      cur += c; continue;
    }
    if (c === '"' || c === '\'') { q = c; cur += c; continue; }
    if (c === '\n' || c === ';') { out.push(cur); cur = ''; continue; }
    if ((c === '&' && cmd[i + 1] === '&') || (c === '|' && cmd[i + 1] === '|')) { out.push(cur); cur = ''; i++; continue; }
    cur += c;
  }
  out.push(cur);
  return out;
}
// `cd <dir> && git checkout -- x` names its own folder, and that folder is the
// repo the pathspecs are relative to. Prefer it over the session's recorded cwd.
function graveCdTarget(segments) {
  const first = (segments[0] || '').trim();
  const m = /^cd\s+(?:\/d\s+)?(.+)$/i.exec(first);
  if (!m) return null;
  const t = graveTokens(m[1])[0];
  return t && path.isAbsolute(t) ? path.normalize(t) : null;
}

// Scan ONE transcript for undo commands. Returns the bytes it read so the caller
// can hold a budget; pushes into `hits` rather than returning them, because a
// session's subagent files all feed the same list.
// The byte budget is only consulted BETWEEN files, so one enormous transcript could
// block the single-threaded hub on its own. Skip anything past a per-file cap —
// the caller already reports when a scan was incomplete.
const GRAVE_MAX_FILE = 48 * 1024 * 1024;
function graveScanFile(abs, hits) {
  let buf;
  try {
    if (fs.statSync(abs).size > GRAVE_MAX_FILE) return 0;
    buf = fs.readFileSync(abs);
  } catch { return 0; }
  if (!GRAVE_NEEDLES.some(n => buf.includes(n))) return buf.length;
  for (const line of buf.toString('utf8').split('\n')) {
    if (line.indexOf('"tool_use"') < 0) continue;
    const o = safeParse(line);
    if (!o || o.type !== 'assistant') continue;
    const content = o.message && o.message.content;
    if (!Array.isArray(content)) continue;
    for (const b of content) {
      if (!b || b.type !== 'tool_use') continue;
      const cmd = b.input && b.input.command;
      if (typeof cmd !== 'string' || !cmd.trim()) continue;
      const segs = graveSegments(cmd);
      for (const seg of segs) {
        const trimmed = seg.trim();
        const u = graveUndoOf(trimmed);
        if (!u) continue;
        const t = o.timestamp ? new Date(o.timestamp).getTime() : NaN;
        hits.push({
          id: b.id || null, tool: clean(b.name, 40), rule: u.rule, paths: u.paths.slice(0, GRAVE_MAX_FILES_PER),
          base: graveCdTarget(segs),
          // the MATCHING segment is what gets shown: a three-line command whose
          // undo is on line two must not be printed as its harmless first line
          command: clean(trimmed.replace(/\s+/g, ' '), GRAVE_CMD_MAX),
          whole: cmd.length > trimmed.length ? clean(cmd.replace(/\s+/g, ' '), GRAVE_CMD_MAX * 2) : null,
          ts: isNaN(t) ? null : t,
        });
        break; // one moment per command, even when it chains two undos together
      }
    }
  }
  return buf.length;
}

// Every transcript on this computer whose raw command text can still be read:
// the local ones, plus main transcripts pulled into the archive from a relay.
// `orphans` counts archived sessions whose subagent transcripts arrived but whose
// main transcript did not. Their commands are readable, but there is no session to
// open at that moment, so they are left out — and counted, so the panel can admit it.
function graveCandidates() {
  const out = [];
  let orphans = 0;
  for (const m of listSessions()) {
    const full = resolveSessionPath(m.file);
    if (full) out.push({ file: m.file, path: full, title: m.title || m.session, mtime: m.mtime, machine: os.hostname(), source: 'this computer' });
  }
  let machines = [];
  try { machines = fs.readdirSync(ARCHIVE_DIR()); } catch { /* no archive pulled yet */ }
  for (const machine of machines) {
    const base = archiveMachineDir(machine);
    let man = {};
    try { if (!fs.statSync(base).isDirectory()) continue; man = archiveManifest(machine); } catch { continue; }
    const rels = Object.keys(man);
    const mains = new Set(rels.filter(r => /^claude\/[^/]+\/[^/]+\.jsonl$/.test(r)));
    const seenTrees = new Set();
    for (const rel of rels) {
      const sub = /^(claude\/[^/]+\/[^/]+)\/subagents\//.exec(rel);
      if (sub && !seenTrees.has(sub[1])) { seenTrees.add(sub[1]); if (!mains.has(sub[1] + '.jsonl')) orphans++; }
    }
    for (const rel of mains) {
      const abs = path.join(base, rel);
      let mtime = 0;
      try { mtime = fs.statSync(abs).mtimeMs; } catch { continue; }
      out.push({ file: 'archive:' + machine + ':' + rel, path: abs, title: null, mtime, machine, source: machine });
    }
  }
  out.sort((a, b) => b.mtime - a.mtime);
  out.orphans = orphans;
  return out;
}
// Title for an archived transcript, read only once a session has actually produced
// a row — the same head-read /api/archive/list does, not paid for on every file.
function graveTitleOf(abs) {
  try {
    const size = fs.statSync(abs).size;
    const fd = fs.openSync(abs, 'r');
    const buf = Buffer.alloc(Math.min(8192, size));
    fs.readSync(fd, buf, 0, buf.length, 0); fs.closeSync(fd);
    for (const line of buf.toString('utf8').split('\n')) {
      const o = safeParse(line);
      if (o && (o.type === 'custom-title' || o.type === 'ai-title')) return o.customTitle || o.aiTitle;
    }
  } catch { /* fall back to the file name */ }
  return null;
}

function computeGraveyard() {
  const cands = graveCandidates();
  const t0 = Date.now();
  let scanned = 0, bytes = 0, budgetHit = false;
  const moments = [];
  const overBudget = () => Date.now() - t0 > GRAVE_SCAN_BUDGET || bytes > GRAVE_MAX_BYTES;

  for (const c of cands.slice(0, GRAVE_MAX_SESSIONS)) {
    if (overBudget()) { budgetHit = true; break; }
    const hits = [];
    bytes += graveScanFile(c.path, hits);
    // a subagent's commands are this session's commands: leaving them out would
    // miss most of the undos, since most of the work happens down there
    for (const sf of subagentFiles(c.path)) {
      if (overBudget()) { budgetHit = true; break; }
      bytes += graveScanFile(sf.path, hits);
    }
    scanned++;
    if (!hits.length) continue;

    // Re-parse this ONE session (cached) so each raw hit can be joined back to its
    // parsed event by tool_use id — that is what gives the UI a seq to jump to.
    let r = null;
    try { r = readSession(c.path); } catch { /* the moment still stands; only its link is lost */ }
    // A call and its result carry the SAME tool_use id, and the result comes
    // second — indexing both put every link one event past the command, on the
    // answer rather than the question. Only the call side is indexed.
    const byId = new Map();
    if (r) for (const e of r.events) if (e.toolUseId && (e.kind === 'tool-call' || e.kind === 'spawn') && !byId.has(e.toolUseId)) byId.set(e.toolUseId, e);
    const edits = r ? r.events.filter(e => e.kind === 'tool-call' && STICK_TOOLS.has(e.tool)) : [];
    const cwd = sessionCwd(c.path) || '';
    const title = c.title || graveTitleOf(c.path) || path.basename(c.path, '.jsonl');

    for (const h of hits) {
      const evt = h.id ? byId.get(h.id) : null;
      const seq = evt && evt.seq != null ? evt.seq : null;
      const repo = h.base || cwd || '';
      // Two very different kinds of "the files involved", never blurred together:
      // the command NAMED them (checkout/restore), or it did not (reset/revert) and
      // the best that can honestly be said is what this run had edited by then.
      let files = h.paths.map(p => (path.isAbsolute(p) ? path.normalize(p) : repo ? path.normalize(path.join(repo, p)) : p));
      const named = files.length > 0;
      let filesCapped = false;
      if (!named) {
        const before = seq == null ? edits : edits.filter(e => e.seq != null && e.seq < seq);
        const all = [...new Set(before.map(stickPathOf).filter(Boolean))];
        filesCapped = all.length > GRAVE_MAX_FILES_PER;
        files = all.slice(-GRAVE_MAX_FILES_PER);
      }
      moments.push({
        file: c.file, title: clean(title, 140), machine: c.machine, source: c.source,
        seq, ts: h.ts || c.mtime, kind: h.rule.id, what: h.rule.what, cmd: h.rule.cmd,
        command: h.command, whole: h.whole, tool: h.tool,
        repo, files, filesNamed: named, filesCapped,
        agent: evt ? evt.agent : null,
      });
    }
  }

  moments.sort((a, b) => b.ts - a.ts);
  const kept = moments.slice(0, GRAVE_MAX_MOMENTS);
  // Sessions that exist in the picker but whose commands are simply not here to
  // read: relayed and OTLP ones arrive already summarised, and Codex transcripts
  // record their calls in a different shape this scan does not parse.
  const summaryOnly = relayList().length + otelList().length + codexList().length;
  return {
    moments: kept,
    repos: new Set(kept.map(m => stickKey(m.repo || ''))).size,
    totalMoments: moments.length,
    scannedSessions: scanned,
    candidates: cands.length,
    capped: cands.length > scanned,
    budgetHit,
    mbRead: Math.round(bytes / 1e6),
    summaryOnly,
    orphans: cands.orphans || 0,
    looksFor: Object.values(GRAVE_RULES).map(r => ({ cmd: r.cmd, what: r.what })),
  };
}

// Public entry point. Cached 60s: one pass can read hundreds of megabytes of
// transcript, so clicking back onto the tab must not repeat that.
function graveyard() {
  if (graveCache.value && Date.now() - graveCache.at < GRAVE_TTL) return graveCache.value;
  const out = computeGraveyard();
  graveCache.at = Date.now();
  graveCache.value = out;
  return out;
}

// ---------- secret leak sentinel (did an agent write a real credential into a file?) ----------
// WHAT IT ANSWERS: did one of my agents drop something that looks like a REAL
// credential into a file? This is a smoke alarm, not a security audit.
// WHY IT IS THIS NARROW: an earlier version of this design scored strings by
// entropy and was deliberately cut, because entropy flags every hash, minified
// bundle, UUID and base64 blob on the disk. A scanner that cries wolf gets
// ignored, which is worse than not having one, so this recognises ONLY a short
// list of credential shapes whose PREFIX is unmistakable — the vendor stamps it
// on the key itself. There is no "high entropy string" rule and no "password="
// rule, on purpose. It will miss real secrets, and that trade is the point:
// prefer missing a real one over inventing a fake one.
// SCOPE: only files the agents themselves wrote, per the transcript record — the
// same STICK_TOOLS/stickPathOf spine "did it stick", "unsaved work" and "trouble
// files" already use. Never the whole disk, never a path the browser sent.
// READ-ONLY, permanently: it reads those files and runs one `git check-ignore`
// per project. Nothing is written, and nothing is ever changed.
// THE SECRET ITSELF NEVER LEAVES THIS FUNCTION: the response carries the file,
// the line number, what KIND of key it looks like, and the first four characters.
// Nothing is logged, cached to disk, or sent anywhere.
const LEAK_MAX_SESSIONS = 60;               // most recent transcripts scanned, newest first
const LEAK_MAX_FILES = 150;                 // files actually opened and read
const LEAK_MAX_BYTES = 512 * 1024;          // per file: larger than this is a bundle or a blob, not hand-written config
const LEAK_TOTAL_BYTES = 16 * 1024 * 1024;  // whole scan, so this can never eat the disk
const LEAK_MAX_PER_FILE = 5;                // one bad file must not fill the whole panel
const LEAK_MAX_FINDINGS = 60;
const LEAK_MAX_PER_REPO = 120;              // paths in one `git check-ignore` argv
const LEAK_SCAN_BUDGET = 6000;              // ms spent reading transcripts before stopping
const LEAK_TTL = 60000;                     // this answer moves slowly; the scan cost does not
const leakCache = { at: 0, value: null };
let leakInflight = null;

// A JWT-shaped triple is only a token if its first part really is a base64url
// JSON header naming an algorithm. Anything else that happens to have two dots
// in it — a version string, a filename, a hash — fails here and is dropped.
function jwtHeaderIsReal(m) {
  try {
    const head = m.split('.')[0];
    const json = Buffer.from(head.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8');
    if (json[0] !== '{') return false;
    const o = JSON.parse(json);
    // alg:"none" is the unsigned form — a demo token, never a live credential
    return !!o && typeof o === 'object' && typeof o.alg === 'string' && o.alg.toLowerCase() !== 'none';
  } catch { return false; }
}

// The whole detector. Every rule is a fixed vendor prefix plus an exact length —
// nothing here is a guess, and nothing here is a heuristic. `kind` is the sentence
// the owner reads, so it names the service, not the token format.
const LEAK_RULES = [
  { id: 'aws', kind: 'An Amazon Web Services key', re: /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/g },
  { id: 'github', kind: 'A GitHub token', re: /\bgh[pousr]_[A-Za-z0-9]{36}\b/g },
  { id: 'slack', kind: 'A Slack token', re: /\bxox[baprs]-[A-Za-z0-9]{10,48}-[A-Za-z0-9]{10,48}(?:-[A-Za-z0-9]{10,64})?\b/g },
  { id: 'stripe', kind: 'A Stripe live payment key', re: /\b[sr]k_live_[A-Za-z0-9]{24,64}\b/g },
  { id: 'google', kind: 'A Google API key', re: /\bAIza[A-Za-z0-9_-]{35}\b/g },
  // The header ALONE is not a key — parsers, validators, docs and error strings all
  // contain that literal. Require actual key material after it: at least one line of
  // 40+ base64 characters before the END marker. Mentions stop matching; real keys don't.
  { id: 'pem', kind: 'A private key', re: /-----BEGIN (?:RSA |DSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----[\r\n]+(?:[A-Za-z0-9+/=]{40,}[\r\n]+)+/g },
  { id: 'jwt', kind: 'A signed login token', re: /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b/g, confirm: jwtHeaderIsReal },
];

// Documentation, READMEs and .env.example files are full of correctly-shaped fake
// keys, and flagging those is exactly how this feature would lose the owner's
// trust. Tested against the match AND the text immediately around it, because the
// giveaway is usually the sentence, not the key ("replace this with your own").
const LEAK_PLACEHOLDER = /EXAMPLE|SAMPLE|PLACEHOLDER|REPLACE|CHANGE[-_ ]?ME|INSERT[-_ ]?YOUR|DUMMY|REDACT|NOT[-_ ]?REAL|FAKE|YOUR[-_]|<|abcdef|1234567890/i;
// A random key essentially never repeats one character six times; XXXXXX, 000000
// and aaaaaa are all somebody typing a blank to be filled in later.
const leakIsPlaceholder = s => LEAK_PLACEHOLDER.test(s) || /(.)\1{5,}/.test(s);

// Folders whose contents nobody wrote by hand: dependencies, build output, git's
// own storage, and test fixtures — which exist precisely to hold realistic fakes.
// Tests, docs and examples belong here too: they are full of realistic-looking
// fakes on purpose, and flagging them is how a scanner teaches you to ignore it.
const LEAK_NOISE_DIRS = new Set(['node_modules', 'dist', 'build', '.git', 'coverage', 'vendor', '.next', '.nuxt', '.venv', 'site-packages', 'fixtures', '__fixtures__', 'testdata', 'test-fixtures', '__snapshots__',
  'test', 'tests', '__tests__', 'spec', '__mocks__', 'docs', 'doc', 'examples', 'example', 'samples']);
// Generated files: lockfiles, minified bundles and source maps are megabytes of
// machine output where any match is noise by definition.
// ...and by FILENAME: .env.example / .env.sample / anything.template exists to hold
// a shaped placeholder, and *.test.* / *.spec.* are tests wherever they happen to sit.
const LEAK_NOISE_FILES = /^(?:package-lock\.json|yarn\.lock|pnpm-lock\.yaml|composer\.lock|Cargo\.lock)$|\.min\.[a-z0-9]+$|\.map$|\.(?:example|sample|template|dist)$|^\.env\.(?:example|sample|template)$|\.(?:test|spec)\.[a-z0-9]+$/i;
function leakIsNoisePath(p) {
  const segs = p.split(/[\\/]/);
  const name = segs[segs.length - 1] || '';
  if (LEAK_NOISE_FILES.test(name)) return true;
  return segs.slice(0, -1).some(s => LEAK_NOISE_DIRS.has(s));
}

// Scan ONE file. Returns null when it could not be read, and never returns any of
// the file's text — only a line number, a kind, and four characters.
function leakScanFile(p) {
  let buf;
  try { buf = fs.readFileSync(p); } catch { return null; }
  if (buf.includes(0)) return { binary: true, hits: [] }; // a NUL byte means this is not source or config
  const text = buf.toString('utf8');
  const hits = [], seen = new Set();
  for (const rule of LEAK_RULES) {
    rule.re.lastIndex = 0; // these regexes are /g and module-level: reset before every file
    let m;
    while (hits.length < LEAK_MAX_PER_FILE && (m = rule.re.exec(text)) !== null) {
      const match = m[0];
      // A window around the match, used ONLY for the placeholder test below. It is
      // never returned, never logged, and goes out of scope with this iteration.
      const ls = text.lastIndexOf('\n', m.index) + 1;
      let le = text.indexOf('\n', m.index);
      if (le < 0) le = text.length;
      const around = text.slice(Math.max(ls, m.index - 120), Math.min(le, m.index + match.length + 120));
      if (leakIsPlaceholder(match) || leakIsPlaceholder(around)) continue;
      if (rule.confirm && !rule.confirm(match)) continue;
      const line = text.slice(0, m.index).split('\n').length;
      const key = rule.id + '@' + line;
      if (seen.has(key)) continue;
      seen.add(key);
      hits.push({ kind: rule.kind, line, fragment: match.slice(0, 4) + '…' });
    }
  }
  return { binary: false, hits };
}

async function computeLeaks() {
  const all = listSessions();                     // already newest-first
  const recent = all.slice(0, LEAK_MAX_SESSIONS);
  const cand = new Map();                         // stickKey(path) -> candidate
  const t0 = Date.now();
  let scanned = 0, skippedNoise = 0;

  for (const meta of recent) {
    // Reading 60 transcripts is real work on one thread. Stop on either budget and
    // SAY SO in the response, rather than freezing the dashboard for the owner.
    if (Date.now() - t0 > LEAK_SCAN_BUDGET || cand.size >= LEAK_MAX_FILES) break;
    const full = resolveSessionPath(meta.file);
    if (!full || !fs.existsSync(full)) continue;
    let r;
    try { r = readSession(full); } catch { continue; } // one unreadable transcript must not sink the scan
    scanned++;
    let n = 0;
    for (const p of new Set(r.events
      .filter(e => e.kind === 'tool-call' && STICK_TOOLS.has(e.tool))
      .map(stickPathOf).filter(Boolean))) {
      if (n++ >= STICK_MAX_PATHS || cand.size >= LEAK_MAX_FILES) break;
      const k = stickKey(p);
      if (cand.has(k)) continue;                  // sessions run newest-first, so the newest writer wins
      if (leakIsNoisePath(p)) { skippedNoise++; continue; }
      cand.set(k, { path: p, sessionFile: meta.file, sessionTitle: meta.title || meta.session });
    }
  }
  const capped = scanned < all.length;
  const filesCapped = cand.size >= LEAK_MAX_FILES;

  // Files that are gone, or too big to be hand-written config, never get opened.
  const alive = [];
  let skippedBig = 0;
  for (const c of cand.values()) {
    let st;
    try { st = fs.statSync(c.path); } catch { continue; }
    if (!st.isFile()) continue;
    if (st.size > LEAK_MAX_BYTES) { skippedBig++; continue; }
    alive.push({ ...c, mtime: st.mtimeMs, sizeBytes: st.size });
  }

  // Group by project so `git check-ignore` costs ONE call per repo, never one per
  // file. Roots are cached per folder: most of these paths are siblings.
  const rootByDir = new Map(), byRepo = new Map(), loose = [];
  for (const e of alive) {
    const dk = stickKey(path.dirname(e.path));
    if (!rootByDir.has(dk)) rootByDir.set(dk, (await gitRepoRootInfo(e.path)).root);
    const root = rootByDir.get(dk);
    e.repoRoot = root || null;
    if (!root) { loose.push(e); continue; }       // outside every project: nothing to ask git about
    if (!byRepo.has(root)) byRepo.set(root, []);
    byRepo.get(root).push(e);
  }

  // A file git was told to ignore is one the owner already decided not to share,
  // so a key in it is not a leak into the codebase. check-ignore exits 1 when
  // NOTHING matched, which git() reports as a failure — that is the normal case
  // and simply means nothing here is suppressed.
  const toScan = loose.slice();
  let skippedIgnored = 0, skippedUnchecked = 0;
  for (const [root, list] of byRepo) {
    const batch = list.slice(0, LEAK_MAX_PER_REPO);
    // Anything past the cap does not get scanned AT ALL. Scanning a file we could
    // not ask git about risks flagging a local .env the owner deliberately keeps
    // out of the repo — which is the exact noise this feature exists to avoid — so
    // it is dropped and counted, and the panel says how many.
    skippedUnchecked += list.length - batch.length;
    // `check-ignore -z` is a FATAL error unless it is paired with --stdin: git
    // rejects the combination outright, so this call always failed and the whole
    // gitignore suppression silently did nothing — every ignored file an agent
    // wrote got scanned and flagged, which is precisely the noise this exists to
    // stop. Paths go in on stdin (NUL-separated), results come back the same way.
    const ig = await gitStdin(root, ['-c', 'core.quotepath=false', 'check-ignore', '-z', '--stdin'],
      batch.map(e => e.path).join('\0'));
    // exit 1 just means "none of them are ignored" — that is a normal answer, not
    // a failure. Anything else and we do not know, so nothing is suppressed.
    const ignored = new Set(String(ig.out || '').split('\0').filter(Boolean).map(s => stickKey(path.normalize(s))));
    for (const e of batch) {
      if (ignored.has(stickKey(e.path))) { skippedIgnored++; continue; }
      toScan.push(e);
    }
  }

  toScan.sort((a, b) => b.mtime - a.mtime);       // newest first: the work still fresh in mind
  const findings = [];
  let read = 0, bytes = 0, skippedBinary = 0, budgetHit = false;
  for (const e of toScan) {
    if (findings.length >= LEAK_MAX_FINDINGS || bytes >= LEAK_TOTAL_BYTES) { budgetHit = true; break; }
    const r = leakScanFile(e.path);
    if (!r) continue;
    read++; bytes += e.sizeBytes;
    if (r.binary) { skippedBinary++; continue; }
    for (const h of r.hits) {
      if (findings.length >= LEAK_MAX_FINDINGS) break;
      findings.push({
        path: e.path,
        repoRoot: e.repoRoot,
        line: h.line,
        kind: h.kind,
        fragment: h.fragment,                     // first four characters and nothing else, ever
        mtime: e.mtime,
        sessionFile: e.sessionFile,
        sessionTitle: clean(e.sessionTitle, 140),
      });
    }
  }

  return {
    findings,
    scannedSessions: scanned,
    totalSessions: all.length,
    maxSessions: LEAK_MAX_SESSIONS,
    capped,                                       // older sessions existed but were not looked at
    filesRead: read,                              // files actually opened and searched
    filesCapped,                                  // those sessions wrote more files than one pass reads
    skippedNoise,                                 // dependency/build/fixture folders and generated files
    skippedBig,                                   // over LEAK_MAX_BYTES
    skippedBinary,                                // not text
    skippedIgnored,                               // git was told to ignore them
    skippedUnchecked,                             // too many in one project to ask git about, so never opened
    budgetHit,                                    // stopped early on the findings or bytes cap
  };
}

// Public entry point. Cached for 60s, and a single in-flight scan is shared: two
// clicks a second apart must not read every file twice.
async function secretLeaks() {
  if (leakCache.value && Date.now() - leakCache.at < LEAK_TTL) return leakCache.value;
  if (leakInflight) return leakInflight;
  leakInflight = computeLeaks()
    .then(v => { leakCache.at = Date.now(); leakCache.value = v; return v; })
    .finally(() => { leakInflight = null; });
  return leakInflight;
}

// ---------- standalone replay export ----------

function buildExport(result, title) {
  const html = fs.readFileSync(path.join(PUBLIC_DIR, 'index.html'), 'utf8');
  const css = fs.readFileSync(path.join(PUBLIC_DIR, 'style.css'), 'utf8');
  const js = fs.readFileSync(path.join(PUBLIC_DIR, 'app.js'), 'utf8');
  const data = { ...result, now: Date.now() };
  const baked = `<script>window.__BAKED__=${JSON.stringify({ title: title || 'Session replay', data }).replace(/</g, '\\u003c')}</script>`;
  // Replacement FUNCTIONS, not strings: String.replace treats $$ $& $` $' and $1
  // in a replacement string as patterns, and app.js contains `$'` inside fmtUsd —
  // which silently mangled every exported replay into a dead page.
  return html
    .replace('<link rel="stylesheet" href="/style.css">', () => `<style>\n${css}\n</style>`)
    .replace('<script src="/app.js"></script>', () => `${baked}\n<script>\n${js}\n</script>`);
}

// ---------- http server ----------

const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml' };

function json(res, obj, code = 200) {
  res.writeHead(code, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*' });
  res.end(JSON.stringify(obj));
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');

  const authorized = !TOKEN || req.headers['x-relay-token'] === TOKEN;

  // OTLP/HTTP trace ingestion — any OpenTelemetry-instrumented agent can POST here
  if (url.pathname === '/v1/traces' && req.method === 'POST') {
    if (!authorized) return json(res, { error: 'bad or missing x-relay-token' }, 401);
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

  // Relay ingestion — remote machines POST their parsed sessions here
  if (url.pathname === '/v1/relay' && req.method === 'POST') {
    if (!authorized) return json(res, { error: 'bad or missing x-relay-token' }, 401);
    readBody(req, body => {
      try {
        const b = JSON.parse(body);
        if (!b.machine || !b.file || !b.result) throw new Error('need machine, file, result');
        json(res, { ok: true, id: ingestRelay(b), boot: BOOT_ID });
      } catch (e) {
        json(res, { error: e.message }, 400);
      }
    });
    return;
  }

  // full-transcript archive: manifest (what the hub already has) + upload
  if (url.pathname === '/v1/archive/manifest' && req.method === 'GET') {
    if (!authorized) return json(res, { error: 'bad token' }, 401);
    return json(res, { manifest: archiveManifest(url.searchParams.get('machine') || '') });
  }
  if (url.pathname === '/v1/archive' && req.method === 'POST') {
    if (!authorized) return json(res, { error: 'bad token' }, 401);
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        if (!b.machine || !b.relPath || typeof b.data !== 'string') throw new Error('need machine, relPath, data');
        const buf = Buffer.from(b.data, 'base64');
        storeArchiveFile(b.machine, b.relPath, buf);
        json(res, { ok: true });
      } catch (e) { json(res, { error: e.message }, 400); }
    });
  }
  if (url.pathname === '/api/archive' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, { archives: archiveSummary(), dir: ARCHIVE_DIR() });
  }
  if (url.pathname === '/api/archive/list' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    const machine = url.searchParams.get('machine') || '';
    const man = archiveManifest(machine);
    // main transcripts only: claude/<slug>/<uuid>.jsonl (skip subagent/workflow files)
    const items = Object.entries(man)
      .filter(([rel]) => /^claude\/[^/]+\/[^/]+\.jsonl$/.test(rel) || /^codex\//.test(rel))
      .map(([rel, size]) => {
        const abs = path.join(archiveMachineDir(machine), rel);
        let title = null, mtime = 0;
        try { mtime = fs.statSync(abs).mtimeMs; } catch { /* skip */ }
        try {
          const fd = fs.openSync(abs, 'r'); const buf = Buffer.alloc(Math.min(8192, size));
          fs.readSync(fd, buf, 0, buf.length, 0); fs.closeSync(fd);
          for (const line of buf.toString('utf8').split('\n')) { const o = safeParse(line); if (o && (o.type === 'custom-title' || o.type === 'ai-title')) { title = o.customTitle || o.aiTitle; break; } }
        } catch { /* skip */ }
        return { file: 'archive:' + machine + ':' + rel, rel, size, mtime, title: title || rel.split('/').pop() };
      })
      .sort((a, b) => b.mtime - a.mtime);
    return json(res, { machine, items });
  }

  // lightweight identity ping — relays poll this every tick to detect a hub
  // restart (in-memory store wiped) and trigger a full resend
  if (url.pathname === '/v1/boot') {
    // relays poll this every tick; treat it as a machine heartbeat when named
    const hb = req.headers['x-relay-machine'];
    if (hb && authorized) {
      const prev = machines.get(hb) || { name: hb, ips: [], remote: true };
      // deliberately does NOT set lastData: a heartbeat proves the process is up,
      // not that any work is being reported
      machines.set(hb, { ...prev, lastSeen: Date.now(), version: req.headers['x-relay-version'] || prev.version || null });
    }
    return json(res, { boot: BOOT_ID, version: APP_VERSION });
  }

  if (url.pathname === '/api/machines') return json(res, machineList());

  // update check: latest version on GitHub main (cached 6h). Hub never
  // self-updates (a UI you're using shouldn't swap under you) — it just tells.
  if (url.pathname === '/api/update-check') {
    const now = Date.now();
    if (updateCache.at && now - updateCache.at < 6 * 3600e3) return json(res, updateCache.data);
    return fetchLatestVersion().then(v => {
      updateCache.at = now;
      updateCache.data = { current: APP_VERSION, latest: v, updateAvailable: !!v && semverGt(v, APP_VERSION) };
      json(res, updateCache.data);
    }).catch(() => json(res, { current: APP_VERSION, latest: null, updateAvailable: false }));
  }

  // ---- audit log (read-only, gated) ----
  if (url.pathname === '/api/audit' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, { entries: readAudit() });
  }

  // ---- brain version history ----
  if (url.pathname === '/api/brain/history' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    const item = brainResolve(url.searchParams.get('id'));
    if (!item) return json(res, { error: 'unknown file' }, 404);
    return json(res, { history: brainHistory(item.path) });
  }
  if (url.pathname === '/api/brain/snapshot' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    const item = brainResolve(url.searchParams.get('id'));
    if (!item) return json(res, { error: 'unknown file' }, 404);
    const content = brainSnapshotContent(item.path, url.searchParams.get('stamp'));
    if (content == null) return json(res, { error: 'no such snapshot' }, 404);
    return json(res, { content });
  }

  // ---- insight triage (gated) ----
  if (url.pathname === '/api/triage' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, { triage: loadTriage() });
  }
  if (url.pathname === '/api/triage' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        if (typeof b.key !== 'string' || !b.key) throw new Error('key required');
        const t = loadTriage();
        if (b.status === 'open') delete t[b.key];
        else t[b.key] = { status: clean(b.status, 20) || 'resolved', at: Date.now(), note: clean(b.note, 500) };
        saveTriage(t);
        appendAudit({ kind: 'triage', key: b.key, status: b.status });
        json(res, { ok: true, triage: t });
      } catch (e) { metaErr(res, e); }
    });
  }

  // ---- playbook library (gated CRUD, audited) ----
  if (url.pathname === '/api/playbooks' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, loadPlaybooks());
  }
  if (url.pathname === '/api/playbooks' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const store = loadPlaybooks();
        if (b.op === 'save') {
          const name = clean(b.name, 100) || 'Untitled play';
          if (typeof b.body !== 'string' || b.body.length > 60000) throw new Error('bad body');
          if (b.id) {
            const p = store.items.find(x => x.id === b.id); if (!p) throw new Error('not found');
            p.name = name; p.body = b.body; p.kind = clean(b.kind, 40) || p.kind; p.updatedAt = Date.now();
          } else {
            if (store.items.length >= 300) throw new Error('library full');
            store.items.push({ id: 'pb_' + crypto.randomBytes(5).toString('hex'), name, kind: clean(b.kind, 40) || 'custom', body: b.body, source: clean(b.source, 40) || 'manual', createdAt: Date.now(), updatedAt: Date.now() });
          }
          savePlaybooks(store);
          appendAudit({ kind: 'playbook-save', name });
          return json(res, { ok: true, items: store.items });
        }
        if (b.op === 'delete') {
          store.items = store.items.filter(x => x.id !== b.id);
          savePlaybooks(store);
          appendAudit({ kind: 'playbook-delete', id: b.id });
          return json(res, { ok: true, items: store.items });
        }
        throw new Error('bad op');
      } catch (e) { metaErr(res, e); }
    });
  }

  // ---- directive registry (gated CRUD; plants standing orders into guidance files) ----
  if (url.pathname === '/api/directives' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    // `ok:false` = the folder is gone or no longer valid, so it offers no targets;
    // the UI says so instead of showing a healthy-looking row that does nothing.
    return json(res, { items: loadDirectives().items, targets: directiveTargets(), roots: loadDirectiveRoots().roots.map(r => ({ ...r, ok: !!safeRoot(r.path) })) });
  }
  if (url.pathname === '/api/directives' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const store = loadDirectives();
        if (b.op === 'plant') {
          const title = clean(b.title, 100) || 'Untitled directive';
          if (typeof b.body !== 'string' || !b.body.trim() || b.body.length > 20000) throw new Error('bad body');
          if (!Array.isArray(b.targets) || !b.targets.length || b.targets.length > 40) throw new Error('pick at least one target');
          if (store.items.length >= 200) throw new Error('registry full');
          const allowed = new Map(directiveTargets().map(t => [t.id, t]));
          let reviewEveryDays = parseInt(b.reviewEveryDays, 10);
          if (!Number.isFinite(reviewEveryDays) || reviewEveryDays < 1) reviewEveryDays = 30;
          reviewEveryDays = Math.min(reviewEveryDays, 3650);
          // topic is computed deterministically on the client (from insight key + title,
          // no LLM) so the conflict sentry can warn before planting — we just clean & store it.
          const topic = clean(b.topic, 40) || 'general';
          const d = { id: 'dir_' + crypto.randomBytes(5).toString('hex'), title, body: String(b.body), insightKey: clean(b.insightKey, 200) || null, topic, createdAt: Date.now(), reviewEveryDays, lastReviewedAt: Date.now(), targets: [] };
          const results = [];
          for (const id of b.targets) {
            const t = allowed.get(String(id));
            if (!t) { results.push({ id: String(id).slice(0, 60), status: 'unknown-target' }); continue; }
            const r = plantIntoFile(t.path, d);
            results.push({ label: t.label, ...r });
            if (r.status === 'planted') d.targets.push({ path: t.path, label: t.label, name: t.name, plantedAt: Date.now() });
          }
          if (d.targets.length) { store.items.push(d); saveDirectives(store); }
          appendAudit({ kind: 'directive-plant', title, targets: d.targets.length });
          return json(res, { ok: true, items: store.items, results });
        }
        // ---- owner-added folders (widen the allowlist, one real directory at a time) ----
        if (b.op === 'add-root') {
          const p = safeRoot(b.path); // does all the validating — see safeRoot
          if (!p) throw new Error('I could not find that folder on this computer. Paste the full folder path, not a file.');
          const roots = loadDirectiveRoots();
          if (roots.roots.some(r => normRoot(r.path) === normRoot(p))) return json(res, { ok: true, roots: roots.roots, targets: directiveTargets(), note: 'already on the list' });
          if (roots.roots.length >= ROOT_LIMIT) throw new Error(`You already have ${ROOT_LIMIT} folders on the list — remove one first.`);
          roots.roots.push({ path: p, label: path.basename(p) || p, addedAt: Date.now() });
          saveDirectiveRoots(roots);
          appendAudit({ kind: 'directive-root-add', path: p });
          return json(res, { ok: true, roots: roots.roots, targets: directiveTargets() });
        }
        if (b.op === 'remove-root') { // stops offering the folder; anything already planted stays planted
          if (typeof b.path !== 'string' || !b.path.trim() || b.path.length > ROOT_MAX_LEN) throw new Error('which folder?');
          const want = normRoot(path.resolve(clean(b.path, ROOT_MAX_LEN).trim()));
          const roots = loadDirectiveRoots();
          const before = roots.roots.length;
          roots.roots = roots.roots.filter(r => normRoot(r.path) !== want);
          if (roots.roots.length !== before) { saveDirectiveRoots(roots); appendAudit({ kind: 'directive-root-remove', path: want }); }
          // Rules planted here stay live but drop out of the target list, so they
          // vanish from the coverage grid. Say how many, rather than hiding them.
          const stranded = loadDirectives().items.filter(d => (d.targets || []).some(t => normRoot(t.path) === want || normRoot(t.path).startsWith(want + path.sep))).length;
          return json(res, { ok: true, roots: roots.roots, targets: directiveTargets(), stranded });
        }
        if (b.op === 'plant-existing') { // coverage matrix: plant an already-existing order into more targets
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          if (!Array.isArray(b.targets) || !b.targets.length || b.targets.length > 40) throw new Error('pick at least one target');
          const allowed = new Map(directiveTargets().map(t => [t.id, t]));
          const already = new Set((d.targets || []).map(t => t.path));
          const results = [];
          for (const id of b.targets) {
            const t = allowed.get(String(id));
            if (!t) { results.push({ id: String(id).slice(0, 60), status: 'unknown-target' }); continue; }
            if (already.has(t.path)) { results.push({ label: t.label, status: 'already' }); continue; }
            // same ceiling the 'plant' op enforces — the git panel only reports 40
            if ((d.targets || []).length >= 40) { results.push({ label: t.label, status: 'full' }); continue; }
            const r = plantIntoFile(t.path, d);
            results.push({ label: t.label, ...r });
            if (r.status === 'planted') d.targets.push({ path: t.path, label: t.label, name: t.name, plantedAt: Date.now() });
          }
          saveDirectives(store);
          appendAudit({ kind: 'directive-plant', title: d.title, targets: d.targets.length });
          return json(res, { ok: true, items: store.items, results });
        }
        if (b.op === 'check') {
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          return json(res, { ok: true, statuses: checkDirective(d) });
        }
        if (b.op === 'reviewed') { // owner looked it over and it's still good — resets the review clock
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          d.lastReviewedAt = Date.now();
          saveDirectives(store);
          appendAudit({ kind: 'directive-reviewed', title: d.title });
          return json(res, { ok: true, items: store.items });
        }
        // ---- git transport: commit/push a planted rule so other machines pull it ----
        // Only paths already recorded in this directive's targets are ever passed to
        // git; b.path is matched against them and rejected otherwise.
        if (b.op === 'git-status') { // read-only look at each repo — runs nothing that changes anything
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          return gitStatesFor(d).then(states => {
            appendAudit({ kind: 'directive-git-status', title: d.title, targets: states.length });
            json(res, { ok: true, states });
          }).catch(e => metaErr(res, e));
        }
        if (b.op === 'git-commit') {
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          const t = (d.targets || []).find(x => x.path === b.path); if (!t) throw new Error('that file is not one of this order\'s targets');
          return gitCommitDirective(d, t).then(r => {
            appendAudit({ kind: 'directive-git-commit', title: d.title, path: t.path, done: r.done, note: r.note });
            json(res, { ok: true, ...r });
          }).catch(e => metaErr(res, e));
        }
        if (b.op === 'git-push') { // the only outward action in the whole registry
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          const t = (d.targets || []).find(x => x.path === b.path); if (!t) throw new Error('that file is not one of this order\'s targets');
          return gitPushRepo(t).then(r => {
            appendAudit({ kind: 'directive-git-push', title: d.title, path: t.path, done: r.done, note: r.note });
            json(res, { ok: true, ...r });
          }).catch(e => metaErr(res, e));
        }
        if (b.op === 'retire') {
          const d = store.items.find(x => x.id === b.id); if (!d) throw new Error('not found');
          const targets = d.targets || [];
          const results = targets.map(t => ({ label: t.label, ...retireFromFile(t.path, d.id) }));
          // Removing the block is a local edit. If the rule was ever committed and
          // sent, other machines keep obeying it until the REMOVAL is committed too —
          // so name those repos instead of letting "retired" imply "gone everywhere".
          return Promise.all(targets.map(t => gitRepoRoot(t.path).then(r => (r ? t.label : null)).catch(() => null)))
            .then(inGit => {
              store.items = store.items.filter(x => x.id !== b.id);
              saveDirectives(store);
              appendAudit({ kind: 'directive-retire', title: d.title });
              json(res, { ok: true, items: store.items, results, needsCommit: inGit.filter(Boolean) });
            }).catch(e => metaErr(res, e));
        }
        if (b.op === 'delete') { // forget the record without touching any file
          store.items = store.items.filter(x => x.id !== b.id);
          saveDirectives(store);
          appendAudit({ kind: 'directive-forget', id: b.id });
          return json(res, { ok: true, items: store.items });
        }
        throw new Error('bad op');
      } catch (e) { metaErr(res, e); }
    });
  }

  // ---- brain center (loopback + origin + CSRF gated; local files only) ----
  if (url.pathname === '/api/brain' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, { items: brainInventory(), csrf: META_CSRF });
  }
  if (url.pathname === '/api/brain/file' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    const item = brainResolve(url.searchParams.get('id'));
    if (!item) return json(res, { error: 'unknown file' }, 404);
    try { return json(res, { ...item, content: fs.readFileSync(item.path, 'utf8') }); }
    catch (e) { return json(res, { error: e.message }, 500); }
  }
  if (url.pathname === '/api/brain/file' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const item = brainResolve(b.id);
        if (!item) throw new Error('unknown file');
        if (typeof b.content !== 'string' || Buffer.byteLength(b.content) > BRAIN_MAX) throw new Error('bad content');
        const st = fs.statSync(item.path);
        if (b.baseMtime && Math.abs(st.mtimeMs - b.baseMtime) > 1) { const e = new Error('file changed on disk — reload before saving'); e.status409 = true; throw e; }
        fs.copyFileSync(item.path, item.path + '.mc-backup'); // one-deep undo
        snapshotBrain(item); // full version history
        const tmp = item.path + '.mc-tmp';
        fs.writeFileSync(tmp, b.content);
        fs.renameSync(tmp, item.path);
        appendAudit({ kind: 'brain-write', path: item.path, name: item.name, bytes: Buffer.byteLength(b.content), hooks: /settings/.test(item.name) });
        json(res, { ok: true, mtime: fs.statSync(item.path).mtimeMs });
      } catch (e) { metaErr(res, e); }
    });
  }

  // ---- session/project metadata (loopback + origin + CSRF gated) ----
  if (url.pathname === '/api/meta' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    // appVersion rides along here rather than on a new route: the frontend
    // already fetches this once at boot, so the header can name the version
    // you're actually looking at without another request. (Not metaVersion —
    // that one counts metadata edits.)
    return json(res, { ...metaState, csrf: META_CSRF, readOnly: metaReadOnly, appVersion: APP_VERSION });
  }
  if (url.pathname === '/api/meta/session' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        if (typeof b.stableKey !== 'string' || typeof b.patch !== 'object') throw new Error('need stableKey, patch');
        const v = commit(next => applySessionPatch(next, b.stableKey, b.patch));
        json(res, { ok: true, session: metaState.sessions[b.stableKey], metaVersion: v });
      } catch (e) { metaErr(res, e); }
    });
  }
  if (url.pathname === '/api/meta/bulk' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        if (!Array.isArray(b.stableKeys) || typeof b.patch !== 'object') throw new Error('need stableKeys, patch');
        if (b.stableKeys.length > LIM.bulk) throw new Error('too many keys');
        const v = commit(next => { for (const k of b.stableKeys) applySessionPatch(next, k, b.patch); });
        json(res, { ok: true, count: b.stableKeys.length, metaVersion: v });
      } catch (e) { metaErr(res, e); }
    });
  }
  if (url.pathname === '/api/meta/project' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const v = commit(next => {
          if (b.baseVersion != null && b.baseVersion !== metaState.metaVersion) { const e = new Error('stale — refetch'); e.status409 = true; throw e; }
          if (b.op === 'create') {
            if (next.projects.length >= LIM.projects) throw new Error('too many projects');
            next.projects.push({ id: 'p_' + crypto.randomBytes(4).toString('hex'), name: clean(b.name, LIM.name) || 'Project', color: /^#[0-9a-f]{6}$/i.test(b.color || '') ? b.color : '#818cf8', order: next.projects.length, createdAt: metaClock() });
          } else if (b.op === 'update') {
            const p = next.projects.find(x => x.id === b.id); if (!p) throw new Error('no such project');
            if (b.name != null) p.name = clean(b.name, LIM.name);
            if (/^#[0-9a-f]{6}$/i.test(b.color || '')) p.color = b.color;
            if (typeof b.order === 'number') p.order = b.order;
          } else if (b.op === 'delete') {
            next.projects = next.projects.filter(x => x.id !== b.id);
            for (const k of Object.keys(next.sessions)) if (next.sessions[k].projectId === b.id) next.sessions[k].projectId = null;
          } else throw new Error('bad op');
        });
        json(res, { ok: true, projects: metaState.projects, metaVersion: v });
      } catch (e) { metaErr(res, e); }
    });
  }
  if (url.pathname === '/api/meta/tag' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const v = commit(next => {
          if (b.baseVersion != null && b.baseVersion !== metaState.metaVersion) { const e = new Error('stale — refetch'); e.status409 = true; throw e; }
          if (b.op === 'create') {
            if (next.tags.length >= LIM.tags) throw new Error('too many tags');
            next.tags.push({ id: 't_' + crypto.randomBytes(4).toString('hex'), name: clean(b.name, LIM.name) || 'tag', color: /^#[0-9a-f]{6}$/i.test(b.color || '') ? b.color : '#f59e0b' });
          } else if (b.op === 'update') {
            const t = next.tags.find(x => x.id === b.id); if (!t) throw new Error('no such tag');
            if (b.name != null) t.name = clean(b.name, LIM.name);
            if (/^#[0-9a-f]{6}$/i.test(b.color || '')) t.color = b.color;
          } else if (b.op === 'delete') {
            next.tags = next.tags.filter(x => x.id !== b.id);
            for (const k of Object.keys(next.sessions)) next.sessions[k].tags = (next.sessions[k].tags || []).filter(t => t !== b.id);
          } else throw new Error('bad op');
        });
        json(res, { ok: true, tags: metaState.tags, metaVersion: v });
      } catch (e) { metaErr(res, e); }
    });
  }
  // Rename a machine — metadata only. Keyed by the machine's REAL name (never
  // the display name), so it's stable across relay re-sends/hub restarts and
  // never confused with the identity sessions are actually keyed on. Blank
  // displayName clears the override back to the real name.
  if (url.pathname === '/api/meta/machine' && req.method === 'POST') {
    if (!writeGate(req, res)) return;
    return readBody(req, body => {
      try {
        const b = JSON.parse(body);
        const name = clean(b.name, LIM.name);
        if (!name) throw new Error('need name');
        const displayName = clean(b.displayName, LIM.name);
        const v = commit(next => {
          if (b.baseVersion != null && b.baseVersion !== metaState.metaVersion) { const e = new Error('stale — refetch'); e.status409 = true; throw e; }
          if (displayName) next.machineNames[name] = displayName; else delete next.machineNames[name];
        });
        appendAudit({ kind: 'machine-rename', name, displayName: displayName || null });
        json(res, { ok: true, machineNames: metaState.machineNames, metaVersion: v });
      } catch (e) { metaErr(res, e); }
    });
  }

  // deep search: full-text across every session's events (cached parses)
  if (url.pathname === '/api/search') {
    const q = (url.searchParams.get('q') || '').toLowerCase();
    if (q.length < 2) return json(res, { hits: [], scanned: 0 });
    const all = [...relayList(), ...otelList(), ...listSessions(), ...codexList()].sort((a, b) => b.mtime - a.mtime);
    const hits = [];
    let scanned = 0;
    for (const meta of all) {
      if (hits.length >= 120) break;
      let r;
      try { r = getResult(meta.file); } catch { continue; }
      if (!r) continue;
      scanned++;
      let perSession = 0;
      for (const e of r.events) {
        if (perSession >= 3) break;
        const hay = (e.full || e.text || '') + ' ' + (e.tool || '');
        const idx = hay.toLowerCase().indexOf(q);
        if (idx < 0) continue;
        perSession++;
        hits.push({
          file: meta.file, title: meta.title || meta.session, kind: agentKindOf(meta.file),
          machine: meta.machine || (meta.file.startsWith('relay:') ? meta.file.split(':')[1] : os.hostname()),
          mtime: meta.mtime, seq: e.seq, ts: e.ts, eventKind: e.kind, tool: e.tool || null,
          snippet: hay.slice(Math.max(0, idx - 60), idx + q.length + 90).replace(/\s+/g, ' '),
        });
      }
    }
    return json(res, { hits, scanned });
  }

  if (url.pathname === '/api/sessions' || url.pathname === '/api/fleet') {
    const all = [...relayList(), ...otelList(), ...listSessions(), ...codexList()].sort((a, b) => b.mtime - a.mtime);
    if (url.pathname === '/api/fleet') return json(res, all.map(sessionSummary).filter(Boolean));
    return json(res, all.map(it => ({ ...it, kind: agentKindOf(it.file), machine: it.machine || (it.file.startsWith('relay:') ? it.file.split(':')[1] : os.hostname()), stableKey: stableKeyForItem(it) })));
  }

  if (url.pathname === '/api/session') {
    const r = getResult(url.searchParams.get('file') || '');
    if (!r) return json(res, { error: 'not found' }, 404);
    return json(res, { ...r, now: Date.now() });
  }

  // ---- did it stick: one session vs. git (gated read) ----
  // Fetched lazily, one session at a time. Deliberately NOT part of /api/fleet:
  // scoring a whole fleet would fork git dozens of times on every refresh.
  if (url.pathname === '/api/stickiness' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return sessionStickiness(url.searchParams.get('file') || '')
      .then(s => json(res, s))
      .catch(() => json(res, { status: 'unknown', reason: 'Something went wrong while checking this session against your code.' }));
  }

  // ---- unsaved work: ghost files across the recent fleet (gated read) ----
  // One fleet-wide scan, cached 30s inside unsavedWork(). No file it names is ever
  // touched — there is no companion write route here, and there never will be.
  if (url.pathname === '/api/unsaved' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return unsavedWork()
      .then(u => json(res, u))
      .catch(() => json(res, { files: [], error: 'Something went wrong while looking for work that was never saved.' }));
  }

  // ---- trouble files: which files agents keep fighting with (gated read) ----
  // Same session data "unsaved work" already reads, aggregated by path instead
  // of by session. No git involved. Cached 30s inside troubleFiles().
  if (url.pathname === '/api/trouble-files' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    try { return json(res, troubleFiles()); }
    catch { return json(res, { files: [], error: 'Something went wrong while looking for files your agents keep fighting with.' }); }
  }

  // ---- the graveyard: moments an agent threw work away (gated read) ----
  // Raw transcript scan, cached 60s inside graveyard(). It reads command text and
  // nothing else — no git, no writes, and no companion write route: v1 proves the
  // list is right before anything is allowed to act on it.
  if (url.pathname === '/api/graveyard' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    try { return json(res, graveyard()); }
    catch { return json(res, { moments: [], error: 'Something went wrong while looking for work your agents threw away.' }); }
  }

  // ---- recent tool calls: what a permission rule would have matched (gated read) ----
  // Feeds the Brain tab's read-only "blast radius" preview. There is no companion
  // write route: nothing in that preview can save a settings file, by design.
  if (url.pathname === '/api/tool-calls' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    try { return json(res, recentToolCalls()); }
    catch { return json(res, { calls: [], error: 'Something went wrong while reading what your agents have actually run.' }); }
  }

  // ---- secret leak sentinel: credential shapes in files agents wrote (gated read) ----
  // The response carries a kind, a line number and four characters — never the
  // matched text. There is no companion write route, and nothing is ever written
  // to disk. Cached 60s inside secretLeaks().
  if (url.pathname === '/api/leaks' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return secretLeaks()
      .then(l => json(res, l))
      .catch(() => json(res, { findings: [], error: 'Something went wrong while checking your agents’ files for keys.' }));
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
        : fileParam.startsWith('relay:')
          ? String(relaySessions.get(fileParam)?.version || 0)
          : fileParam.startsWith('codex:')
            ? codexSignature(fileParam.slice(6))
            : sessionSignature(resolveSessionPath(fileParam));
      const mv = metaState ? metaState.metaVersion : 0;
      if (sig !== lastSig) {
        lastSig = sig;
        try {
          const r = getResult(fileParam);
          if (r) res.write('data: ' + JSON.stringify({ ...r, now: Date.now(), metaVersion: mv }) + '\n\n');
        } catch { /* mid-write read; next tick catches up */ }
      } else {
        res.write(`: keepalive mv=${mv}\n\n`); // cheap metaVersion heartbeat for cross-tab freshness
      }
    };
    tick();
    const timer = setInterval(tick, 700);
    req.on('close', () => clearInterval(timer));
    return;
  }

  // app icon: serve the local personal icon if present (gitignored), else the
  // neutral committed default — keeps the public repo free of the photo
  if (url.pathname === '/app-icon' || url.pathname === '/favicon.ico') {
    // default: the satellite logo. Drop a public/icon-override.png to swap it
    // locally (e.g. the mullet); gitignored, never in the public repo.
    const override = path.join(PUBLIC_DIR, 'icon-override.png');
    if (fs.existsSync(override)) {
      res.writeHead(200, { 'Content-Type': 'image/png', 'Cache-Control': 'no-store' });
      return res.end(fs.readFileSync(override));
    }
    res.writeHead(200, { 'Content-Type': 'image/svg+xml', 'Cache-Control': 'no-store' });
    return res.end(fs.readFileSync(path.join(PUBLIC_DIR, 'favicon.svg')));
  }

  // static files
  const rel = url.pathname === '/' ? '/index.html' : url.pathname;
  const file = path.resolve(PUBLIC_DIR, '.' + rel);
  if (file.startsWith(path.resolve(PUBLIC_DIR)) && fs.existsSync(file) && fs.statSync(file).isFile()) {
    // no-store: the app self-updates in place, so never let a browser pin a stale bundle
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
    res.end(fs.readFileSync(file));
    return;
  }

  res.writeHead(404); res.end('not found');
});

// ---------- update visibility (no self-update: humans stay in the loop) ----------
const GH_RAW = 'https://raw.githubusercontent.com/evanchakrin/agent-mission-control/main';
const updateCache = { at: 0, data: null };
function semverGt(a, b) {
  const pa = String(a).split('.').map(Number), pb = String(b).split('.').map(Number);
  for (let i = 0; i < 3; i++) { if ((pa[i] || 0) > (pb[i] || 0)) return true; if ((pa[i] || 0) < (pb[i] || 0)) return false; }
  return false;
}
async function fetchLatestVersion() {
  const r = await fetch(GH_RAW + '/package.json');
  if (!r.ok) return null;
  return (await r.json()).version || null;
}

// ---------- relay (client side) ----------
// `--relay http://hub:4173 [--token secret] [--name machine-name]`
// No UI here: every few seconds, any local session whose files changed is
// parsed and POSTed to the hub, where it appears in the fleet.
async function runRelay(hub, machineName) {
  const sent = new Map(); // file -> last signature
  let hubBoot = null;     // hub restarts wipe its in-memory store; resend everything when its boot id changes
  console.log(`Relaying local sessions → ${hub}/v1/relay as "${machineName}"`);
  console.log(`Watching transcripts in ${PROJECTS_DIR}`);
  const tick = async () => {
    // detect hub restart up front, before any skip logic — a wiped hub gets a
    // full resend even when no local session changed
    try {
      const b = await fetch(hub + '/v1/boot', { headers: { 'x-relay-machine': machineName, 'x-relay-version': APP_VERSION, ...(TOKEN ? { 'x-relay-token': TOKEN } : {}) } }).then(r => r.json()).catch(() => null);
      if (b && b.boot) {
        if (hubBoot && b.boot !== hubBoot) { sent.clear(); console.log('hub restarted — resending all sessions'); }
        hubBoot = b.boot;
      }
    } catch { /* hub down; sends below will fail and retry */ }
    // WHY: the hub cannot work out which project a relayed session belongs to —
    // it only ever sees what arrives here, which is why every remote session
    // used to be labelled with this machine's name instead. Only THIS side knows
    // the answer, so send it: the project-directory slug, plus the true working
    // directory when it can be resolved (claudeProjectCwd / Codex session_meta).
    // Two short strings per session; the parsed result dwarfs them.
    let codexCwds = null; // built once per tick, and only if a Codex session actually changed
    for (const meta of [...listSessions(), ...codexList()]) {
      const isCodex = meta.file.startsWith('codex:');
      const sig = isCodex ? codexSignature(meta.file.slice(6)) : sessionSignature(resolveSessionPath(meta.file));
      if (!sig || sent.get(meta.file) === sig) continue;
      let result;
      try { result = getResult(meta.file); } catch (e) { console.error(`parse failed ${meta.file}: ${e.message}`); continue; }
      if (!result) continue;
      if (isCodex && !codexCwds) codexCwds = new Map(codexFiles().map(f => [f.uuid, codexMeta(f).cwd || null]));
      const proj = {
        slug: meta.project || null,
        cwd: (isCodex ? codexCwds.get(meta.file.slice(6))
          : meta.project ? claudeProjectCwd(path.join(PROJECTS_DIR, meta.project)) : null) || null,
      };
      // keep POSTs under the hub's body cap — trim oldest events if needed
      const ips = localIPs();
      let body = JSON.stringify({ machine: machineName, file: meta.file, meta, proj, result, ips, version: APP_VERSION });
      while (body.length > 35e6 && result.events.length > 500) {
        result = { ...result, events: result.events.slice(Math.ceil(result.events.length / 2)) };
        body = JSON.stringify({ machine: machineName, file: meta.file, meta, proj, result, ips, version: APP_VERSION });
      }
      try {
        const r = await fetch(hub + '/v1/relay', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...(TOKEN ? { 'x-relay-token': TOKEN } : {}) },
          body,
        });
        if (r.ok) {
          sent.set(meta.file, sig);
          const info = await r.json().catch(() => ({}));
          if (info.boot && hubBoot && info.boot !== hubBoot) { sent.clear(); sent.set(meta.file, sig); }
          if (info.boot) hubBoot = info.boot;
          console.log(`sent ${meta.title || meta.session} (${Math.round(body.length / 1024)}KB)`);
        } else {
          console.error(`hub rejected ${meta.file}: ${r.status} ${await r.text()}`);
        }
      } catch (e) {
        console.error(`send failed for ${meta.file}: ${e.message} — will retry`);
        // keep going: one bad session must not block the rest
      }
    }
  };
  await tick();
  setInterval(tick, 5000);

  // --archive: upload raw .jsonl transcripts the hub doesn't have yet. Runs on a
  // slower cadence than the live relay; skips oversized files; Codex rollouts
  // included only with --archive-codex (they can be hundreds of MB each).
  if (process.argv.includes('--archive')) {
    const withCodex = process.argv.includes('--archive-codex');
    // base64 inflates by 4/3 and the JSON envelope adds a little; leave 15% headroom.
    // Anything above this CANNOT be accepted, so sending it is guaranteed waste.
    const FILE_CAP = Math.floor((50e6 * 0.85) * 3 / 4);
    const failed = new Map();          // relPath -> consecutive failures
    const FAIL_GIVE_UP = 2;            // stop after this many; never retry into the ground
    const skipped = [];
    const collect = () => {
      const files = [];
      const walk = (dir, relBase) => {
        let entries = [];
        try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
        for (const e of entries) {
          const full = path.join(dir, e.name), rel = relBase ? relBase + '/' + e.name : e.name;
          if (e.isDirectory()) walk(full, rel);
          else if (e.name.endsWith('.jsonl')) { try { const st = fs.statSync(full); files.push({ full, rel, size: st.size }); } catch { /* skip */ } }
        }
      };
      walk(PROJECTS_DIR, 'claude');
      if (withCodex) walk(CODEX_DIR, 'codex');
      return files;
    };
    const archiveTick = async () => {
      let manifest = {};
      try { manifest = (await fetch(hub + '/v1/archive/manifest?machine=' + encodeURIComponent(machineName), { headers: TOKEN ? { 'x-relay-token': TOKEN } : {} }).then(r => r.json())).manifest || {}; }
      catch { return; }
      let sent = 0;
      for (const f of collect()) {
        if (f.size > FILE_CAP) {
          if (!skipped.includes(f.rel)) { skipped.push(f.rel); console.log(`archive: skipping ${f.rel} (${Math.round(f.size / 1048576)}MB) — larger than this hub can accept`); }
          continue;
        }
        if ((failed.get(f.rel) || 0) >= FAIL_GIVE_UP) continue; // already refused twice; stop burning memory on it
        if (manifest[f.rel] === f.size) continue; // hub already has this exact file
        try {
          const data = fs.readFileSync(f.full).toString('base64');
          const r = await fetch(hub + '/v1/archive', {
            method: 'POST', headers: { 'Content-Type': 'application/json', ...(TOKEN ? { 'x-relay-token': TOKEN } : {}) },
            body: JSON.stringify({ machine: machineName, relPath: f.rel, data }),
          });
          if (r.ok) { sent++; failed.delete(f.rel); if (sent % 10 === 0) console.log(`archived ${sent} transcripts…`); }
          else if (r.status === 400) { const e = await r.json().catch(() => ({})); if (/archive full/.test(e.error || '')) { console.error('hub archive full — stopping'); return; } }
          else {
            // Remember the refusal. Retrying a file the hub will never accept is what
            // allocated ~4x its size every tick until the machine ran out of memory.
            const n = (failed.get(f.rel) || 0) + 1;
            failed.set(f.rel, n);
            console.error(`archive: ${f.rel} refused (${r.status})${n >= FAIL_GIVE_UP ? ' — giving up on it' : ''}`);
          }
        } catch (e) {
          const n = (failed.get(f.rel) || 0) + 1;
          failed.set(f.rel, n);
          console.error('archive upload failed', f.rel, e.message, n >= FAIL_GIVE_UP ? '— giving up on it' : '');
        }
        await new Promise(r => setTimeout(r, 50)); // gentle pacing
      }
      if (sent) console.log(`archive pass done: ${sent} new/changed transcripts uploaded`);
    };
    console.log(`Archiving raw transcripts → ${hub}/v1/archive${withCodex ? ' (incl. Codex)' : ''}`);
    setTimeout(archiveTick, 8000);          // first pass after startup
    setInterval(archiveTick, 5 * 60 * 1000); // then every 5 min
  }
}

// ---------- install as an always-on background service (Windows) ----------
// `--install [--relay hub --token x ...]` copies the app to LocalAppData,
// registers a hidden launcher in the Startup folder (runs at every login),
// starts it now, and drops a desktop shortcut to the dashboard. No admin,
// no terminal afterwards. `--uninstall` removes all of it.
function installPaths() {
  return {
    dest: path.join(process.env.LOCALAPPDATA, 'AgentMissionControl'),
    startupVbs: path.join(process.env.APPDATA, 'Microsoft', 'Windows', 'Start Menu', 'Programs', 'Startup', 'AgentMissionControl.vbs'),
    desktopUrl: path.join(os.homedir(), 'Desktop', 'Agent Mission Control.url'), // legacy, removed on install
    desktopLnk: path.join(os.homedir(), 'Desktop', 'Agent Mission Control.lnk'),
  };
}

function installWindows() {
  if (process.platform !== 'win32') {
    console.log('--install is Windows-only for now. On Mac/Linux, add "node server.js" to launchd/systemd.');
    process.exit(1);
  }
  const { dest, startupVbs, desktopUrl, desktopLnk } = installPaths();
  fs.mkdirSync(dest, { recursive: true });
  fs.copyFileSync(__filename, path.join(dest, 'server.js'));
  fs.cpSync(path.join(__dirname, 'public'), path.join(dest, 'public'), { recursive: true });
  try { fs.copyFileSync(path.join(__dirname, 'package.json'), path.join(dest, 'package.json')); } catch { /* version falls back */ }

  const extra = [];
  for (const f of ['--relay', '--token', '--name', '--port', '--dir']) {
    const v = argValue(f);
    if (v) extra.push(f, v);
  }
  for (const flag of ['--archive', '--archive-codex', '--no-auto-update']) if (process.argv.includes(flag)) extra.push(flag);
  const inner = ['node', `"${path.join(dest, 'server.js')}"`, ...extra.map(x => x.startsWith('--') ? x : `"${x}"`)].join(' ');
  const vbs = `CreateObject("Wscript.Shell").Run "${inner.replace(/"/g, '""')}", 0\r\n`;
  fs.writeFileSync(path.join(dest, 'start.vbs'), vbs);
  fs.writeFileSync(startupVbs, vbs);

  if (!RELAY_TO) {
    // The old shortcut was a plain .url: it opened the browser but never STARTED
    // the service, so clicking it while stopped just produced a connection error
    // with no clue why. launch.vbs health-checks the port, starts start.vbs if
    // needed, waits for it to bind, then opens the dashboard.
    const Q = String.fromCharCode(34);
    const launch = [
      'Option Explicit',
      'Dim sh, fso, here, url, http, i',
      'Set sh = CreateObject(' + Q + 'WScript.Shell' + Q + ')',
      'Set fso = CreateObject(' + Q + 'Scripting.FileSystemObject' + Q + ')',
      'here = fso.GetParentFolderName(WScript.ScriptFullName)',
      'url = ' + Q + 'http://localhost:' + PORT + Q,
      'Function IsUp()',
      '  IsUp = False',
      '  On Error Resume Next',
      '  Set http = CreateObject(' + Q + 'MSXML2.ServerXMLHTTP.6.0' + Q + ')',
      '  http.SetTimeouts 800, 800, 1500, 1500',
      '  http.Open ' + Q + 'GET' + Q + ', url & ' + Q + '/api/meta' + Q + ', False',
      '  http.Send',
      '  If Err.Number = 0 Then IsUp = True',
      '  On Error GoTo 0',
      'End Function',
      'If Not IsUp() Then',
      '  sh.Run ' + Q + 'wscript ' + Q + Q + Q + ' & here & ' + Q + '\start.vbs' + Q + Q + Q + ', 0, False',
      '  For i = 1 To 20',
      '    WScript.Sleep 500',
      '    If IsUp() Then Exit For',
      '  Next',
      'End If',
      'If IsUp() Then',
      '  sh.Run url, 1, False',
      'Else',
      '  MsgBox ' + Q + 'Agent Mission Control could not start. Try opening ' + Q + ' & url, 48, ' + Q + 'Agent Mission Control' + Q,
      'End If',
    ].join('\r\n') + '\r\n';
    fs.writeFileSync(path.join(dest, 'launch.vbs'), launch);

    // the brand mark, drawn in pure Node so any machine can produce it
    let iconArg = '';
    try {
      const { buildIco } = require(path.join(__dirname, 'tools', 'make-icon.js'));
      fs.writeFileSync(path.join(dest, 'icon.ico'), buildIco([16, 32, 48, 64, 128, 256]));
      iconArg = path.join(dest, 'icon.ico') + ',0';
    } catch { /* no icon: Windows uses its default, the shortcut still works */ }

    try {
      const mk = path.join(dest, 'mkshortcut.vbs');
      fs.writeFileSync(mk, [
        'Dim sh, lnk',
        'Set sh = CreateObject(' + Q + 'WScript.Shell' + Q + ')',
        'Set lnk = sh.CreateShortcut(' + Q + desktopLnk + Q + ')',
        'lnk.TargetPath = ' + Q + 'wscript.exe' + Q,
        'lnk.Arguments = ' + Q + Q + Q + path.join(dest, 'launch.vbs') + Q + Q + Q,
        'lnk.WorkingDirectory = ' + Q + dest + Q,
        iconArg ? 'lnk.IconLocation = ' + Q + iconArg + Q : '',
        'lnk.Description = ' + Q + 'Agent Mission Control - starts the dashboard if it is not running, then opens it' + Q,
        'lnk.WindowStyle = 1',
        'lnk.Save',
      ].filter(Boolean).join('\r\n') + '\r\n');
      require('child_process').execFileSync('cscript', ['//nologo', mk], { windowsHide: true, timeout: 15000 });
      try { fs.unlinkSync(desktopUrl); } catch { /* no legacy shortcut to clean up */ }
    } catch {
      // couldn't build a .lnk: fall back to the old behaviour rather than nothing
      try { fs.writeFileSync(desktopUrl, `[InternetShortcut]\r\nURL=http://localhost:${PORT}\r\n`); } catch { /* no Desktop dir */ }
    }
  }

  require('child_process').spawn('wscript', [path.join(dest, 'start.vbs')], { detached: true, stdio: 'ignore' }).unref();
  console.log(`Installed. Runs now and at every login (${RELAY_TO ? 'relay → ' + RELAY_TO : 'dashboard at http://localhost:' + PORT}).`);
  if (!RELAY_TO) console.log('Desktop shortcut created: "Agent Mission Control".');
  console.log('Remove any time with: --uninstall');
  process.exit(0);
}

function uninstallWindows() {
  const { dest, startupVbs, desktopUrl, desktopLnk } = installPaths();
  // both shortcut kinds: the current .lnk and any legacy .url left by an old install
  for (const p of [startupVbs, desktopUrl, desktopLnk]) { try { fs.unlinkSync(p); } catch { /* absent */ } }
  try { fs.rmSync(dest, { recursive: true, force: true }); } catch { /* absent */ }
  console.log('Uninstalled (a running instance keeps going until logoff/reboot or you end node.exe in Task Manager).');
  process.exit(0);
}

if (process.argv.includes('--install')) {
  installWindows();
} else if (process.argv.includes('--uninstall')) {
  uninstallWindows();
} else if (RELAY_TO) {
  runRelay(RELAY_TO.replace(/\/$/, ''), argValue('--name') || os.hostname());
} else {
  loadState();
  loadRelayCache(); // restore relayed sessions from disk so a restart keeps them
  server.listen(PORT, () => {
    console.log(`Agent Mission Control → http://localhost:${PORT}`);
    console.log(`Watching transcripts in ${PROJECTS_DIR}`);
    console.log(`Session metadata: ${STATE_FILE}${metaReadOnly ? ' (READ-ONLY: recovered from corrupt state)' : ''}`);
    if (TOKEN) console.log('Relay/OTLP ingestion requires x-relay-token');
  });
}
