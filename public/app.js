// Agent Mission Control v2.0 — frontend
const $ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

const BAKED = window.__BAKED__ || null; // standalone replay export mode

// agent-type identity: color + label, used across cards, table, and constellation
const AGENT_KIND = {
  claude: { label: 'Claude', color: '#fb7185' },   // coral
  codex: { label: 'Codex', color: '#60a5fa' },     // blue
  otel: { label: 'OTLP', color: '#c084fc' },        // violet
};
const kindColor = k => (AGENT_KIND[k] || AGENT_KIND.claude).color;

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

// ---------- session/project metadata ----------
let metaMap = {};        // stableKey -> {projectId, archived, pinned, tags, note}
let metaProjects = [];
let metaTags = [];
let metaCsrf = null;
let metaVersion = -1;
let metaReadOnly = false;
async function loadMeta() {
  try {
    const m = await (await fetch('/api/meta')).json();
    metaMap = m.sessions || {}; metaProjects = m.projects || []; metaTags = m.tags || [];
    metaCsrf = m.csrf; metaVersion = m.metaVersion; metaReadOnly = !!m.readOnly;
  } catch { /* first load may race boot */ }
}
function metaOf(s) { return (s && s.stableKey && metaMap[s.stableKey]) || {}; }
function projectById(id) { return metaProjects.find(p => p.id === id); }
async function metaPost(path, body) {
  if (metaReadOnly) { alert('Metadata is read-only (recovered from a corrupt state file).'); return null; }
  const r = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf },
    body: JSON.stringify({ baseVersion: metaVersion, ...body }),
  });
  if (r.status === 409) { await loadMeta(); return metaPost(path, body); } // stale → refetch + retry once
  if (!r.ok) { console.warn('meta write failed', path, r.status); return null; }
  await loadMeta();
  refreshOverview();
  return r.json().catch(() => ({}));
}
function refreshOverview() {
  if (state.view === 'fleet') renderFleet();
  else if (state.view === 'table') renderTable();
  else if (state.view === 'projects') renderProjects();
}
async function setSessionMeta(stableKey, patch) { if (stableKey) return metaPost('/api/meta/session', { stableKey, patch }); }

// ---------- notifications ----------
// A background poll of /api/fleet detects newly-errored sessions and runs that
// finished (were active, now idle > N min). Fires desktop notifications + a bell.
const notifs = [];
const notifSeen = new Map(); // file -> {errors, lastMtime, wasActive}
let notifTimer = null;
function startNotifications() {
  if (notifTimer || BAKED) return;
  // seed baseline silently so we don't alert on the whole backlog at startup
  fetch('/api/fleet').then(r => r.json()).then(fleet => {
    for (const s of fleet) notifSeen.set(s.file, { errors: s.errors, mtime: s.mtime, active: Date.now() - s.mtime < 6e5 });
    if ('Notification' in window && Notification.permission === 'default') Notification.requestPermission();
    notifTimer = setInterval(pollNotifications, 15000);
  });
}
async function pollNotifications() {
  let fleet;
  try { fleet = await (await fetch('/api/fleet')).json(); } catch { return; }
  for (const s of fleet) {
    const prev = notifSeen.get(s.file);
    if (!prev) { notifSeen.set(s.file, { errors: s.errors, mtime: s.mtime, active: Date.now() - s.mtime < 6e5 }); continue; }
    if (s.errors > prev.errors) {
      pushNotif('error', `${s.errors - prev.errors} new error${s.errors - prev.errors > 1 ? 's' : ''}`, s);
    }
    const nowActive = Date.now() - s.mtime < 6e5;
    if (prev.active && !nowActive && s.durationMs > 6e5) {
      pushNotif('done', `run finished · ${s.agents} agents · ~${fmtUsd(s.cost)}`, s);
    }
    notifSeen.set(s.file, { errors: s.errors, mtime: s.mtime, active: nowActive });
  }
}
function pushNotif(type, msg, s) {
  const n = { type, msg, title: s.title || s.session.slice(0, 8), file: s.file, machine: s.machine, kind: s.kind, at: Date.now() };
  notifs.unshift(n);
  if (notifs.length > 40) notifs.pop();
  renderBell();
  if ('Notification' in window && Notification.permission === 'granted') {
    try { new Notification(`${type === 'error' ? '⚠️' : '✅'} ${n.title}`, { body: `${msg} · ${n.machine || ''}`, silent: type !== 'error' }); } catch { /* blocked */ }
  }
}
function renderBell() {
  const unread = notifs.filter(n => !n.read).length;
  const bc = $('bellCount');
  bc.style.display = unread ? '' : 'none'; bc.textContent = unread;
  $('bell').classList.toggle('has', unread > 0);
  $('notifList').innerHTML = notifs.length ? notifs.map(n => `
    <div class="notif ${n.type}" data-file="${esc(n.file)}">
      <span class="ni">${n.type === 'error' ? '⚠️' : '✅'}</span>
      <div><div class="nt">${esc(n.title)}</div><div class="nm">${esc(n.msg)} · ${esc(n.machine || '')}</div></div>
      <span class="ndot" style="background:${kindColor(n.kind)}"></span>
    </div>`).join('') : '<div class="notif-empty">No alerts yet. Errors and finished long runs show here.</div>';
  $('notifList').querySelectorAll('.notif[data-file]').forEach(el => el.onclick = () => { $('notifPanel').classList.remove('open'); openSession(el.dataset.file); });
}
$('bell').onclick = () => { $('notifPanel').classList.toggle('open'); notifs.forEach(n => n.read = true); renderBell(); };
$('notifClear').onclick = (e) => { e.stopPropagation(); notifs.length = 0; renderBell(); };
document.addEventListener('click', e => { if (!$('notifPanel').contains(e.target) && e.target !== $('bell') && !$('bell').contains(e.target)) $('notifPanel').classList.remove('open'); });

