// Agent Mission Control v2.0 — frontend
const $ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

const BAKED = window.__BAKED__ || null; // standalone replay export mode

const KINDS = ['user-text', 'user-queued', 'assistant-text', 'tool-call', 'tool-result', 'spawn', 'spawn-result'];
const KIND_LABEL = { 'user-text': 'user', 'user-queued': 'queued', 'assistant-text': 'reply', 'tool-call': 'tool', 'tool-result': 'result', 'spawn': 'spawn', 'spawn-result': 'return' };
const KIND_COLOR = { 'user-text': '#f87171', 'user-queued': '#f87171', 'assistant-text': '#fbbf24', 'tool-call': '#818cf8', 'tool-result': '#60a5fa', 'spawn': '#5eead4', 'spawn-result': '#34d399' };

const state = {
  data: { events: [], agents: [], now: 0 },
  es: null, live: !BAKED, scrub: 0, file: null,
  playing: false, speed: 4, playTimer: null,
  view: BAKED ? 'board' : 'fleet',
  filterText: '', kindsOn: new Set(KINDS),
  hot: new Map(), lastSeq: -1,
};

// ---------- sessions & fleet ----------
async function loadSessions() {
  const sessions = await (await fetch('/api/sessions')).json();
  const sel = $('sessionSel');
  sel.innerHTML = '<option value="">— select session —</option>';
  for (const s of sessions) {
    const opt = document.createElement('option');
    opt.value = s.file;
    const when = new Date(s.mtime).toLocaleString();
    const agents = s.agentCount ? ` · ${s.agentCount} agents` : '';
    opt.textContent = `${s.title || s.session.slice(0, 8)} — ${s.project.replace(/^[Cc]--Users-[^-]+-/, '')}${agents} (${when})`;
    sel.appendChild(opt);
  }
  sel.onchange = () => { if (sel.value) openSession(sel.value); };
}

async function loadFleet() {
  $('fleet').innerHTML = '<div class="fleet-loading">Scanning sessions…</div>';
  const fleet = await (await fetch('/api/fleet')).json();
  const totCost = fleet.reduce((n, s) => n + s.cost, 0);
  const totAgents = fleet.reduce((n, s) => n + s.agents, 0);
  $('fleet').innerHTML =
    `<div class="fleet-head"><h2>Fleet — ${fleet.length} sessions · ${totAgents} agents · ~${fmtUsd(totCost)} total</h2></div>` +
    `<div class="fleet-grid">` + fleet.map(s => `
      <div class="fcard" data-file="${esc(s.file)}">
        <h3>${esc(s.title || s.session.slice(0, 8))}</h3>
        <div class="fproj">${esc(s.project.replace(/^[Cc]--Users-[^-]+-/, ''))}</div>
        <div class="fstats">
          <span><b>${s.agents}</b> agents</span><span><b>${s.events}</b> events</span>
          <span><b>${s.toolCalls}</b> tools</span><span><b>${fmtDur(s.durationMs)}</b></span>
          <span><b>${fmtTok(s.tokensIn)}</b> in</span><span><b>${fmtTok(s.tokensOut)}</b> out</span>
          <span class="fcost"><b>~${fmtUsd(s.cost)}</b></span>
          ${s.errors ? `<span class="ferr"><b>${s.errors}</b> errors</span>` : ''}
        </div>
        <div class="fdate">${new Date(s.mtime).toLocaleString()}</div>
      </div>`).join('') + `</div>`;
  $('fleet').querySelectorAll('.fcard').forEach(c => { c.onclick = () => openSession(c.dataset.file); });
}

function openSession(file) {
  state.view = 'board';
  setTabs();
  $('sessionSel').value = file;
  connect(file);
}

