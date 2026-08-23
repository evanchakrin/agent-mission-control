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

const APP_VERSION = '5.2.0'; // keep in sync with package.json

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
  const ctx = { agents: new Map(), events: [], pending: new Map(), subThreads: new Map() };
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
    let payload = JSON.stringify({ id: rec.id, machine: rec.machine, meta: rec.meta, ips: rec.ips, at: rec.at, result: { ...rec.result, events } });
    while (payload.length > RELAY_MAX_BYTES && events.length > 200) { // keep the disk cache bounded
      events = events.slice(Math.ceil(events.length / 2));
      payload = JSON.stringify({ id: rec.id, machine: rec.machine, meta: rec.meta, ips: rec.ips, at: rec.at, result: { ...rec.result, events } });
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

function loadRelayCache() {
  let files = [];
  try { files = fs.readdirSync(RELAY_DIR()).filter(f => f.endsWith('.json')); } catch { return; }
  let n = 0;
  for (const f of files) {
    try {
      const r = JSON.parse(fs.readFileSync(path.join(RELAY_DIR(), f), 'utf8'));
      if (!r.id || !r.result) continue;
      relaySessions.set(r.id, { id: r.id, machine: r.machine, meta: r.meta || {}, version: 1, result: r.result, ips: r.ips, at: r.at });
      if (r.machine) machines.set(r.machine, { name: r.machine, ips: Array.isArray(r.ips) ? r.ips : [], lastSeen: r.at || Date.now(), remote: true, cached: true });
      n++;
    } catch { /* skip bad file */ }
  }
  if (n) console.log(`Restored ${n} relayed sessions from cache (${RELAY_DIR()})`);
}

function ingestRelay(body) {
  const id = 'relay:' + body.machine + ':' + body.file;
  const prev = relaySessions.get(id);
  const rec = {
    id, machine: body.machine, meta: body.meta || {},
    version: (prev ? prev.version : 0) + 1, result: body.result,
    ips: Array.isArray(body.ips) ? body.ips : [], at: Date.now(),
  };
  relaySessions.set(id, rec);
  machines.set(body.machine, { name: body.machine, ips: rec.ips, lastSeen: Date.now(), remote: true, version: body.version || null });
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

function machineList() {
  const localName = os.hostname();
  const local = { name: localName, ips: localIPs(), lastSeen: Date.now(), remote: false, version: APP_VERSION };
  return [local, ...[...machines.values()].filter(m => m.name !== localName)];
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
  return { v: 1, metaVersion: 0, machineId: crypto.randomUUID(), projects: [], tags: [], savedFilters: [], sessions: {} };
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

// ---------- brain center (local memories, hooks, agent configs) ----------
// Read/write the agent "brains" on THIS machine only: Claude global memory,
// per-project memory stores, hook settings, Codex AGENTS.md/config. Same
// loopback+origin+CSRF gating as metadata — these files steer your agents,
// so they are never exposed to the LAN and never editable remotely.
const BRAIN_MAX = 512 * 1024;
function brainInventory() {
  const home = os.homedir();
  const items = [];
  const add = (category, p, name) => {
    try {
      const st = fs.statSync(p);
      if (st.isFile() && st.size <= BRAIN_MAX) items.push({ id: enc(p), category, path: p, name: name || path.basename(p), size: st.size, mtime: st.mtimeMs });
    } catch { /* absent */ }
  };
  add('Claude · global', path.join(home, '.claude', 'CLAUDE.md'), 'CLAUDE.md (global memory)');
  add('Claude · hooks & settings', path.join(home, '.claude', 'settings.json'), 'settings.json (hooks, permissions)');
  add('Claude · hooks & settings', path.join(home, '.claude', 'settings.local.json'), 'settings.local.json');
  try {
    for (const proj of fs.readdirSync(PROJECTS_DIR)) {
      const memDir = path.join(PROJECTS_DIR, proj, 'memory');
      try {
        for (const f of fs.readdirSync(memDir)) {
          if (f.endsWith('.md')) add('Claude · project memory (' + proj.replace(/^[Cc]--Users-[^-]+-/, '').slice(0, 30) + ')', path.join(memDir, f));
        }
      } catch { /* no memory dir */ }
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

function relayList() {
  return [...relaySessions.values()].map(s => {
    let title = s.meta.title;
    if (!title) {
      const firstUser = s.result.events.find(e => (e.kind === 'user-text' || e.kind === 'user-queued') && e.text && !e.text.startsWith('<'));
      title = firstUser ? clip(firstUser.text.replace(/\s+/g, ' ').trim(), 70) : s.id.split(':').pop().slice(0, 12);
    }
    return {
      project: '⇄ ' + s.machine, file: s.id, session: s.meta.session || s.id, machine: s.machine,
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
  const machine = meta.file.startsWith('relay:') ? meta.file.split(':')[1] : os.hostname();
  return {
    file: meta.file, project: meta.project, session: meta.session, title: meta.title, mtime: meta.mtime,
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
    stalled: r.agents.some(a => a.pendingTool && a.pendingTool.since && Date.now() - new Date(a.pendingTool.since) > 120000) && Date.now() - meta.mtime < 600000,
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

  // lightweight identity ping — relays poll this every tick to detect a hub
  // restart (in-memory store wiped) and trigger a full resend
  if (url.pathname === '/v1/boot') {
    // relays poll this every tick; treat it as a machine heartbeat when named
    const hb = req.headers['x-relay-machine'];
    if (hb && authorized) {
      const prev = machines.get(hb) || { name: hb, ips: [], remote: true };
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
        const tmp = item.path + '.mc-tmp';
        fs.writeFileSync(tmp, b.content);
        fs.renameSync(tmp, item.path);
        json(res, { ok: true, mtime: fs.statSync(item.path).mtimeMs });
      } catch (e) { metaErr(res, e); }
    });
  }

  // ---- session/project metadata (loopback + origin + CSRF gated) ----
  if (url.pathname === '/api/meta' && req.method === 'GET') {
    if (!metaGate(req, res)) return;
    return json(res, { ...metaState, csrf: META_CSRF, readOnly: metaReadOnly });
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
    for (const meta of [...listSessions(), ...codexList()]) {
      const isCodex = meta.file.startsWith('codex:');
      const sig = isCodex ? codexSignature(meta.file.slice(6)) : sessionSignature(resolveSessionPath(meta.file));
      if (!sig || sent.get(meta.file) === sig) continue;
      let result;
      try { result = getResult(meta.file); } catch (e) { console.error(`parse failed ${meta.file}: ${e.message}`); continue; }
      if (!result) continue;
      // keep POSTs under the hub's body cap — trim oldest events if needed
      const ips = localIPs();
      let body = JSON.stringify({ machine: machineName, file: meta.file, meta, result, ips, version: APP_VERSION });
      while (body.length > 35e6 && result.events.length > 500) {
        result = { ...result, events: result.events.slice(Math.ceil(result.events.length / 2)) };
        body = JSON.stringify({ machine: machineName, file: meta.file, meta, result, ips, version: APP_VERSION });
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
    desktopUrl: path.join(os.homedir(), 'Desktop', 'Agent Mission Control.url'),
  };
}

function installWindows() {
  if (process.platform !== 'win32') {
    console.log('--install is Windows-only for now. On Mac/Linux, add "node server.js" to launchd/systemd.');
    process.exit(1);
  }
  const { dest, startupVbs, desktopUrl } = installPaths();
  fs.mkdirSync(dest, { recursive: true });
  fs.copyFileSync(__filename, path.join(dest, 'server.js'));
  fs.cpSync(path.join(__dirname, 'public'), path.join(dest, 'public'), { recursive: true });

  const extra = [];
  for (const f of ['--relay', '--token', '--name', '--port', '--dir']) {
    const v = argValue(f);
    if (v) extra.push(f, v);
  }
  const inner = ['node', `"${path.join(dest, 'server.js')}"`, ...extra.map(x => x.startsWith('--') ? x : `"${x}"`)].join(' ');
  const vbs = `CreateObject("Wscript.Shell").Run "${inner.replace(/"/g, '""')}", 0\r\n`;
  fs.writeFileSync(path.join(dest, 'start.vbs'), vbs);
  fs.writeFileSync(startupVbs, vbs);

  if (!RELAY_TO) {
    try {
      fs.writeFileSync(desktopUrl, `[InternetShortcut]\r\nURL=http://localhost:${PORT}\r\n`);
    } catch { /* no Desktop dir */ }
  }

  require('child_process').spawn('wscript', [path.join(dest, 'start.vbs')], { detached: true, stdio: 'ignore' }).unref();
  console.log(`Installed. Runs now and at every login (${RELAY_TO ? 'relay → ' + RELAY_TO : 'dashboard at http://localhost:' + PORT}).`);
  if (!RELAY_TO) console.log('Desktop shortcut created: "Agent Mission Control".');
  console.log('Remove any time with: --uninstall');
  process.exit(0);
}

function uninstallWindows() {
  const { dest, startupVbs, desktopUrl } = installPaths();
  for (const p of [startupVbs, desktopUrl]) { try { fs.unlinkSync(p); } catch { /* absent */ } }
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