let fleetCache = null;
async function loadFleet() {
  if (!fleetCache) $('fleet').innerHTML = '<div class="fleet-loading">Scanning sessions…</div>';
  fleetCache = await (await fetch('/api/fleet')).json();
  renderFleet();
}

let fleetFilter = '';
function renderFleet() {
  const fleet = fleetCache || [];
  const shown = filteredFleet();
  const totCost = shown.reduce((n, s) => n + s.cost, 0);
  const totAgents = shown.reduce((n, s) => n + s.agents, 0);
  $('fleet').innerHTML =
    fleetControls(shown.length, fleet.length, totAgents, totCost) +
    `<div class="fleet-grid">` + shown.map(s => {
      const col = kindColor(s.kind);
      const m = metaOf(s);
      return `
      <div class="fcard${m.archived ? ' is-archived' : ''}" data-file="${esc(s.file)}" data-sk="${esc(s.stableKey || '')}" draggable="true" style="border-left:3px solid ${col}">
        ${s.stableKey ? `<div class="fcard-actions"><button class="fcard-arch" title="${m.archived ? 'unarchive' : 'archive'}">${m.archived ? '⤴' : '🗄'}</button><button class="fcard-menu" title="organize">⋯</button></div>` : ''}
        <h3>${esc(s.title || s.session.slice(0, 8))}</h3>
        <div class="fproj"><span class="kind-badge" style="background:${col}22;color:${col}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span> ${esc(s.machine || '')} · ${esc(s.project.replace(/^[Cc⇄]+\s?[·]?\s?/, '').replace(/^[Cc]--Users-[^-]+-/, ''))}</div>
        ${cardBadges(s) ? `<div class="fbadges">${cardBadges(s)}</div>` : ''}
        <div class="fstats">
          <span><b>${s.agents}</b> agents</span><span><b>${s.events}</b> events</span>
          <span><b>${s.toolCalls}</b> tools</span><span><b>${fmtDur(s.durationMs)}</b></span>
          <span><b>${fmtTok(s.tokensIn)}</b> in</span><span><b>${fmtTok(s.tokensOut)}</b> out</span>
          <span class="fcost"><b>~${fmtUsd(s.cost)}</b></span>
          ${s.errors ? `<span class="ferr"><b>${s.errors}</b> errors</span>` : ''}
        </div>
        <div class="fdate"><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span> · ${new Date(s.mtime).toLocaleString()}</div>
      </div>`;
    }).join('') + `</div>`;
  $('fleet').querySelectorAll('.fcard').forEach(c => {
    const s = shown.find(x => x.file === c.dataset.file);
    c.onclick = () => openSession(c.dataset.file);
    const menu = c.querySelector('.fcard-menu');
    if (menu && s) menu.onclick = e => showCardMenu(e, s);
    const arch = c.querySelector('.fcard-arch');
    if (arch && s) arch.onclick = e => { e.stopPropagation(); setSessionMeta(s.stableKey, { archived: !metaOf(s).archived }); };
    if (s && s.stableKey) c.ondragstart = e => e.dataTransfer.setData('text/plain', s.stableKey);
  });
  wireFleetControls(renderFleet);
}