function connect(file) {
  state.file = file;
  if (state.es) state.es.close();
  state.lastSeq = -1;
  state.hot.forEach(t => clearTimeout(t)); state.hot.clear();
  stopPlay();
  state.live = true; $('liveBtn').classList.add('on');
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'connecting…';
  const es = new EventSource('/api/stream?file=' + encodeURIComponent(file));
  state.es = es;
  es.onmessage = m => {
    const next = JSON.parse(m.data);
    const fresh = next.events.filter(e => e.seq > state.lastSeq);
    state.lastSeq = next.events.length ? next.events[next.events.length - 1].seq : state.lastSeq;
    state.data = next;
    for (const e of fresh.slice(-12)) heat(e.agent);
    $('liveDot').className = 'dot live'; $('liveLabel').textContent = 'live';
    if (state.live) state.scrub = next.events.length;
    render();
  };
  es.onerror = () => { $('liveDot').className = 'dot'; $('liveLabel').textContent = 'reconnecting…'; };
}

function heat(agentId) {
  if (state.hot.has(agentId)) clearTimeout(state.hot.get(agentId));
  state.hot.set(agentId, setTimeout(() => { state.hot.delete(agentId); render(); }, 2500));
}

// ---------- derived state ----------
function agentStateAt(idx) {
  const stats = new Map();
  for (const a of state.data.agents) {
    stats.set(a.id, { ...a, events: 0, tools: {}, errors: 0, lastTs: null, spawned: a.id === 'main', done: false, recent: [] });
  }
  const evs = state.data.events.slice(0, idx);
  for (const e of evs) {
    const s = stats.get(e.agent); if (!s) continue;
    s.events++; s.lastTs = e.ts; s.recent.push(e.ts);
    if (e.tool) s.tools[e.tool] = (s.tools[e.tool] || 0) + 1;
    if (e.error) s.errors++;
    if (e.kind === 'spawn' && e.spawnedAgent && stats.has(e.spawnedAgent)) stats.get(e.spawnedAgent).spawned = true;
    if (e.kind === 'spawn-result' && e.spawnedAgent && stats.has(e.spawnedAgent)) stats.get(e.spawnedAgent).done = true;
  }
  return [...stats.values()].filter(s => s.spawned || s.events > 0);
}

function statusOf(s) {
  if (s.id !== 'main' && s.done) return 'done';
  const last = s.lastTs ? new Date(s.lastTs).getTime() : 0;
  if (state.live && state.data.now && state.data.now - last < 20000) return 'working';
  if (!state.live && s.events > 0) return 'working';
  return 'idle';
}