// ---------- shared fleet filtering (used by grid + table) ----------
let fleetKind = 'all', fleetMachine = 'all', fleetArchived = 'hide', fleetProject = 'all';
function filteredFleet() {
  const q = fleetFilter.toLowerCase();
  return (fleetCache || []).filter(s => {
    const m = metaOf(s);
    if (fleetArchived === 'hide' && m.archived) return false;
    if (fleetArchived === 'only' && !m.archived) return false;
    if (fleetProject === 'unassigned' && m.projectId) return false;
    if (fleetProject !== 'all' && fleetProject !== 'unassigned' && m.projectId !== fleetProject) return false;
    if (fleetKind !== 'all' && s.kind !== fleetKind) return false;
    if (fleetMachine !== 'all' && s.machine !== fleetMachine) return false;
    if (q && !((s.title || '') + ' ' + s.project + ' ' + s.machine + ' ' + s.session + ' ' + (m.note || '')).toLowerCase().includes(q)) return false;
    return true;
  }).sort((a, b) => (metaOf(b).pinned ? 1 : 0) - (metaOf(a).pinned ? 1 : 0)); // pinned first
}
function fleetControls(shownN, totalN, agents, cost) {
  const machinesInFleet = [...new Set((fleetCache || []).map(s => s.machine).filter(Boolean))];
  const kinds = ['all', 'claude', 'codex', 'otel'];
  const arch = [['hide', 'Active'], ['only', 'Archived'], ['all', 'All']];
  return `<div class="fleet-head">
    <h2>${shownN}${shownN !== totalN ? '/' + totalN : ''} sessions · ${agents} agents · ~${fmtUsd(cost)}</h2>
    <input id="fleetSearch" type="text" placeholder="search… title, machine, note" value="${esc(fleetFilter)}">
    <div class="seg" id="kindSeg">${kinds.map(k => `<button data-k="${k}" class="${fleetKind === k ? 'on' : ''}" ${k !== 'all' ? `style="--c:${kindColor(k)}"` : ''}>${k === 'all' ? 'All' : AGENT_KIND[k].label}</button>`).join('')}</div>
    <div class="seg" id="archSeg">${arch.map(([v, l]) => `<button data-a="${v}" class="${fleetArchived === v ? 'on' : ''}">${l}</button>`).join('')}</div>
    <select id="projSel"><option value="all">all projects</option><option value="unassigned" ${fleetProject === 'unassigned' ? 'selected' : ''}>unassigned</option>${metaProjects.map(p => `<option value="${esc(p.id)}" ${fleetProject === p.id ? 'selected' : ''}>${esc(p.name)}</option>`).join('')}</select>
    <select id="machineSel"><option value="all">all machines</option>${machinesInFleet.map(m => `<option value="${esc(m)}" ${fleetMachine === m ? 'selected' : ''}>${esc(m)}</option>`).join('')}</select>
  </div>`;
}
function wireFleetControls(rerender) {
  const search = $('fleetSearch');
  if (search) search.oninput = () => { fleetFilter = search.value; const p = search.selectionStart; rerender(); const s2 = $('fleetSearch'); if (s2) { s2.focus(); s2.setSelectionRange(p, p); } };
  $('kindSeg')?.querySelectorAll('button').forEach(b => { b.onclick = () => { fleetKind = b.dataset.k; rerender(); }; });
  $('archSeg')?.querySelectorAll('button').forEach(b => { b.onclick = () => { fleetArchived = b.dataset.a; rerender(); }; });
  const ps = $('projSel'); if (ps) ps.onchange = () => { fleetProject = ps.value; rerender(); };
  const ms = $('machineSel'); if (ms) ms.onchange = () => { fleetMachine = ms.value; rerender(); };
}

// ---------- card edit menu popover ----------
let openPopover = null;
function closePopover() { if (openPopover) { openPopover.remove(); openPopover = null; } }
document.addEventListener('click', closePopover);
function showCardMenu(ev, s) {
  ev.stopPropagation(); closePopover();
  const m = metaOf(s);
  const pop = document.createElement('div');
  pop.className = 'card-pop';
  pop.onclick = e => e.stopPropagation();
  const projOpts = metaProjects.map(p => `<button data-proj="${esc(p.id)}"><span class="pdot" style="background:${p.color}"></span>${esc(p.name)}${m.projectId === p.id ? ' ✓' : ''}</button>`).join('');
  pop.innerHTML = `
    <div class="pop-sec">Project</div>
    ${projOpts || '<div class="pop-empty">no projects yet</div>'}
    <button data-proj="">— none —</button>
    <div class="pop-div"></div>
    <button data-act="pin">${m.pinned ? '★ Unpin' : '☆ Pin'}</button>
    <button data-act="archive">${m.archived ? '⌃ Unarchive' : '⌄ Archive'}</button>
    <button data-act="note">✎ ${m.note ? 'Edit note' : 'Add note'}</button>`;
  document.body.appendChild(pop);
  const r = ev.currentTarget.getBoundingClientRect();
  pop.style.left = Math.min(r.left, window.innerWidth - 220) + 'px';
  pop.style.top = (r.bottom + 4) + 'px';
  openPopover = pop;
  pop.querySelectorAll('[data-proj]').forEach(b => b.onclick = () => { closePopover(); setSessionMeta(s.stableKey, { projectId: b.dataset.proj || null }); });
  pop.querySelector('[data-act="pin"]').onclick = () => { closePopover(); setSessionMeta(s.stableKey, { pinned: !m.pinned }); };
  pop.querySelector('[data-act="archive"]').onclick = () => { closePopover(); setSessionMeta(s.stableKey, { archived: !m.archived }); };
  pop.querySelector('[data-act="note"]').onclick = () => { closePopover(); const n = prompt('Note for this session:', m.note || ''); if (n !== null) setSessionMeta(s.stableKey, { note: n }); };
}
function cardBadges(s) {
  const m = metaOf(s); const p = projectById(m.projectId);
  return (m.pinned ? '<span class="mini-badge pin">★</span>' : '') +
    (p ? `<span class="mini-badge" style="background:${p.color}22;color:${p.color}">${esc(p.name)}</span>` : '') +
    (m.archived ? '<span class="mini-badge arch">archived</span>' : '') +
    (m.note ? `<span class="mini-badge note" title="${esc(m.note)}">✎</span>` : '');
}

// ---------- TABLE view ----------
let tableSort = { col: 'mtime', dir: -1 };
async function loadTable() {
  if (!fleetCache) { $('tableView').innerHTML = '<div class="fleet-loading">Scanning…</div>'; fleetCache = await (await fetch('/api/fleet')).json(); }
  renderTable();
}
function renderTable() {
  const cols = [
    { k: 'title', label: 'Session', num: false },
    { k: 'kind', label: 'Agent', num: false },
    { k: 'machine', label: 'Machine', num: false },
    { k: 'agents', label: 'Agents', num: true },
    { k: 'events', label: 'Events', num: true },
    { k: 'toolCalls', label: 'Tools', num: true },
    { k: 'durationMs', label: 'Duration', num: true },
    { k: 'tokensOut', label: 'Out tok', num: true },
    { k: 'cost', label: 'Cost', num: true },
    { k: 'errors', label: 'Err', num: true },
    { k: 'mtime', label: 'When', num: true },
  ];
  let rows = filteredFleet().slice();
  const { col, dir } = tableSort;
  rows.sort((a, b) => {
    const av = a[col], bv = b[col];
    if (typeof av === 'number') return (av - bv) * dir;
    return String(av || '').localeCompare(String(bv || '')) * dir;
  });
  const totCost = rows.reduce((n, s) => n + s.cost, 0), totAgents = rows.reduce((n, s) => n + s.agents, 0);
  $('tableView').innerHTML =
    fleetControls(rows.length, (fleetCache || []).length, totAgents, totCost) +
    `<div class="table-wrap"><table class="ftable"><thead><tr>` +
    cols.map(c => `<th data-k="${c.k}" class="${c.num ? 'num' : ''} ${col === c.k ? 'sorted' : ''}">${c.label}${col === c.k ? (dir < 0 ? ' ▼' : ' ▲') : ''}</th>`).join('') +
    `</tr></thead><tbody>` +
    rows.map(s => {
      const c = kindColor(s.kind);
      return `<tr data-file="${esc(s.file)}">
        <td class="tsess">${esc(s.title || s.session.slice(0, 8))}</td>
        <td><span class="kind-badge" style="background:${c}22;color:${c}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span></td>
        <td>${esc(s.machine || '')}</td>
        <td class="num">${s.agents}</td><td class="num">${s.events}</td><td class="num">${s.toolCalls}</td>
        <td class="num">${fmtDur(s.durationMs)}</td><td class="num">${fmtTok(s.tokensOut)}</td>
        <td class="num fcost">~${fmtUsd(s.cost)}</td><td class="num ${s.errors ? 'ferr' : ''}">${s.errors || ''}</td>
        <td class="num tdate"><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span></td>
      </tr>`;
    }).join('') + `</tbody></table></div>`;
  $('tableView').querySelectorAll('th').forEach(th => { th.onclick = () => { const k = th.dataset.k; tableSort = { col: k, dir: tableSort.col === k ? -tableSort.dir : (cols.find(c => c.k === k).num ? -1 : 1) }; renderTable(); }; });
  $('tableView').querySelectorAll('tr[data-file]').forEach(tr => { tr.onclick = () => openSession(tr.dataset.file); });
  wireFleetControls(renderTable);
}