const fmtTok = n => n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n || 0);
const fmtUsd = c => c >= 100 ? '$' + Math.round(c) : c >= 1 ? '$' + c.toFixed(2) : '$' + (c || 0).toFixed(3);
const fmtDur = ms => {
  if (!ms || ms < 0) return '—';
  const s = Math.round(ms / 1000);
  return s >= 3600 ? `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m` : s >= 60 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${s}s`;
};

// ---------- render ----------
function setTabs() {
  for (const [btn, v] of [['viewFleet', 'fleet'], ['viewBoard', 'board'], ['viewTimeline', 'timeline']]) {
    $(btn).classList.toggle('on', state.view === v);
  }
  const inSession = state.view !== 'fleet';
  document.querySelector('main').classList.toggle('no-feed', !inSession);
  $('fleet').style.display = inSession ? 'none' : '';
  if (!inSession) { $('board').style.display = 'none'; $('timeline').style.display = 'none'; $('empty').style.display = 'none'; }
  $('feed').style.display = inSession ? '' : 'none';
  document.querySelector('footer').style.display = inSession ? '' : 'none';
  $('statbar').style.display = inSession ? '' : 'none';
  if (!inSession) loadFleet();
}

function render() {
  if (state.view === 'fleet') return;
  renderStatbar();
  if (state.view === 'board') renderBoard(); else renderTimeline();
  renderFeed();
  $('scrub').max = state.data.events.length;
  $('scrub').value = state.scrub;
  $('scrubLabel').textContent = `${state.scrub} / ${state.data.events.length}`;
}

function renderStatbar() {
  const a = state.data.agents;
  const evs = state.data.events;
  const inT = a.reduce((n, x) => n + (x.inTokens || 0) + (x.cacheTokens || 0), 0);
  const outT = a.reduce((n, x) => n + (x.outTokens || 0), 0);
  const cost = a.reduce((n, x) => n + (x.cost || 0), 0);
  const toolCalls = evs.filter(e => e.kind === 'tool-call' || e.kind === 'spawn').length;
  const errs = evs.filter(e => e.error).length;
  const first = evs.find(e => e.ts), last = [...evs].reverse().find(e => e.ts);
  const dur = first && last ? new Date(last.ts) - new Date(first.ts) : 0;
  $('statbar').innerHTML =
    `<span>agents <b>${a.length}</b></span><span>events <b>${evs.length}</b></span>` +
    `<span>tool calls <b>${toolCalls}</b></span><span>duration <b>${fmtDur(dur)}</b></span>` +
    `<span>tokens in <b>${fmtTok(inT)}</b> · out <b>${fmtTok(outT)}</b></span>` +
    `<span>est. cost <b>~${fmtUsd(cost)}</b></span>` +
    (errs ? `<span style="color:var(--red)">errors <b style="color:var(--red)">${errs}</b></span>` : '');
}

function sparkline(recentTs, firstTs, lastTs) {
  if (!recentTs.length || !firstTs || !lastTs) return '';
  const t0 = new Date(firstTs).getTime(), t1 = new Date(lastTs).getTime();
  const span = Math.max(t1 - t0, 1);
  const buckets = new Array(24).fill(0);
  for (const ts of recentTs) {
    if (!ts) continue;
    const i = Math.min(23, Math.floor((new Date(ts).getTime() - t0) / span * 24));
    buckets[i]++;
  }
  const max = Math.max(...buckets, 1);
  return '<div class="spark">' + buckets.map(b => `<i style="height:${Math.round(b / max * 100)}%"></i>`).join('') + '</div>';
}

function renderBoard() {
  $('board').style.display = ''; $('timeline').style.display = 'none';
  const stage = $('stage'), cards = $('cards'), svg = $('edges');
  const W = stage.clientWidth, H = stage.clientHeight;
  const agents = agentStateAt(state.scrub);
  const subs = agents.filter(x => x.id !== 'main');
  $('empty').style.display = agents.length ? 'none' : 'flex';

  const evsAll = state.data.events;
  const firstTs = evsAll.find(e => e.ts)?.ts, lastTs = [...evsAll].reverse().find(e => e.ts)?.ts;

  // Fleet mode: with more than 10 subagents, switch to compact cards laid out
  // column-major in a scrollable grid (workflow runs cluster together).
  const compact = subs.length > 10;
  subs.sort((a, b) => String(a.group || '').localeCompare(String(b.group || '')) || String(a.firstTs).localeCompare(String(b.firstTs)));

  const pos = new Map();
  pos.set('main', { x: Math.min(70, W * 0.05), y: Math.max(20, H / 2 - 90) });
  const cardH = compact ? 92 : 185;
  const cardStep = compact ? 250 : 280;
  const gridX = 420;
  const rows = Math.max(1, Math.floor((H - 30) / cardH));
  subs.forEach((a2, i) => {
    if (compact) pos.set(a2.id, { x: gridX + Math.floor(i / rows) * cardStep, y: 14 + (i % rows) * cardH });
    else {
      const colX = Math.max(W - 340, 400);
      const gap = Math.min(185, Math.max(125, (H - 60) / Math.max(subs.length, 1)));
      const startY = Math.max(14, H / 2 - ((subs.length - 1) * gap) / 2 - 70);
      pos.set(a2.id, { x: colX, y: startY + i * gap });
    }
  });
  const boardW = compact ? gridX + Math.ceil(subs.length / rows) * cardStep + 20 : W;
  $('board').style.width = boardW + 'px';
  $('board').style.height = H + 'px';

  cards.innerHTML = '';
  for (const a of agents) {
    const p = pos.get(a.id);
    const el = document.createElement('div');
    el.className = 'card' + (a.id === 'main' ? ' orchestrator' : '') + (compact && a.id !== 'main' ? ' mini' : '') + (state.hot.has(a.id) ? ' hot' : '');
    el.style.left = p.x + 'px'; el.style.top = p.y + 'px';
    const st = statusOf(a);
    const dur = a.firstTs && a.lastTs ? new Date(a.lastTs) - new Date(a.firstTs) : 0;
    const costTag = a.cost >= 0.005 ? `<span>~<b>${fmtUsd(a.cost)}</b></span>` : '';
    if (compact && a.id !== 'main') {
      el.innerHTML =
        `<h2>🤖 <span class="nm">${esc(a.name || a.id)}</span> <span class="status ${st}">${st}</span></h2>` +
        `<div class="meta"><span>ev <b>${a.events}</b></span><span>out <b>${fmtTok(a.outTokens)}</b></span><span><b>${fmtDur(dur)}</b></span>${costTag}` +
        (a.errors ? `<span class="err">err <b>${a.errors}</b></span>` : '') + `</div>`;
    } else {
      const chips = Object.entries(a.tools).sort((x, y) => y[1] - x[1]).slice(0, 5)
        .map(([n, c]) => `<span class="chip">${esc(n.replace(/^mcp__[^_]+__/, ''))} <b>×${c}</b></span>`).join('');
      el.innerHTML =
        `<h2>${a.id === 'main' ? '🛰️' : '🤖'} <span class="nm">${esc(a.name || a.id)}</span> <span class="status ${st}">${st}</span></h2>` +
        (a.task ? `<div class="task">${esc(a.task)}</div>` : '') +
        `<div class="meta"><span>ev <b>${a.events}</b></span><span>out <b>${fmtTok(a.outTokens)}</b></span><span><b>${fmtDur(dur)}</b></span>${costTag}` +
        (a.errors ? `<span class="err">err <b>${a.errors}</b></span>` : '') + `</div>` +
        sparkline(a.recent, a.firstTs || firstTs, a.lastTs || lastTs) +
        (chips ? `<div class="chips">${chips}</div>` : '');
    }
    cards.appendChild(el);
  }

  svg.setAttribute('viewBox', `0 0 ${boardW} ${H}`);
  svg.style.width = boardW + 'px';
  let paths = '';
  const mp = pos.get('main');
  for (const a of subs) {
    if (compact && !state.hot.has(a.id)) continue; // fleet mode: only draw active edges
    const p = pos.get(a.id);
    const x1 = mp.x + 274, y1 = mp.y + 58, x2 = p.x, y2 = p.y + (compact ? 30 : 48);
    const mx = (x1 + x2) / 2;
    const hot = state.hot.has(a.id);
    const d = `M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`;
    paths += `<path class="edge ${hot ? 'hot' : ''}" d="${d}"/>`;
    if (hot) paths += `<circle class="pulse-dot" r="4"><animateMotion dur="1s" repeatCount="indefinite" path="${d}"/></circle>`;
  }
  svg.innerHTML = paths;
}

function renderTimeline() {
  $('board').style.display = 'none'; $('timeline').style.display = '';
  const agents = agentStateAt(state.data.events.length);
  const evs = state.data.events.filter(e => e.ts);
  if (!evs.length) { $('timeline').innerHTML = ''; return; }
  const t0 = new Date(evs[0].ts).getTime();
  const t1 = new Date(evs[evs.length - 1].ts).getTime();
  const span = Math.max(t1 - t0, 1000);
  const W = Math.max($('stage').clientWidth - 40, 600), LABEL = 170, ROW = 44;
  const plotW = W - LABEL - 20;
  const H = agents.length * ROW + 46;
  const x = ts => LABEL + (new Date(ts).getTime() - t0) / span * plotW;

  let g = '';
  agents.forEach((a, i) => {
    const y = 26 + i * ROW;
    g += `<text class="lane-label" x="8" y="${y + ROW / 2 + 4}">${esc((a.name || a.id).slice(0, 24))}</text>`;
    g += `<line class="lane-line" x1="${LABEL}" y1="${y + ROW}" x2="${W - 10}" y2="${y + ROW}"/>`;
  });
  const laneIdx = new Map(agents.map((a, i) => [a.id, i]));
  const stride = Math.max(1, Math.ceil(evs.length / 6000)); // cap DOM nodes on huge sessions
  for (const e of evs.filter((_, i) => i % stride === 0)) {
    const li = laneIdx.get(e.agent);
    if (li === undefined) continue;
    const y = 26 + li * ROW + 8;
    const col = e.error ? '#f87171' : (KIND_COLOR[e.kind] || '#8a93a8');
    // true duration bar when the tool result timestamp is known; instant tick otherwise
    const x0 = x(e.ts);
    const w = e.endTs ? Math.max(4, x(e.endTs) - x0) : 5;
    const durTip = e.endTs ? ` (${fmtDur(new Date(e.endTs) - new Date(e.ts))})` : '';
    g += `<rect class="tl-block" data-seq="${e.seq}" x="${x0.toFixed(1)}" y="${y}" width="${w.toFixed(1)}" height="${ROW - 16}" fill="${col}" opacity="${e.seq < state.scrub ? (e.endTs ? .75 : .95) : .25}"><title>${esc(KIND_LABEL[e.kind] || e.kind)}${e.tool ? ': ' + esc(e.tool) : ''}${durTip}\n${esc((e.text || '').slice(0, 120))}</title></rect>`;
  }
  for (let i = 0; i <= 4; i++) {
    const tx = LABEL + plotW * i / 4;
    g += `<text class="tl-axis" x="${tx}" y="${H - 6}">${new Date(t0 + span * i / 4).toLocaleTimeString()}</text>`;
  }
  const cur = state.data.events[Math.min(state.scrub, state.data.events.length - 1)];
  if (cur && cur.ts) g += `<line class="tl-cursor" x1="${x(cur.ts)}" y1="16" x2="${x(cur.ts)}" y2="${H - 20}"/>`;

  $('timeline').innerHTML = `<svg id="tl-svg" width="${W}" height="${H}">${g}</svg>`;
  $('timeline').querySelectorAll('.tl-block').forEach(r => {
    r.onclick = () => { seek(Number(r.dataset.seq) + 1); };
  });
}

function renderFeed() {
  const evEl = $('events');
  const atBottom = evEl.scrollHeight - evEl.scrollTop - evEl.clientHeight < 60;
  const ft = state.filterText.toLowerCase();
  const shown = state.data.events.slice(0, state.scrub)
    .filter(e => state.kindsOn.has(e.kind))
    .filter(e => !ft || (e.text || '').toLowerCase().includes(ft) || (e.tool || '').toLowerCase().includes(ft))
    .slice(-250);
  evEl.innerHTML = shown.map(e => {
    const who = e.agent === 'main' ? 'orchestrator' : (state.data.agents.find(a => a.id === e.agent)?.name || 'subagent');
    const t = e.ts ? new Date(e.ts).toLocaleTimeString() : '';
    const body = (e.kind === 'tool-call' || e.kind === 'spawn')
      ? `<span class="tool">${esc(e.tool)}</span> <span class="txt">${esc(e.text || '')}</span>`
      : `<span class="txt">${esc(e.text || '')}</span>`;
    return `<div class="ev k-${e.kind}${e.error ? ' err' : ''}" data-seq="${e.seq}"><span class="who">${esc(who)}</span><span class="t">${t}</span><br>${body}</div>`;
  }).join('');
  evEl.querySelectorAll('.ev').forEach(el => { el.onclick = () => openDrawer(Number(el.dataset.seq)); });
  if (atBottom || state.live) evEl.scrollTop = evEl.scrollHeight;
}

// ---------- inspector ----------
function openDrawer(seq) {
  const e = state.data.events.find(x => x.seq === seq);
  if (!e) return;
  const who = e.agent === 'main' ? 'Orchestrator' : (state.data.agents.find(a => a.id === e.agent)?.name || 'subagent');
  const dur = e.endTs ? ` · took ${fmtDur(new Date(e.endTs) - new Date(e.ts))}` : '';
  $('d-title').textContent = `${KIND_LABEL[e.kind] || e.kind}${e.tool ? ' · ' + e.tool : ''}`;
  $('d-meta').textContent = `${who} · ${e.ts ? new Date(e.ts).toLocaleString() : 'no timestamp'} · event #${e.seq}${dur}${e.error ? ' · ERROR' : ''}`;
  $('d-body').textContent = e.full || e.text || '(empty)';
  $('drawer').classList.add('open');
}
$('d-close').onclick = () => $('drawer').classList.remove('open');
document.addEventListener('keydown', ev => { if (ev.key === 'Escape') $('drawer').classList.remove('open'); });