// ---------- PROJECTS view (drag sessions into colored columns) ----------
async function loadProjects() {
  await loadMeta();
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  renderProjects();
}
function renderProjects() {
  const sessions = (fleetCache || []).filter(s => !metaOf(s).archived);
  const cols = [
    ...metaProjects.map(p => ({ id: p.id, name: p.name, color: p.color, sessions: sessions.filter(s => metaOf(s).projectId === p.id) })),
    { id: null, name: 'Unassigned', color: '#8a93a8', sessions: sessions.filter(s => !metaOf(s).projectId) },
  ];
  $('projects').innerHTML =
    `<div class="fleet-head"><h2>Projects — ${metaProjects.length}</h2>
      <button id="newProjBtn" class="mini-btn">+ New project</button>
      ${metaReadOnly ? '<span class="ro-warn">metadata read-only (corrupt state recovered)</span>' : ''}</div>` +
    `<div class="proj-board">` + cols.map(c => `
      <div class="proj-col" data-proj="${esc(c.id || '')}" style="--pc:${c.color}">
        <div class="proj-col-head"><span class="pdot" style="background:${c.color}"></span><span class="pcn">${esc(c.name)}</span><span class="pcc">${c.sessions.length}</span>
          ${c.id ? `<button class="proj-edit" data-proj="${esc(c.id)}" title="edit">⋯</button>` : ''}</div>
        <div class="proj-drop">${c.sessions.map(s => `
          <div class="pchip" draggable="true" data-sk="${esc(s.stableKey || '')}" data-file="${esc(s.file)}" style="border-left:3px solid ${kindColor(s.kind)}">
            <div class="pchip-t">${esc(s.title || s.session.slice(0, 8))}</div>
            <div class="pchip-m">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label} · ${esc(s.machine || '')} · ~${fmtUsd(s.cost)}</div>
          </div>`).join('') || '<div class="proj-empty">drop sessions here</div>'}</div>
      </div>`).join('') + `</div>`;
  // wire drag + drop
  $('projects').querySelectorAll('.pchip').forEach(ch => {
    ch.ondragstart = e => { e.dataTransfer.setData('text/plain', ch.dataset.sk); e.stopPropagation(); };
    ch.onclick = () => openSession(ch.dataset.file);
  });
  $('projects').querySelectorAll('.proj-col').forEach(col => {
    col.ondragover = e => { e.preventDefault(); col.classList.add('drag-over'); };
    col.ondragleave = () => col.classList.remove('drag-over');
    col.ondrop = e => {
      e.preventDefault(); col.classList.remove('drag-over');
      const sk = e.dataTransfer.getData('text/plain');
      if (sk) setSessionMeta(sk, { projectId: col.dataset.proj || null });
    };
  });
  $('newProjBtn').onclick = async () => {
    const name = prompt('New project name:'); if (!name) return;
    const color = PROJ_COLORS[metaProjects.length % PROJ_COLORS.length];
    await metaPost('/api/meta/project', { op: 'create', name, color });
  };
  $('projects').querySelectorAll('.proj-edit').forEach(b => b.onclick = async (e) => {
    e.stopPropagation();
    const p = projectById(b.dataset.proj); if (!p) return;
    const name = prompt('Rename project (blank = delete):', p.name);
    if (name === null) return;
    if (name === '') { if (confirm(`Delete "${p.name}"? Its ${(fleetCache || []).filter(s => metaOf(s).projectId === p.id).length} sessions become unassigned.`)) await metaPost('/api/meta/project', { op: 'delete', id: p.id }); return; }
    await metaPost('/api/meta/project', { op: 'update', id: p.id, name });
  });
}
const PROJ_COLORS = ['#fb7185', '#60a5fa', '#c084fc', '#34d399', '#fbbf24', '#f472b6', '#22d3ee', '#a3e635'];

// ---------- MACHINES view ----------
async function loadMachines() {
  const [machinesData, fleet] = await Promise.all([
    fetch('/api/machines').then(r => r.json()),
    fleetCache ? Promise.resolve(fleetCache) : fetch('/api/fleet').then(r => r.json()),
  ]);
  fleetCache = fleet;
  const byMachine = {};
  for (const s of fleet) {
    const m = s.machine || 'unknown';
    (byMachine[m] = byMachine[m] || { sessions: 0, agents: 0, cost: 0, kinds: {}, lastMs: 0 });
    byMachine[m].sessions++; byMachine[m].agents += s.agents; byMachine[m].cost += s.cost;
    byMachine[m].kinds[s.kind] = (byMachine[m].kinds[s.kind] || 0) + 1;
    byMachine[m].lastMs = Math.max(byMachine[m].lastMs, s.mtime);
  }
  const known = new Set(machinesData.map(m => m.name));
  for (const m of Object.keys(byMachine)) if (!known.has(m)) machinesData.push({ name: m, ips: [], lastSeen: byMachine[m].lastMs, remote: true });
  $('machines').innerHTML =
    `<div class="fleet-head"><h2>Machines — ${machinesData.length}</h2></div>` +
    `<div class="machine-grid">` + machinesData.map(m => {
      const st = byMachine[m.name] || { sessions: 0, agents: 0, cost: 0, kinds: {} };
      const fresh = Date.now() - m.lastSeen < 120000;
      const kindDots = Object.entries(st.kinds).map(([k, n]) => `<span class="mkind" style="color:${kindColor(k)}">● ${(AGENT_KIND[k] || AGENT_KIND.claude).label} ${n}</span>`).join('');
      return `<div class="mcard ${fresh ? 'fresh' : ''}">
        <h3>${m.remote ? '⇄' : '★'} ${esc(m.name)} <span class="mstatus ${fresh ? 'on' : ''}">${fresh ? 'live' : 'idle'}</span></h3>
        <div class="mips">${(m.ips || []).map(ip => `<span class="ip">${esc(ip)}</span>`).join('') || '<span class="ip dim">no IPs reported</span>'}</div>
        <div class="mstats"><span><b>${st.sessions}</b> sessions</span><span><b>${st.agents}</b> agents</span><span class="fcost"><b>~${fmtUsd(st.cost)}</b></span></div>
        <div class="mkinds">${kindDots}</div>
        <div class="fdate">last seen ${new Date(m.lastSeen).toLocaleString()}</div>
      </div>`;
    }).join('') + `</div>`;
}