// ---------- playback & controls ----------
function seek(idx) {
  state.scrub = Math.max(0, Math.min(idx, state.data.events.length));
  state.live = false; $('liveBtn').classList.remove('on');
  render();
}
function stopPlay() {
  state.playing = false;
  if (state.playTimer) clearInterval(state.playTimer);
  $('playBtn').textContent = '▶';
}
$('playBtn').onclick = () => {
  if (state.playing) { stopPlay(); return; }
  if (state.scrub >= state.data.events.length) state.scrub = 0;
  state.playing = true; state.live = false;
  $('liveBtn').classList.remove('on');
  $('playBtn').textContent = '⏸';
  state.playTimer = setInterval(() => {
    state.scrub = Math.min(state.scrub + state.speed, state.data.events.length);
    const e = state.data.events[state.scrub - 1];
    if (e) heat(e.agent);
    render();
    if (state.scrub >= state.data.events.length) stopPlay();
  }, 180);
};
$('speed').onchange = () => { state.speed = Number($('speed').value); };
$('scrub').oninput = () => { stopPlay(); seek(Number($('scrub').value)); };
$('liveBtn').onclick = () => {
  if (BAKED) return;
  stopPlay();
  state.live = true; $('liveBtn').classList.add('on');
  state.scrub = state.data.events.length; render();
};
$('viewFleet').onclick = () => { if (!BAKED) { state.view = 'fleet'; setTabs(); } };
$('viewBoard').onclick = () => { if (state.data.events.length || state.file) { state.view = 'board'; setTabs(); render(); } };
$('viewTimeline').onclick = () => { if (state.data.events.length || state.file) { state.view = 'timeline'; setTabs(); render(); } };
$('exportBtn').onclick = () => {
  if (!state.file) return;
  const title = $('sessionSel').selectedOptions[0]?.textContent.split(' — ')[0] || '';
  location.href = '/api/export?file=' + encodeURIComponent(state.file) + '&title=' + encodeURIComponent(title);
};
$('filterText').oninput = () => { state.filterText = $('filterText').value; renderFeed(); };

const kt = $('kindToggles');
for (const k of KINDS) {
  const b = document.createElement('button');
  b.textContent = KIND_LABEL[k]; b.className = 'on';
  b.onclick = () => {
    if (state.kindsOn.has(k)) { state.kindsOn.delete(k); b.classList.remove('on'); }
    else { state.kindsOn.add(k); b.classList.add('on'); }
    renderFeed();
  };
  kt.appendChild(b);
}

let resizeT = null;
window.onresize = () => { clearTimeout(resizeT); resizeT = setTimeout(() => { if (state.view !== 'fleet') render(); }, 120); };

// ---------- boot ----------
if (BAKED) {
  // standalone replay: no server, no live mode
  state.data = BAKED.data;
  state.scrub = state.data.events.length;
  document.title = 'Replay — ' + BAKED.title;
  $('sessionSel').style.display = 'none';
  $('viewFleet').style.display = 'none';
  $('exportBtn').style.display = 'none';
  $('liveBtn').style.display = 'none';
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'replay';
  setTabs(); render();
} else {
  loadSessions();
  setTabs();
}