// ---------- CONSTELLATION view (force-directed galaxy) ----------
let constAnim = null;
function stopConstellation() { if (constAnim) { cancelAnimationFrame(constAnim); constAnim = null; } }
async function loadConstellation() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  const cv = $('constCanvas'), wrap = $('constellation');
  const DPR = window.devicePixelRatio || 1;
  const W = wrap.clientWidth, H = wrap.clientHeight;
  cv.width = W * DPR; cv.height = H * DPR; cv.style.width = W + 'px'; cv.style.height = H + 'px';
  const ctx = cv.getContext('2d'); ctx.scale(DPR, DPR);

  // nodes: one sun per machine, one star per session (bound to its machine)
  const machineNames = [...new Set(fleetCache.map(s => s.machine || 'local'))];
  const suns = new Map();
  machineNames.forEach((m, i) => {
    const ang = (i / machineNames.length) * Math.PI * 2;
    suns.set(m, { id: 'm:' + m, machine: m, sun: true, x: W / 2 + Math.cos(ang) * 180, y: H / 2 + Math.sin(ang) * 140, vx: 0, vy: 0, r: 16, label: m });
  });
  const maxCost = Math.max(...fleetCache.map(s => s.cost), 1);
  const stars = fleetCache.map(s => {
    const sun = suns.get(s.machine || 'local');
    return {
      id: s.file, sun: false, file: s.file, machine: s.machine,
      x: sun.x + (Math.random() - 0.5) * 120, y: sun.y + (Math.random() - 0.5) * 120, vx: 0, vy: 0,
      r: 3 + Math.sqrt(s.cost / maxCost) * 14, color: kindColor(s.kind), sunNode: sun,
      active: Date.now() - s.mtime < 6 * 3600e3, title: s.title || s.session.slice(0, 8), kind: s.kind,
    };
  });
  const nodes = [...suns.values(), ...stars];

  let tx = 0, ty = 0, scale = 1, dragging = null, hover = null, panning = false, lastM = null;
  cv.onwheel = e => { e.preventDefault(); const f = e.deltaY < 0 ? 1.1 : 0.9; scale = Math.max(0.3, Math.min(4, scale * f)); };
  cv.onmousedown = e => {
    const p = toWorld(e); const n = pick(p);
    if (n && !n.sun) dragging = n; else { panning = true; lastM = { x: e.clientX, y: e.clientY }; }
  };
  cv.onmousemove = e => {
    const p = toWorld(e); hover = pick(p);
    cv.style.cursor = hover ? 'pointer' : (panning ? 'grabbing' : 'grab');
    if (dragging) { dragging.x = p.x; dragging.y = p.y; dragging.vx = dragging.vy = 0; }
    else if (panning) { tx += e.clientX - lastM.x; ty += e.clientY - lastM.y; lastM = { x: e.clientX, y: e.clientY }; }
  };
  cv.onmouseup = e => {
    if (!dragging && !panning) { const n = pick(toWorld(e)); if (n && !n.sun) openSession(n.file); }
    dragging = null; panning = false;
  };
  function toWorld(e) { const r = cv.getBoundingClientRect(); return { x: (e.clientX - r.left - tx) / scale, y: (e.clientY - r.top - ty) / scale }; }
  function pick(p) { let best = null, bd = 1e9; for (const n of nodes) { const d = Math.hypot(n.x - p.x, n.y - p.y); if (d < n.r + 6 && d < bd) { bd = d; best = n; } } return best; }

  function step() {
    // repulsion between all, spring stars to their sun, mild gravity to center
    for (let i = 0; i < nodes.length; i++) {
      const a = nodes[i];
      if (a === dragging) continue;
      for (let j = i + 1; j < nodes.length; j++) {
        const b = nodes[j];
        let dx = a.x - b.x, dy = a.y - b.y; let d2 = dx * dx + dy * dy || 1;
        if (d2 < 40000) { const f = (a.sun || b.sun ? 900 : 260) / d2; const d = Math.sqrt(d2); a.vx += dx / d * f; a.vy += dy / d * f; b.vx -= dx / d * f; b.vy -= dy / d * f; }
      }
      if (!a.sun) { const s = a.sunNode; const dx = s.x - a.x, dy = s.y - a.y, d = Math.hypot(dx, dy) || 1; const f = (d - 90) * 0.006; a.vx += dx / d * f; a.vy += dy / d * f; }
      a.vx += (W / 2 - a.x) * 0.0008; a.vy += (H / 2 - a.y) * 0.0008;
    }
    for (const n of nodes) { if (n === dragging) continue; n.vx *= 0.85; n.vy *= 0.85; n.x += n.vx; n.y += n.vy; }
  }
  let t = 0;
  function draw() {
    step(); t++;
    ctx.setTransform(DPR, 0, 0, DPR, 0, 0);
    ctx.fillStyle = '#080b11'; ctx.fillRect(0, 0, W, H);
    ctx.setTransform(DPR * scale, 0, 0, DPR * scale, tx * DPR, ty * DPR);
    // edges
    ctx.lineWidth = 0.6 / scale;
    for (const s of stars) { ctx.strokeStyle = s.color + '22'; ctx.beginPath(); ctx.moveTo(s.x, s.y); ctx.lineTo(s.sunNode.x, s.sunNode.y); ctx.stroke(); }
    // suns
    for (const m of suns.values()) {
      ctx.fillStyle = '#e5e9f0'; ctx.beginPath(); ctx.arc(m.x, m.y, m.r, 0, 7); ctx.fill();
      ctx.fillStyle = 'rgba(229,233,240,0.12)'; ctx.beginPath(); ctx.arc(m.x, m.y, m.r + 8, 0, 7); ctx.fill();
      ctx.fillStyle = '#cfd6e4'; ctx.font = `${12 / scale}px Segoe UI`; ctx.textAlign = 'center'; ctx.fillText(m.label, m.x, m.y - m.r - 6 / scale);
    }
    // stars
    for (const s of stars) {
      if (s.active) { const pulse = 0.5 + 0.5 * Math.sin(t * 0.08 + s.x); ctx.fillStyle = s.color + '33'; ctx.beginPath(); ctx.arc(s.x, s.y, s.r + 4 + pulse * 4, 0, 7); ctx.fill(); }
      ctx.fillStyle = s.color; ctx.beginPath(); ctx.arc(s.x, s.y, s.r, 0, 7); ctx.fill();
    }
    if (hover && !hover.sun) {
      ctx.fillStyle = '#e5e9f0'; ctx.font = `${12 / scale}px Segoe UI`; ctx.textAlign = 'left';
      ctx.fillText(hover.title, hover.x + hover.r + 4 / scale, hover.y + 4 / scale);
    }
    constAnim = requestAnimationFrame(draw);
  }
  stopConstellation(); draw();
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
    if (typeof next.metaVersion === 'number' && next.metaVersion !== metaVersion) loadMeta(); // cross-tab freshness
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
// relative "time ago" — coarse, human
const fmtAgo = t => {
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  const d = Math.round(s / 86400);
  if (d < 14) return `${d}d ago`;
  if (d < 60) return `${Math.round(d / 7)}w ago`;
  if (d < 365) return `${Math.round(d / 30)}mo ago`;
  return `${(d / 365).toFixed(1)}y ago`;
};
const agoClass = t => { const h = (Date.now() - t) / 3.6e6; return h < 6 ? 'ago-fresh' : h < 72 ? 'ago-recent' : 'ago-stale'; };

// ---------- render ----------
const OVERVIEW = ['fleet', 'table', 'projects', 'constellation', 'machines'];
function setTabs() {
  for (const [btn, v] of [['viewFleet', 'fleet'], ['viewTable', 'table'], ['viewProjects', 'projects'], ['viewConstellation', 'constellation'], ['viewMachines', 'machines'], ['viewBoard', 'board'], ['viewTimeline', 'timeline']]) {
    const el = $(btn); if (el) el.classList.toggle('on', state.view === v);
  }
  const overview = OVERVIEW.includes(state.view);
  document.querySelector('main').classList.toggle('no-feed', overview);
  for (const id of ['fleet', 'tableView', 'projects', 'constellation', 'machines']) $(id).style.display = (state.view === id.replace('View', '')) ? '' : 'none';
  if (overview) { $('board').style.display = 'none'; $('timeline').style.display = 'none'; $('empty').style.display = 'none'; }
  $('feed').style.display = overview ? 'none' : '';
  document.querySelector('footer').style.display = overview ? 'none' : '';
  $('statbar').style.display = overview ? 'none' : '';
  stopConstellation();
  if (state.view === 'fleet') loadFleet();
  else if (state.view === 'table') loadTable();
  else if (state.view === 'projects') loadProjects();
  else if (state.view === 'constellation') loadConstellation();
  else if (state.view === 'machines') loadMachines();
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
$('viewTable').onclick = () => { if (!BAKED) { state.view = 'table'; setTabs(); } };
$('viewProjects').onclick = () => { if (!BAKED) { state.view = 'projects'; setTabs(); } };
$('viewConstellation').onclick = () => { if (!BAKED) { state.view = 'constellation'; setTabs(); } };
$('viewMachines').onclick = () => { if (!BAKED) { state.view = 'machines'; setTabs(); } };
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
  for (const id of ['viewFleet', 'viewTable', 'viewProjects', 'viewConstellation', 'viewMachines']) { const el = $(id); if (el) el.style.display = 'none'; }
  $('exportBtn').style.display = 'none';
  $('liveBtn').style.display = 'none';
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'replay';
  setTabs(); render();
} else {
  loadSessions();
  loadMeta().then(() => setTabs());
  startNotifications();
}
