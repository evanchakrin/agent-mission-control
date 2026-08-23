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

// ---------- session picker (custom dropdown: color-coded, archived hidden) ----------
let sessionsCache = [];
async function loadSessions() {
  sessionsCache = await (await fetch('/api/sessions')).json();
}
function renderPicker(filter = '') {
  const q = filter.toLowerCase();
  const items = sessionsCache
    .filter(s => !(s.stableKey && metaMap[s.stableKey] && metaMap[s.stableKey].archived)) // active only
    .filter(s => !q || ((s.title || '') + ' ' + (s.machine || '') + ' ' + s.project).toLowerCase().includes(q))
    .slice(0, 80);
  $('spickerList').innerHTML = items.map(s => {
    const col = kindColor(s.kind);
    const m = metaMap[s.stableKey] || {};
    const proj = projectById(m.projectId);
    return `<div class="sp-item" data-file="${esc(s.file)}" style="border-left:3px solid ${col}">
      <div class="sp-title">${m.pinned ? '★ ' : ''}${esc(s.title || s.session.slice(0, 8))}</div>
      <div class="sp-meta"><span class="kind-badge" style="background:${col}22;color:${col}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span>
        ${proj ? `<span class="mini-badge" style="background:${proj.color}22;color:${proj.color}">${esc(proj.name)}</span>` : ''}
        <span>${esc(s.machine || '')}</span><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span>${s.agentCount ? `<span>${s.agentCount} agents</span>` : ''}</div>
    </div>`;
  }).join('') || '<div class="sp-empty">no active sessions match</div>';
  $('spickerList').querySelectorAll('.sp-item').forEach(el => el.onclick = () => { closePicker(); openSession(el.dataset.file); });
}
function closePicker() { $('spickerPanel').classList.remove('open'); }
$('spickerBtn').onclick = async e => {
  e.stopPropagation();
  const open = $('spickerPanel').classList.toggle('open');
  if (open) { await loadSessions(); renderPicker($('spickerSearch').value); $('spickerSearch').focus(); }
};
$('spickerSearch').oninput = () => renderPicker($('spickerSearch').value);
$('spickerPanel').onclick = e => e.stopPropagation();
document.addEventListener('click', closePicker);
function setPickerLabel(file) {
  const s = sessionsCache.find(x => x.file === file) || (fleetCache || []).find(x => x.file === file);
  $('spickerBtn').innerHTML = s
    ? `<span class="sp-dot" style="background:${kindColor(s.kind)}"></span>${esc((s.title || '').slice(0, 44))}`
    : 'session';
  state.fileTitle = s ? (s.title || '') : '';
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

// ---------- deep search (full-text across every transcript) ----------
function openSearch() { $('searchOverlay').classList.add('open'); $('soInput').focus(); }
function closeSearch() { $('searchOverlay').classList.remove('open'); }
$('deepSearchBtn').onclick = e => { e.stopPropagation(); openSearch(); };
$('searchOverlay').onclick = e => { if (e.target === $('searchOverlay')) closeSearch(); };
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') closeSearch();
  if (e.key === 'k' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); openSearch(); }
});
$('soInput').onkeydown = async e => {
  if (e.key !== 'Enter') return;
  const q = $('soInput').value.trim();
  if (q.length < 2) return;
  $('soResults').innerHTML = '<div class="fleet-loading">Searching every transcript…</div>';
  let data;
  try { data = await (await fetch('/api/search?q=' + encodeURIComponent(q))).json(); }
  catch { $('soResults').innerHTML = '<div class="sp-empty">search failed</div>'; return; }
  $('soResults').innerHTML = data.hits.length
    ? `<div class="so-count">${data.hits.length} hits across ${data.scanned} sessions</div>` + data.hits.map(h => `
      <div class="so-hit" data-file="${esc(h.file)}" data-seq="${h.seq}" style="border-left:3px solid ${kindColor(h.kind)}">
        <div class="so-t">${esc((h.title || '').slice(0, 60))} <span class="dim">· ${esc(h.machine)} · ${fmtAgo(h.mtime)}${h.tool ? ' · ' + esc(h.tool) : ''}</span></div>
        <div class="so-s">${esc(h.snippet)}</div>
      </div>`).join('')
    : `<div class="sp-empty">no matches in ${data.scanned} sessions</div>`;
  $('soResults').querySelectorAll('.so-hit').forEach(el => el.onclick = () => {
    closeSearch();
    openSessionAt(el.dataset.file, Number(el.dataset.seq));
  });
};
// open a session and jump the scrubber + inspector to a specific event
function openSessionAt(file, seq) {
  openSession(file);
  const started = Date.now();
  const wait = setInterval(() => {
    if (state.file === file && state.data.events.length > seq) {
      clearInterval(wait);
      seek(seq + 1);
      openDrawer(seq);
    } else if (Date.now() - started > 20000) clearInterval(wait);
  }, 300);
}

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
const machineSeen = new Map(); // name -> wasFresh
let updateNotified = false, budgetNotifiedDay = null;
async function pollNotifications() {
  let fleet;
  try { fleet = await (await fetch('/api/fleet')).json(); } catch { return; }
  for (const s of fleet) {
    const prev = notifSeen.get(s.file);
    if (!prev) { notifSeen.set(s.file, { errors: s.errors, mtime: s.mtime, active: Date.now() - s.mtime < 6e5, stalled: s.stalled }); continue; }
    if (s.errors > prev.errors) {
      pushNotif('error', `${s.errors - prev.errors} new error${s.errors - prev.errors > 1 ? 's' : ''}`, s);
    }
    if (s.stalled && !prev.stalled) pushNotif('error', 'agent stalled — pending tool call >2 min', s);
    const nowActive = Date.now() - s.mtime < 6e5;
    if (prev.active && !nowActive && s.durationMs > 6e5) {
      pushNotif('done', `run finished · ${s.agents} agents · ~${fmtUsd(s.cost)}`, s);
    }
    notifSeen.set(s.file, { errors: s.errors, mtime: s.mtime, active: nowActive, stalled: s.stalled });
  }
  // machine silence: a live machine stopped reporting
  try {
    const ms = await (await fetch('/api/machines')).json();
    for (const m of ms.filter(x => x.remote)) {
      const freshNow = Date.now() - m.lastSeen < 600000; // 10 min: tolerant of relays that predate the boot-poll heartbeat
      const was = machineSeen.get(m.name);
      if (was === true && !freshNow) pushNotif('error', 'machine went silent — relay stopped reporting', { title: m.name, file: '', machine: m.name, kind: 'claude', session: m.name });
      machineSeen.set(m.name, freshNow);
    }
  } catch { /* ignore */ }
  // update available (once per page load)
  if (!updateNotified) {
    try {
      const u = await (await fetch('/api/update-check')).json();
      if (u.updateAvailable) { updateNotified = true; pushNotif('done', `update available: v${u.latest} (running v${u.current})`, { title: 'Agent Mission Control', file: '', machine: '', kind: 'claude', session: 'update' }); }
    } catch { /* ignore */ }
  }
  // daily cost budget (set in Usage view; stored locally)
  const budget = Number(localStorage.getItem('mc-daily-budget') || 0);
  if (budget > 0) {
    const today = new Date().toDateString();
    const todayCost = fleet.filter(s => new Date(s.mtime).toDateString() === today).reduce((n, s) => n + s.cost, 0);
    if (todayCost > budget && budgetNotifiedDay !== today) {
      budgetNotifiedDay = today;
      pushNotif('error', `today's est. cost ~${fmtUsd(todayCost)} exceeded your ${fmtUsd(budget)} budget`, { title: 'Daily budget', file: '', machine: '', kind: 'claude', session: 'budget' });
    }
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
  $('notifList').querySelectorAll('.notif[data-file]').forEach(el => el.onclick = () => { $('notifPanel').classList.remove('open'); if (el.dataset.file) openSession(el.dataset.file); });
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
  wireFleetControls(renderFleet, $('fleet'));
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
function wireFleetControls(rerender, root) {
  // scope to the view that just rendered — Fleet and Table both emit these
  // controls, and unscoped getElementById always found Fleet's hidden copy,
  // leaving Table's filter bar dead
  root = root || $('fleet');
  const q = sel => root.querySelector(sel);
  const search = q('#fleetSearch');
  if (search) search.oninput = () => { fleetFilter = search.value; const p = search.selectionStart; rerender(); const s2 = q('#fleetSearch') || root.querySelector('#fleetSearch'); if (s2) { s2.focus(); s2.setSelectionRange(p, p); } };
  q('#kindSeg')?.querySelectorAll('button').forEach(b => { b.onclick = () => { fleetKind = b.dataset.k; rerender(); }; });
  q('#archSeg')?.querySelectorAll('button').forEach(b => { b.onclick = () => { fleetArchived = b.dataset.a; rerender(); }; });
  const ps = q('#projSel'); if (ps) ps.onchange = () => { fleetProject = ps.value; rerender(); };
  const ms = q('#machineSel'); if (ms) ms.onchange = () => { fleetMachine = ms.value; rerender(); };
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
    `<th class="tact"></th></tr></thead><tbody>` +
    rows.map(s => {
      const c = kindColor(s.kind); const m = metaOf(s);
      return `<tr data-file="${esc(s.file)}"${m.archived ? ' class="row-archived"' : ''}>
        <td class="tsess">${esc(s.title || s.session.slice(0, 8))}</td>
        <td><span class="kind-badge" style="background:${c}22;color:${c}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span></td>
        <td>${esc(s.machine || '')}</td>
        <td class="num">${s.agents}</td><td class="num">${s.events}</td><td class="num">${s.toolCalls}</td>
        <td class="num">${fmtDur(s.durationMs)}</td><td class="num">${fmtTok(s.tokensOut)}</td>
        <td class="num fcost">~${fmtUsd(s.cost)}</td><td class="num ${s.errors ? 'ferr' : ''}">${s.errors || ''}</td>
        <td class="num tdate"><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span></td>
        <td class="tact">${s.stableKey ? `<button class="row-arch" data-sk="${esc(s.stableKey)}" title="${m.archived ? 'unarchive' : 'archive'}">${m.archived ? '⤴' : '🗄'}</button><button class="row-menu" title="organize">⋯</button>` : ''}</td>
      </tr>`;
    }).join('') + `</tbody></table></div>`;
  $('tableView').querySelectorAll('th[data-k]').forEach(th => { th.onclick = () => { const k = th.dataset.k; tableSort = { col: k, dir: tableSort.col === k ? -tableSort.dir : (cols.find(c => c.k === k).num ? -1 : 1) }; renderTable(); }; });
  $('tableView').querySelectorAll('tr[data-file]').forEach(tr => {
    const s = rows.find(x => x.file === tr.dataset.file);
    tr.onclick = () => openSession(tr.dataset.file);
    const arch = tr.querySelector('.row-arch');
    if (arch && s) arch.onclick = e => { e.stopPropagation(); setSessionMeta(s.stableKey, { archived: !metaOf(s).archived }); };
    const menu = tr.querySelector('.row-menu');
    if (menu && s) menu.onclick = e => showCardMenu(e, s);
  });
  wireFleetControls(renderTable, $('tableView'));
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

// ---------- USAGE view (tokens / agents / cost over time) ----------
let usageGran = 'month', usageMetric = 'cost';
async function loadUsage() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  renderUsage();
}
function bucketKey(ts, gran) {
  const d = new Date(ts);
  const y = d.getFullYear(), mo = String(d.getMonth() + 1).padStart(2, '0'), da = String(d.getDate()).padStart(2, '0');
  if (gran === 'year') return `${y}`;
  if (gran === 'month') return `${y}-${mo}`;
  if (gran === 'week') { const oneJan = new Date(y, 0, 1); const wk = Math.ceil(((d - oneJan) / 86400000 + oneJan.getDay() + 1) / 7); return `${y}-W${String(wk).padStart(2, '0')}`; }
  return `${y}-${mo}-${da}`;
}
function metricVal(s, metric) {
  if (metric === 'cost') return s.cost || 0;
  if (metric === 'tokens') return (s.tokensIn || 0) + (s.tokensOut || 0);
  if (metric === 'agents') return s.agents || 0;
  return 1; // sessions
}
const METRIC_LABEL = { cost: 'Est. cost', tokens: 'Tokens', agents: 'Agents', sessions: 'Sessions' };
const fmtMetric = (v, m) => m === 'cost' ? fmtUsd(v) : m === 'tokens' ? fmtTok(v) : String(Math.round(v));

function renderUsage() {
  const data = (fleetCache || []).filter(s => s.mtime);
  // bucket -> {kind -> sum}
  const buckets = new Map();
  for (const s of data) {
    const k = bucketKey(s.mtime, usageGran);
    if (!buckets.has(k)) buckets.set(k, { claude: 0, codex: 0, otel: 0, total: 0, sessions: 0 });
    const b = buckets.get(k);
    const v = metricVal(s, usageMetric);
    b[s.kind] = (b[s.kind] || 0) + v; b.total += v; b.sessions++;
  }
  const keys = [...buckets.keys()].sort();
  const totAll = { cost: 0, tokensIn: 0, tokensCache: 0, tokensOut: 0, agents: 0, sessions: data.length };
  for (const s of data) { totAll.cost += s.cost || 0; totAll.tokensIn += s.tokensIn || 0; totAll.tokensCache += s.tokensCache || 0; totAll.tokensOut += s.tokensOut || 0; totAll.agents += s.agents || 0; }
  const range = data.length ? `${new Date(Math.min(...data.map(s => s.mtime))).toLocaleDateString()} – ${new Date(Math.max(...data.map(s => s.mtime))).toLocaleDateString()}` : '—';

  // summary tiles
  const tiles = `<div class="usage-tiles">
    <div class="utile"><div class="ul">Total est. cost</div><div class="uv accent">~${fmtUsd(totAll.cost)}</div></div>
    <div class="utile"><div class="ul">Fresh in / out</div><div class="uv">${fmtTok(totAll.tokensIn)} / ${fmtTok(totAll.tokensOut)}</div></div>
    <div class="utile"><div class="ul">Cache reads <span title="cached prefix re-read each turn, billed at 0.1×">ⓘ</span></div><div class="uv small" style="font-size:16px">${fmtTok(totAll.tokensCache)}</div></div>
    <div class="utile"><div class="ul">Agents</div><div class="uv">${totAll.agents}</div></div>
    <div class="utile"><div class="ul">Sessions</div><div class="uv">${totAll.sessions}</div></div>
    <div class="utile"><div class="ul">Range</div><div class="uv small">${range}</div></div>
  </div>`;

  // chart geometry: fill available width, stretch bars when few buckets,
  // horizontal-scroll only when truly many; y-gridlines; labels never overlap
  const max = Math.max(...keys.map(k => buckets.get(k).total), 1);
  const H = 300, PAD_L = 56, PAD_B = 30, PAD_T = 30; // top pad leaves room for total labels above full-height bars
  const availW = Math.max(($('usage').clientWidth || 900) - 82, 320); // view padding + wrap padding + border + scrollbar
  const BW = Math.min(64, Math.max(14, Math.floor((availW - PAD_L) / Math.max(keys.length, 1) * 0.72)));
  const GAP = Math.max(4, Math.round(BW * 0.38));
  const chartW = Math.max(keys.length * (BW + GAP) + PAD_L + 8, availW); // never narrower than the container, scrolls only when bars demand it
  const plotH = H - PAD_B - PAD_T;
  const order = ['claude', 'codex', 'otel'];
  let bars = '';
  // y-axis gridlines at nice fractions
  for (let g = 0; g <= 4; g++) {
    const v = max * g / 4, gy = PAD_T + plotH - (g / 4) * plotH;
    bars += `<line class="ugrid" x1="${PAD_L}" y1="${gy.toFixed(1)}" x2="${chartW - 8}" y2="${gy.toFixed(1)}"/>`;
    bars += `<text class="uy-label" x="${PAD_L - 8}" y="${(gy + 3).toFixed(1)}" text-anchor="end">${fmtMetric(v, usageMetric)}</text>`;
  }
  const labelEvery = Math.ceil(keys.length / Math.floor((chartW - PAD_L) / 74)); // skip labels that would collide
  const showTotals = BW >= 30;
  keys.forEach((k, i) => {
    const b = buckets.get(k);
    const x = PAD_L + i * (BW + GAP);
    let yTop = PAD_T + plotH;
    const tip = `${k} — total ${fmtMetric(b.total, usageMetric)}\n` + order.filter(kd => b[kd]).map(kd => `${AGENT_KIND[kd].label}: ${fmtMetric(b[kd], usageMetric)}`).join('\n');
    for (const kind of order) {
      const v = b[kind] || 0; if (!v) continue;
      const h = Math.max((v / max) * plotH, 1.5);
      bars += `<rect class="ubar" x="${x}" y="${(yTop - h).toFixed(1)}" width="${BW}" height="${h.toFixed(1)}" rx="2" fill="${kindColor(kind)}"><title>${esc(tip)}</title></rect>`;
      yTop -= h;
    }
    if (showTotals) bars += `<text class="ubar-total" x="${x + BW / 2}" y="${Math.max(yTop - 6, 12).toFixed(1)}" text-anchor="middle">${fmtMetric(b.total, usageMetric)}</text>`;
    if (i % labelEvery === 0) {
      const label = usageGran === 'day' ? k.slice(5) : usageGran === 'week' ? k.slice(2) : k;
      bars += `<text class="ux-label" x="${x + BW / 2}" y="${H - 8}" text-anchor="middle">${label}</text>`;
    }
  });
  bars += `<line class="uaxis" x1="${PAD_L}" y1="${PAD_T + plotH}" x2="${chartW - 8}" y2="${PAD_T + plotH}"/>`;

  const grans = [['day', 'Day'], ['week', 'Week'], ['month', 'Month'], ['year', 'Year']];
  const metrics = [['cost', 'Cost'], ['tokens', 'Tokens'], ['agents', 'Agents'], ['sessions', 'Sessions']];
  $('usage').innerHTML =
    `<div class="fleet-head"><h2>Usage over time — ${METRIC_LABEL[usageMetric]}</h2>
      <div class="seg" id="metricSeg">${metrics.map(([v, l]) => `<button data-m="${v}" class="${usageMetric === v ? 'on' : ''}">${l}</button>`).join('')}</div>
      <div class="seg" id="granSeg">${grans.map(([v, l]) => `<button data-g="${v}" class="${usageGran === v ? 'on' : ''}">${l}</button>`).join('')}</div>
      <label class="budget-lbl">daily alert $<input id="budgetInput" type="number" min="0" step="5" value="${esc(localStorage.getItem('mc-daily-budget') || '')}" placeholder="off"></label>
    </div>` + tiles +
    `<div class="usage-legend"><span style="color:${kindColor('claude')}">■ Claude</span><span style="color:${kindColor('codex')}">■ Codex</span><span style="color:${kindColor('otel')}">■ OTLP</span></div>` +
    `<div class="usage-chart-wrap"><svg width="${chartW}" height="${H}" class="usage-chart">${bars}</svg></div>`;
  $('usage').querySelector('#metricSeg').querySelectorAll('button').forEach(b => b.onclick = () => { usageMetric = b.dataset.m; renderUsage(); });
  $('usage').querySelector('#granSeg').querySelectorAll('button').forEach(b => b.onclick = () => { usageGran = b.dataset.g; renderUsage(); });
  const bi = $('usage').querySelector('#budgetInput');
  bi.onchange = () => { localStorage.setItem('mc-daily-budget', bi.value || '0'); budgetNotifiedDay = null; };
}

// ---------- FLOWS view (fleet-wide behavior: weighted tool-flow + trajectory clusters) ----------
// Nobody else has cross-machine multi-session data: this aggregates EVERY
// session's top tool usage and control shape into one behavioral picture.
let flowsCache = null;
async function loadFlows() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  if (!flowsCache) {
    $('flows').innerHTML = '<div class="fleet-loading">Analyzing fleet behavior across all sessions…</div>';
    // sample up to 60 most recent sessions' full event streams
    const picks = filteredFleet().slice(0, 60);
    const results = [];
    for (const chunk of [picks.slice(0, 20), picks.slice(20, 40), picks.slice(40, 60)]) {
      const part = await Promise.all(chunk.map(s =>
        fetch('/api/session?file=' + encodeURIComponent(s.file)).then(r => r.json()).then(d => ({ s, d })).catch(() => null)));
      results.push(...part.filter(Boolean));
    }
    flowsCache = results;
  }
  renderFlows();
}
function sessionSignatureShape(d) {
  // trajectory signature: dominant tools + fanout bucket + error-ness
  const toolCounts = {};
  for (const a of d.agents) for (const [t, n] of Object.entries(a.tools || {})) toolCounts[t.replace(/^mcp__[^_]+__/, '')] = (toolCounts[t.replace(/^mcp__[^_]+__/, '')] || 0) + n;
  const top = Object.entries(toolCounts).sort((a, b) => b[1] - a[1]).slice(0, 3).map(([t]) => t);
  const fan = d.agents.length <= 1 ? 'solo' : d.agents.length <= 5 ? 'small-team' : d.agents.length <= 30 ? 'team' : 'fleet';
  const err = d.events.some(e => e.error) ? 'errors' : 'clean';
  return `${fan} · ${top.join('+') || 'no-tools'} · ${err}`;
}
function renderFlows() {
  const data = flowsCache || [];
  const norm = n => String(n || '').replace(/\s*#\d+$/, '').replace(/[0-9a-f-]{12,}/g, '·').slice(0, 30);
  // per-role aggregation across the whole fleet
  const roles = new Map();
  for (const { s, d } of data) {
    for (const a of d.agents) {
      if (a.id === 'main') continue;
      const key = norm(a.name || 'subagent');
      const r = roles.get(key) || { n: 0, errors: 0, clean: 0, cost: 0, durMs: 0, durN: 0, machines: new Set(), lastMs: 0, example: null };
      r.n++;
      r.errors += a.errors || 0;
      if (!a.errors) r.clean++;
      r.cost += a.cost || 0;
      if (a.firstTs && a.lastTs) { r.durMs += new Date(a.lastTs) - new Date(a.firstTs); r.durN++; }
      r.machines.add(s.machine || 'local');
      if (s.mtime > r.lastMs) { r.lastMs = s.mtime; r.example = s; }
      roles.set(key, r);
    }
  }
  const topRoles = [...roles.entries()].sort((a, b) => b[1].n - a[1].n).slice(0, 20);
  const maxN = Math.max(...topRoles.map(([, r]) => r.n), 1);

  // trajectory clusters, in plain language
  const FAN_WORDS = { solo: 'worked alone', 'small-team': 'small team (2–5 agents)', team: 'team (6–30 agents)', fleet: 'big fleet (30+ agents)' };
  const clusters = new Map();
  for (const { s, d } of data) {
    const sig = sessionSignatureShape(d);
    if (!clusters.has(sig)) clusters.set(sig, []);
    clusters.get(sig).push(s);
  }
  const sorted = [...clusters.entries()].filter(([, v]) => v.length > 1).sort((a, b) => b[1].length - a[1].length);
  const outliers = [...clusters.entries()].filter(([, v]) => v.length === 1);
  const biggest = sorted[0];
  const plainSig = sig => {
    const [fan, tools, err] = sig.split(' · ');
    return `${FAN_WORDS[fan] || fan}, mostly using ${tools.replace(/\+/g, ' and ')}${err === 'errors' ? ', hit errors' : ', ran clean'}`;
  };

  $('flows').innerHTML =
    `<div class="fleet-head"><h2>How your fleet behaves — ${data.length} recent sessions</h2><button id="flowsRefresh" class="mini-btn">↻ refresh</button></div>

    <div class="flows-panel" style="margin-bottom:16px">
      <h3>Your most-used agent roles <span class="qi" title="Every time an orchestrator hands work to a named helper agent, that's a delegation. This table totals them across every session on every machine, so you can see which specialist roles your fleet actually relies on — and which ones fail.">ⓘ</span></h3>
      <div class="dim" style="margin-bottom:10px">The specialist agents your orchestrators call on most, ranked by how often. Red bars mean that role hits errors — a role with a low success rate is a prompt worth improving.</div>
      <div class="role-table">
        <div class="rt-head"><span>Role</span><span title="how many times this role was spawned">Used</span><span title="share of runs that finished with zero errors — under 80% means this role's prompt or task needs work">Success</span><span title="average time this role runs before finishing">Avg time</span><span title="total estimated spend on this role across all sessions">Total cost</span><span title="which of your machines this role ran on">Machines</span><span title="most recent session using this role — click to open">Last seen</span></div>
        ${topRoles.map(([name, r]) => {
          const success = Math.round(r.clean / r.n * 100);
          const sCls = success >= 80 ? 'ok' : success >= 50 ? 'warn' : 'bad';
          return `<div class="rt-row" data-file="${esc(r.example?.file || '')}" title="Click to open the most recent session that used ${esc(name)}">
            <span class="rt-name"><span class="rt-bar" style="width:${Math.max(6, r.n / maxN * 100)}%"></span><b>${esc(name)}</b></span>
            <span>×${r.n}</span>
            <span class="rt-s ${sCls}">${success}%</span>
            <span>${r.durN ? fmtDur(r.durMs / r.durN) : '—'}</span>
            <span class="fcost">~${fmtUsd(r.cost)}</span>
            <span class="dim">${[...r.machines].join(', ')}</span>
            <span class="dim">${fmtAgo(r.lastMs)}</span>
          </div>`;
        }).join('') || '<div class="dim">no delegation observed yet</div>'}
      </div>
    </div>

    <div class="flows-grid">
      <div class="flows-panel">
        <h3>Your usual patterns <span class="qi" title="Sessions are grouped by how they behaved: how many agents they used, which tools dominated, and whether they hit errors. Big groups = your routines.">ⓘ</span></h3>
        <div class="dim" style="margin-bottom:10px">These are your fleet's habits — the shapes of work it does over and over. ${biggest ? `Your most common: <b>${esc(plainSig(biggest[0]))}</b> (${biggest[1].length} sessions).` : ''}</div>
        ${sorted.slice(0, 8).map(([sig, ss]) => `
          <div class="flow-cluster">
            <span class="fc-n">×${ss.length}</span>
            <span class="fc-sig" title="${esc(sig)}">${esc(plainSig(sig))}</span>
            <span class="fc-eg" data-file="${esc(ss[0].file)}" title="open an example">e.g. ${esc((ss[0].title || '').slice(0, 28))}</span>
          </div>`).join('') || '<div class="dim">not enough sessions yet</div>'}
      </div>
      <div class="flows-panel">
        <h3>Worth a look <span class="qi" title="Sessions whose behavior matched nothing else in your fleet. Unusual isn't bad — but it's where surprises live: one-off experiments, runs that went sideways, or a new workflow being born.">ⓘ</span></h3>
        <div class="dim" style="margin-bottom:10px">These sessions behaved like nothing else you run. Skim them: an unfamiliar one that <b>hit errors</b> may be a failure worth understanding; a clean one may be a new pattern worth repeating. Archive the ones that are just noise.</div>
        ${outliers.slice(0, 8).map(([sig, ss]) => {
          const s = ss[0];
          const errish = sig.includes('errors');
          return `<div class="flow-cluster fc-outlier ${errish ? 'fc-outlier-err' : ''}">
            <span class="fc-n">${errish ? '⚠' : '✦'}</span>
            <div class="fc-body">
              <span class="fc-eg" data-file="${esc(s.file)}" title="open this session">${esc((s.title || '').slice(0, 40))}</span>
              <span class="fc-why">${esc(plainSig(sig))} — unlike anything else</span>
            </div>
            ${s.stableKey ? `<button class="fc-arch" data-sk="${esc(s.stableKey)}" title="archive this session (just noise)">🗄</button>` : ''}
          </div>`;
        }).join('') || '<div class="dim">no outliers — everything matches a known pattern</div>'}
      </div>
    </div>`;
  $('flowsRefresh').onclick = () => { flowsCache = null; loadFlows(); };
  // outliers & examples open straight into the readable Story view
  $('flows').querySelectorAll('.fc-eg, .rt-row').forEach(el => el.onclick = () => { if (el.dataset.file) { openSession(el.dataset.file); state.view = 'story'; setTabs(); render(); } });
  $('flows').querySelectorAll('.fc-arch').forEach(b => b.onclick = e => { e.stopPropagation(); setSessionMeta(b.dataset.sk, { archived: true }).then(() => { flowsCache = null; loadFlows(); }); });
}

// ---------- PLAYBOOK STUDIO (mine the fleet, generate reusable plays) ----------
// Generation only — the studio designs plays from YOUR fleet's track record;
// you paste them to an agent yourself. It never executes anything.
async function loadPlaybooks() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  if (!flowsCache) { $('playbooks').innerHTML = '<div class="fleet-loading">Studying your fleet’s track record…</div>'; await loadFlows.fetchOnly(); }
  renderPlaybooks();
}
loadFlows.fetchOnly = async function () {
  // study the whole track record, archived included — history is the teacher
  const picks = (fleetCache || []).slice(0, 60);
  const results = [];
  for (const chunk of [picks.slice(0, 20), picks.slice(20, 40), picks.slice(40, 60)]) {
    const part = await Promise.all(chunk.map(s =>
      fetch('/api/session?file=' + encodeURIComponent(s.file)).then(r => r.json()).then(d => ({ s, d })).catch(() => null)));
    results.push(...part.filter(Boolean));
  }
  flowsCache = results;
};

function mineFleet() {
  const data = flowsCache || [];
  const norm = n => String(n || '').replace(/\s*#\d+$/, '').replace(/[0-9a-f-]{12,}/g, '·').slice(0, 30);
  const roles = new Map();
  const sessions = [];
  for (const { s, d } of data) {
    const main = d.agents.find(a => a.id === 'main');
    const subCost = d.agents.filter(a => a.id !== 'main').reduce((n, a) => n + (a.cost || 0), 0);
    const mainCost = main ? (main.cost || 0) : 0;
    sessions.push({ s, agents: d.agents.length, errors: d.events.filter(e => e.error).length, mainCost, subCost, roles: d.agents.filter(a => a.id !== 'main').map(a => norm(a.name)) });
    for (const a of d.agents) {
      if (a.id === 'main') continue;
      const key = norm(a.name || 'subagent');
      const r = roles.get(key) || { n: 0, clean: 0, cost: 0, byMachine: {}, example: null };
      r.n++; if (!a.errors) r.clean++; r.cost += a.cost || 0;
      const m = s.machine || 'local';
      r.byMachine[m] = r.byMachine[m] || { n: 0, clean: 0 };
      r.byMachine[m].n++; if (!a.errors) r.byMachine[m].clean++;
      if (!r.example || s.mtime > r.example.mtime) r.example = s;
      roles.set(key, r);
    }
  }
  // insights (rule-based, plain-language)
  const insights = [];
  for (const [name, r] of roles) {
    if (r.n >= 3 && r.clean / r.n < 0.7) insights.push({ sev: 'bad', icon: '🛠', text: `"${name}" fails ${Math.round((1 - r.clean / r.n) * 100)}% of the time (${r.n} runs). Its prompt or task definition needs work — open a failed run and read what goes wrong.`, file: r.example?.file });
    const machines = Object.entries(r.byMachine).filter(([, v]) => v.n >= 2);
    if (machines.length >= 2) {
      const rates = machines.map(([m, v]) => ({ m, rate: v.clean / v.n }));
      rates.sort((a, b) => b.rate - a.rate);
      if (rates[0].rate - rates[rates.length - 1].rate > 0.3) insights.push({ sev: 'warn', icon: '🖥', text: `"${name}" succeeds ${Math.round(rates[0].rate * 100)}% on ${rates[0].m} but only ${Math.round(rates[rates.length - 1].rate * 100)}% on ${rates[rates.length - 1].m} — the environment matters for this role.`, file: r.example?.file });
    }
  }
  for (const x of sessions.filter(x => x.mainCost + x.subCost > 20)) {
    const share = x.mainCost / (x.mainCost + x.subCost);
    if (share > 0.6 && x.agents > 5) insights.push({ sev: 'warn', icon: '💸', text: `"${(x.s.title || '').slice(0, 40)}" spent ${Math.round(share * 100)}% of ~${fmtUsd(x.mainCost + x.subCost)} in the orchestrator itself despite having ${x.agents} agents — more delegation would likely be cheaper.`, file: x.s.file });
  }
  const cleanRoles = [...roles.entries()].filter(([, r]) => r.n >= 3 && r.clean / r.n >= 0.85).sort((a, b) => b[1].n - a[1].n);
  if (cleanRoles.length >= 2) insights.push({ sev: 'good', icon: '🏆', text: `Your most reliable roles: ${cleanRoles.slice(0, 3).map(([n]) => `"${n}"`).join(', ')} — proven building blocks for new playbooks below.` });
  return { roles, sessions, insights, cleanRoles };
}

function buildPlaybook(kind, mined) {
  const { cleanRoles } = mined;
  const roster = cleanRoles.slice(0, kind === 'review' ? 4 : 6).map(([n, r]) => ({ name: n, success: Math.round(r.clean / r.n * 100), avgCost: r.cost / r.n }));
  const budget = Math.max(5, Math.ceil(roster.reduce((n, r) => n + r.avgCost, 0) * 1.5));
  const lines = [];
  lines.push(`# Playbook: ${kind === 'review' ? 'Adversarial review pass' : 'Parallel build team'} (generated from your fleet's track record)`);
  lines.push('');
  lines.push(`Paste this to a Claude Code session. Fill in the GOAL.`);
  lines.push('');
  lines.push(`GOAL: <describe what you want built/reviewed>`);
  lines.push('');
  lines.push(`Please orchestrate this with a team of subagents, using roles this fleet has proven:`);
  for (const r of roster) lines.push(`- ${r.name} (historical success ${r.success}%${r.avgCost > 0.01 ? `, avg ~${fmtUsd(r.avgCost)}/run` : ''})`);
  lines.push('');
  if (kind === 'review') {
    lines.push(`Pattern: have each reviewer attack the work independently through a different lens, then a final agent merges confirmed findings. Don't let reviewers see each other's output before verdicts.`);
  } else {
    lines.push(`Pattern: split the goal into independent workstreams, one agent per stream, working in parallel; one integrator agent merges results and runs a final consistency check.`);
  }
  lines.push(`Keep total spend under ~$${budget} (1.5× your historical average for this team size). Report per-agent results before merging.`);
  return lines.join('\n');
}

function renderPlaybooks() {
  const mined = mineFleet();
  const { insights } = mined;
  const sevOrder = { bad: 0, warn: 1, good: 2 };
  insights.sort((a, b) => sevOrder[a.sev] - sevOrder[b.sev]);
  $('playbooks').innerHTML =
    `<div class="fleet-head"><h2>Playbook Studio <span class="qi" title="Studies your fleet's real track record — which roles succeed, where money leaks, which patterns run clean — and turns it into insights and ready-to-paste orchestration playbooks. It only generates text; nothing runs from here.">ⓘ</span></h2><button id="pbRefresh" class="mini-btn">↻ re-study</button></div>
    <div class="flows-grid">
      <div class="flows-panel">
        <h3>What your fleet is telling you</h3>
        <div class="dim" style="margin-bottom:10px">Findings from ${(flowsCache || []).length} recent sessions. Click one to open the session behind it — the Story tab shows you the actual conversation.</div>
        ${insights.slice(0, 12).map(i => `
          <div class="pb-insight pb-${i.sev}" ${i.file ? `data-file="${esc(i.file)}"` : ''}>
            <span class="pb-icon">${i.icon}</span><span class="pb-text">${esc(i.text)}</span>
          </div>`).join('') || '<div class="dim">Not enough history yet — run more agent sessions and come back.</div>'}
      </div>
      <div class="flows-panel">
        <h3>Ready-to-paste playbooks</h3>
        <div class="dim" style="margin-bottom:10px">Orchestration prompts built from YOUR proven roles and real costs. Copy one, paste it to any Claude Code session, fill in the goal.</div>
        ${['build', 'review'].map(k => `
          <div class="pb-card">
            <div class="pb-card-head"><b>${k === 'build' ? '🔨 Parallel build team' : '⚔️ Adversarial review pass'}</b><button class="mini-btn pb-copy" data-kind="${k}">📋 copy</button></div>
            <pre class="pb-preview">${esc(buildPlaybook(k, mined).split('\n').slice(0, 9).join('\n'))}\n…</pre>
          </div>`).join('')}
        <div class="dim" style="margin-top:6px">Budgets are 1.5× your historical average for the roster — a guardrail, not a guess.</div>
      </div>
    </div>`;
  $('pbRefresh').onclick = () => { flowsCache = null; loadPlaybooks(); };
  $('playbooks').querySelectorAll('.pb-insight[data-file]').forEach(el => el.onclick = () => { openSession(el.dataset.file); state.view = 'story'; setTabs(); render(); });
  $('playbooks').querySelectorAll('.pb-copy').forEach(b => b.onclick = () => {
    navigator.clipboard.writeText(buildPlaybook(b.dataset.kind, mined)).then(() => { b.textContent = '✓ copied'; setTimeout(() => { b.textContent = '📋 copy'; }, 1500); });
  });
}

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
      const hubV = machinesData.find(x => !x.remote)?.version;
      const drift = m.version && hubV && m.version !== hubV;
      return `<div class="mcard ${fresh ? 'fresh' : ''}">
        <h3>${m.remote ? '⇄' : '★'} ${esc(m.name)} ${m.version ? `<span class="mver ${drift ? 'drift' : ''}" title="${drift ? 'version differs from hub v' + esc(hubV) : 'app version'}">v${esc(m.version)}${drift ? ' ⚠' : ''}</span>` : ''} <span class="mstatus ${fresh ? 'on' : ''}">${fresh ? 'live' : 'idle'}</span></h3>
        <div class="mips">${(m.ips || []).map(ip => `<span class="ip">${esc(ip)}</span>`).join('') || '<span class="ip dim">no IPs reported</span>'}</div>
        <div class="mstats"><span><b>${st.sessions}</b> sessions</span><span><b>${st.agents}</b> agents</span><span class="fcost"><b>~${fmtUsd(st.cost)}</b></span></div>
        <div class="mkinds">${kindDots}</div>
        <div class="fdate">last seen ${new Date(m.lastSeen).toLocaleString()}</div>
      </div>`;
    }).join('') + `</div>`;
}

// ---------- CONSTELLATION view (force-directed galaxy) ----------
let constAnim = null;
function stopConstellation() {
  if (constAnim) { cancelAnimationFrame(constAnim); constAnim = null; }
  // defensively blank the canvas so a frozen last frame can never bleed into another view
  const cv = document.getElementById('constCanvas');
  if (cv && state.view !== 'constellation') { const c = cv.getContext('2d'); if (c) c.clearRect(0, 0, cv.width, cv.height); }
}
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
  let simFrames = 240; // physics runs until settled, then freezes; interaction re-wakes it
  let userAdjusted = false; // once the user pans/zooms, stop auto-fitting
  const wake = () => { simFrames = Math.max(simFrames, 90); };
  cv.onwheel = e => { e.preventDefault(); userAdjusted = true; const f = e.deltaY < 0 ? 1.1 : 0.9; scale = Math.max(0.3, Math.min(4, scale * f)); };
  cv.onmousedown = e => {
    const p = toWorld(e); const n = pick(p);
    if (n && !n.sun) { dragging = n; wake(); } else { panning = true; lastM = { x: e.clientX, y: e.clientY }; }
  };
  cv.onmousemove = e => {
    const p = toWorld(e); hover = pick(p);
    cv.style.cursor = hover ? 'pointer' : (panning ? 'grabbing' : 'grab');
    if (dragging) { dragging.x = p.x; dragging.y = p.y; dragging.vx = dragging.vy = 0; wake(); }
    else if (panning) { userAdjusted = true; tx += e.clientX - lastM.x; ty += e.clientY - lastM.y; lastM = { x: e.clientX, y: e.clientY }; }
  };
  // auto-fit view to the node cloud (with padding) until the user takes over —
  // keeps clusters tight on screen instead of drifting into empty space
  function autoFit() {
    if (userAdjusted || !nodes.length) return;
    let minX = 1e9, minY = 1e9, maxX = -1e9, maxY = -1e9;
    for (const n of nodes) { minX = Math.min(minX, n.x - n.r); minY = Math.min(minY, n.y - n.r); maxX = Math.max(maxX, n.x + n.r); maxY = Math.max(maxY, n.y + n.r); }
    const pad = 60, bw = maxX - minX + pad * 2, bh = maxY - minY + pad * 2;
    const target = Math.min(W / bw, H / bh, 1.6);
    scale += (target - scale) * 0.08; // ease toward fit
    tx += ((W - (minX - pad + maxX + pad) * scale) / 2 - tx) * 0.08;
    ty += ((H - (minY - pad + maxY + pad) * scale) / 2 - ty) * 0.08;
  }
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
    let ke = 0;
    for (const n of nodes) { if (n === dragging) continue; n.vx *= 0.8; n.vy *= 0.8; n.x += n.vx; n.y += n.vy; ke += n.vx * n.vx + n.vy * n.vy; }
    return ke;
  }
  let t = 0;
  function draw() {
    // run physics only while waking/unsettled; freeze positions once at rest so
    // the galaxy stops the constant slow drift. Pulses keep animating via t.
    if (simFrames > 0 || dragging) { const ke = step(); simFrames--; if (ke < 0.4 && !dragging) simFrames = 0; }
    autoFit();
    t++;
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
  setPickerLabel(file);
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
  // lifecycle-aware: failed / retrying / stalled beat the generic states
  const meta = state.data.agents.find(a => a.id === s.id) || s;
  const now = state.data.now || Date.now();
  const lastActivity = meta.lastTs || s.lastTs;
  const recent = lastActivity && now - new Date(lastActivity) < 600000;
  // "retrying" is a live state — with no activity for 10 min it's just failed
  if (meta.retrying) return recent && state.live ? 'retrying' : 'failed';
  if (meta.lastErrored && (s.done || !state.live || !recent)) return 'failed';
  if (state.live && meta.pendingTool && meta.pendingTool.since && now - new Date(meta.pendingTool.since) > 120000 && !s.done) return 'stalled';
  if (s.id !== 'main' && s.done) return meta.lastErrored ? 'failed' : 'done';
  const last = s.lastTs ? new Date(s.lastTs).getTime() : 0;
  if (state.live && now - last < 20000) return 'working';
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
const OVERVIEW = ['fleet', 'table', 'projects', 'usage', 'flows', 'playbooks', 'constellation', 'machines'];
function setTabs() {
  for (const [btn, v] of [['viewFleet', 'fleet'], ['viewTable', 'table'], ['viewProjects', 'projects'], ['viewUsage', 'usage'], ['viewFlows', 'flows'], ['viewPlaybooks', 'playbooks'], ['viewConstellation', 'constellation'], ['viewMachines', 'machines'], ['viewBoard', 'board'], ['viewStory', 'story'], ['viewLanes', 'lanes'], ['viewWaterfall', 'waterfall'], ['viewCost', 'costflow'], ['viewTimeline', 'timeline']]) {
    const el = $(btn); if (el) el.classList.toggle('on', state.view === v);
  }
  const overview = OVERVIEW.includes(state.view);
  document.querySelector('main').classList.toggle('no-feed', overview);
  $('empty').style.display = 'none'; // only board/timeline turn it back on
  for (const id of ['fleet', 'tableView', 'projects', 'usage', 'flows', 'playbooks', 'constellation', 'machines']) $(id).style.display = (state.view === id.replace('View', '')) ? '' : 'none';
  if (overview) for (const p of SESSION_PANES) $(p).style.display = 'none';
  $('feed').style.display = overview ? 'none' : '';
  document.querySelector('footer').style.display = overview ? 'none' : '';
  $('statbar').style.display = overview ? 'none' : '';
  stopConstellation();
  if (state.view === 'fleet') loadFleet();
  else if (state.view === 'table') loadTable();
  else if (state.view === 'projects') loadProjects();
  else if (state.view === 'usage') loadUsage();
  else if (state.view === 'flows') loadFlows();
  else if (state.view === 'playbooks') loadPlaybooks();
  else if (state.view === 'constellation') loadConstellation();
  else if (state.view === 'machines') loadMachines();
}

function render() {
  if (OVERVIEW.includes(state.view)) return;
  renderStatbar();
  if (state.view === 'board') renderBoard();
  else if (state.view === 'waterfall') renderWaterfall();
  else if (state.view === 'lanes') renderLanes();
  else if (state.view === 'costflow') renderCostFlow();
  else if (state.view === 'story') renderStory();
  else renderTimeline();
  renderFeed();
  $('scrub').max = state.data.events.length;
  $('scrub').value = state.scrub;
  $('scrubLabel').textContent = `${state.scrub} / ${state.data.events.length}`;
}

function renderStatbar() {
  const a = state.data.agents;
  const evs = state.data.events;
  const inT = a.reduce((n, x) => n + (x.inTokens || 0), 0);
  const cacheT = a.reduce((n, x) => n + (x.cacheTokens || 0), 0);
  const outT = a.reduce((n, x) => n + (x.outTokens || 0), 0);
  const cost = a.reduce((n, x) => n + (x.cost || 0), 0);
  const toolCalls = evs.filter(e => e.kind === 'tool-call' || e.kind === 'spawn').length;
  const errs = evs.filter(e => e.error).length;
  const first = evs.find(e => e.ts), last = [...evs].reverse().find(e => e.ts);
  const dur = first && last ? new Date(last.ts) - new Date(first.ts) : 0;
  $('statbar').innerHTML =
    `<span>agents <b>${a.length}</b></span><span>events <b>${evs.length}</b></span>` +
    `<span>tool calls <b>${toolCalls}</b></span><span>duration <b>${fmtDur(dur)}</b></span>` +
    `<span>tokens in <b>${fmtTok(inT)}</b> · cache <b>${fmtTok(cacheT)}</b> · out <b>${fmtTok(outT)}</b></span>` +
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

const SESSION_PANES = ['board', 'timeline', 'waterfall', 'lanes', 'costflow', 'story'];
function showPane(id) { for (const p of SESSION_PANES) $(p).style.display = p === id ? '' : 'none'; }

function renderBoard() {
  showPane('board');
  const stage = $('stage'), cards = $('cards'), svg = $('edges');
  const W = stage.clientWidth, H = stage.clientHeight;
  if (!state.file && !state.data.events.length) { // no session chosen yet
    cards.innerHTML = ''; svg.innerHTML = '';
    $('empty').style.display = 'flex';
    $('empty').textContent = 'Pick a session from the Fleet, Table, or Galaxy to view its agent board.';
    return;
  }
  const agents = agentStateAt(state.scrub);
  let subs = agents.filter(x => x.id !== 'main');
  $('empty').textContent = 'No agent activity yet in this session.';
  $('empty').style.display = agents.length ? 'none' : 'flex';

  const evsAll = state.data.events;
  const firstTs = evsAll.find(e => e.ts)?.ts, lastTs = [...evsAll].reverse().find(e => e.ts)?.ts;

  // Auto-prune: on busy boards, show only agents that need attention
  // (working / stalled / retrying / failed); collapse done+idle into a chip.
  let pruned = { done: 0, idle: 0, failed: 0 };
  let subs2 = subs;
  const nowMs = state.data.now || Date.now();
  if (!state.boardShowAll && subs.length > 10) {
    subs2 = subs.filter(a => {
      const st = statusOf(a);
      if (st === 'done') { pruned.done++; return false; }
      if (st === 'idle') { pruned.idle++; return false; }
      // stale failures (>30 min old) fold away too — visible in the chip, not as cards
      if (st === 'failed' && a.lastTs && nowMs - new Date(a.lastTs) > 1800000) { pruned.failed++; return false; }
      return true;
    });
    // pruning everything is fine: a finished team is just the orchestrator + chip
  }
  subs = subs2;
  const prunedN = pruned.done + pruned.idle + pruned.failed;

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
  if (prunedN || state.boardShowAll) {
    const chip = document.createElement('button');
    chip.className = 'prune-chip';
    chip.textContent = state.boardShowAll
      ? '◎ focus mode (hide finished)'
      : `✓ ${pruned.done} done · ${pruned.idle} idle${pruned.failed ? ` · ${pruned.failed} failed` : ''} — show all`;
    chip.onclick = e => { e.stopPropagation(); state.boardShowAll = !state.boardShowAll; renderBoard(); };
    cards.appendChild(chip);
  }
  const shownIds = new Set([...subs.map(a => a.id), 'main']);
  for (const a of agents) {
    if (!shownIds.has(a.id)) continue;
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
    const st = statusOf(a);
    // typed lifecycle edges: returned=green solid+arrow, failed=red, retrying=red dash,
    // stalled=amber dash, in-flight=dashed neutral, active=teal glow
    const edgeCls =
      st === 'failed' ? 'edge-failed' :
      st === 'retrying' ? 'edge-retrying' :
      st === 'stalled' ? 'edge-stalled' :
      a.done ? 'edge-done' : 'edge-pending';
    const d = `M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`;
    paths += `<path class="edge ${edgeCls} ${hot ? 'hot' : ''}" d="${d}"/>`;
    if (a.done && !compact) paths += `<circle class="edge-cap ${st === 'failed' ? 'cap-failed' : 'cap-done'}" cx="${x2}" cy="${y2}" r="3.5"/>`;
    // task label riding the edge (what work is flowing, not just topology)
    if (!compact && a.task) {
      const label = a.task.replace(/\s+/g, ' ').slice(0, 34) + (a.task.length > 34 ? '…' : '');
      paths += `<text class="edge-label" x="${(x1 + x2) / 2}" y="${(y1 + y2) / 2 - 6}" text-anchor="middle">${esc(label)}</text>`;
    }
    if (st === 'retrying' || st === 'stalled') {
      paths += `<text class="edge-state ${st === 'retrying' ? 'es-retry' : 'es-stall'}" x="${(x1 + x2) / 2}" y="${(y1 + y2) / 2 + 12}" text-anchor="middle">${st === 'retrying' ? '↻ retrying' : '⏸ stalled ' + (a.pendingTool ? 'in ' + esc(a.pendingTool.tool) : '')}</text>`;
    }
    if (hot) paths += `<circle class="pulse-dot" r="4"><animateMotion dur="1s" repeatCount="indefinite" path="${d}"/></circle>`;
  }
  svg.innerHTML = paths;
}

// ---------- WATERFALL view (nested span tree with rollups) ----------
const wfCollapsed = new Set();
function renderWaterfall() {
  showPane('waterfall');
  const evs = state.data.events.filter(e => e.ts);
  if (!evs.length) { $('waterfall').innerHTML = '<div class="fleet-loading">Pick a session to see its span waterfall.</div>'; return; }
  const t0 = new Date(evs[0].ts).getTime();
  const t1 = Math.max(...evs.map(e => new Date(e.endTs || e.ts).getTime()));
  const span = Math.max(t1 - t0, 1000);
  const pct = ts => ((new Date(ts).getTime() - t0) / span * 100);

  // hierarchy: main first, then each subagent (indent 1), spans within
  const agents = agentStateAt(state.data.events.length);
  const main = agents.find(a => a.id === 'main');
  const ordered = [main, ...agents.filter(a => a.id !== 'main')].filter(Boolean);
  let rows = '';
  for (const a of ordered) {
    const depth = a.id === 'main' ? 0 : 1;
    const aEvs = evs.filter(e => e.agent === a.id && (e.kind === 'tool-call' || e.kind === 'spawn'));
    const aDur = a.firstTs && a.lastTs ? new Date(a.lastTs) - new Date(a.firstTs) : 0;
    const collapsed = wfCollapsed.has(a.id);
    const st = statusOf(a);
    // agent rollup row: span bar over its active window + aggregate stats
    rows += `<div class="wf-row wf-agent" data-agent="${esc(a.id)}" style="padding-left:${depth * 22 + 8}px">
      <span class="wf-caret">${aEvs.length ? (collapsed ? '▸' : '▾') : ''}</span>
      <span class="wf-name" style="color:${a.id === 'main' ? 'var(--accent2)' : 'var(--text)'}">${a.id === 'main' ? '🛰️' : '🤖'} ${esc(a.name || a.id)}</span>
      <span class="wf-roll">${aEvs.length} calls · ${fmtTok(a.outTokens)} out · ${fmtDur(aDur)}${a.cost >= 0.005 ? ' · ~' + fmtUsd(a.cost) : ''}${a.errors ? ` · <b class="ferr">${a.errors} err</b>` : ''} <span class="status ${st}">${st}</span></span>
      <span class="wf-track"><span class="wf-bar wf-bar-agent" style="left:${a.firstTs ? pct(a.firstTs).toFixed(2) : 0}%;width:${a.firstTs && a.lastTs ? Math.max(pct(a.lastTs) - pct(a.firstTs), 0.4).toFixed(2) : 0.4}%"></span></span>
    </div>`;
    if (collapsed) continue;
    for (const e of aEvs.slice(-400)) {
      const end = e.endTs || e.ts;
      const w = Math.max(pct(end) - pct(e.ts), 0.25);
      const durMs = e.endTs ? new Date(e.endTs) - new Date(e.ts) : 0;
      const cls = e.error ? 'wf-err' : e.kind === 'spawn' ? 'wf-spawn' : 'wf-tool';
      rows += `<div class="wf-row wf-span" data-seq="${e.seq}" style="padding-left:${depth * 22 + 34}px">
        <span class="wf-name dim">${e.retry ? `<b class="es-retry">↻${e.retry}</b> ` : ''}${esc((e.tool || '').replace(/^mcp__[^_]+__/, ''))}</span>
        <span class="wf-roll">${durMs ? fmtDur(durMs) : '…'}</span>
        <span class="wf-track"><span class="wf-bar ${cls}" style="left:${pct(e.ts).toFixed(2)}%;width:${w.toFixed(2)}%" title="${esc((e.text || '').slice(0, 100))}"></span></span>
      </div>`;
    }
  }
  $('waterfall').innerHTML = `<div class="wf-head"><span>agent / span</span><span></span><span class="wf-axis">${new Date(t0).toLocaleTimeString()} — ${new Date(t1).toLocaleTimeString()} (${fmtDur(span)})</span></div>` + rows;
  $('waterfall').querySelectorAll('.wf-agent').forEach(r => r.onclick = () => {
    const id = r.dataset.agent;
    if (wfCollapsed.has(id)) wfCollapsed.delete(id); else wfCollapsed.add(id);
    renderWaterfall();
  });
  $('waterfall').querySelectorAll('.wf-span').forEach(r => r.onclick = e => { e.stopPropagation(); openDrawer(Number(r.dataset.seq)); });
}

// ---------- LANES view (the event feed as horizontal per-agent card streams) ----------
const EV_ICON = { 'user-text': '💬', 'user-queued': '💬', 'assistant-text': '🗣', 'tool-call': '🔧', 'tool-result': '↩', 'spawn': '🚀', 'spawn-result': '🏁' };
function renderLanes() {
  showPane('lanes');
  const evs = state.data.events.slice(0, state.scrub);
  if (!evs.length) { $('lanes').innerHTML = '<div class="fleet-loading">Pick a session to see its activity lanes.</div>'; return; }
  const agents = agentStateAt(state.scrub);
  const main = agents.find(a => a.id === 'main');
  // active lanes first, orchestrator always on top; cap lanes to keep it readable
  const subs = agents.filter(a => a.id !== 'main')
    .sort((a, b) => ['working', 'retrying', 'stalled'].includes(statusOf(b)) - ['working', 'retrying', 'stalled'].includes(statusOf(a)) || String(b.lastTs).localeCompare(String(a.lastTs)));
  const laneAgents = [main, ...subs.slice(0, 24)].filter(Boolean);
  const dropped = subs.length - Math.min(subs.length, 24);
  const CARDS_PER_LANE = 50;
  $('lanes').innerHTML = laneAgents.map(a => {
    const st = statusOf(a);
    const laneEvs = evs.filter(e => e.agent === a.id).slice(-CARDS_PER_LANE);
    return `<div class="lane">
      <div class="lane-head ${a.id === 'main' ? 'lane-main' : ''}">
        <span class="lane-dot st-${st}"></span>
        <span class="lane-name">${a.id === 'main' ? '🛰️' : '🤖'} ${esc((a.name || a.id).slice(0, 26))}</span>
        <span class="lane-sub">${laneEvs.length}${evs.filter(e => e.agent === a.id).length > CARDS_PER_LANE ? '+' : ''} ev · ${st}</span>
      </div>
      <div class="lane-cards" data-agent="${esc(a.id)}">${laneEvs.map(e => `
        <div class="lcard k-${e.kind}${e.error ? ' err' : ''}" data-seq="${e.seq}" title="${e.ts ? new Date(e.ts).toLocaleTimeString() : ''}">
          <div class="lc-top">${EV_ICON[e.kind] || '•'} ${e.tool ? esc(e.tool.replace(/^mcp__[^_]+__/, '').slice(0, 16)) : (KIND_LABEL[e.kind] || '')}${e.retry ? ` <b class="es-retry">↻${e.retry}</b>` : ''}</div>
          <div class="lc-txt">${esc((e.text || '').slice(0, 64))}</div>
        </div>`).join('') || '<div class="lane-empty">no activity</div>'}</div>
    </div>`;
  }).join('') + (dropped ? `<div class="dim" style="padding:8px 14px">+${dropped} quieter agents not shown (see Board or Waterfall for all)</div>` : '');
  $('lanes').querySelectorAll('.lcard').forEach(el => el.onclick = () => openDrawer(Number(el.dataset.seq)));
  // live: keep each lane scrolled to the newest card
  if (state.live) $('lanes').querySelectorAll('.lane-cards').forEach(el => { el.scrollLeft = el.scrollWidth; });
}

// ---------- STORY view (the session as a readable chat log) ----------
// The thing you actually STUDY: user prompts and assistant replies in full,
// with the tool storms between them collapsed into expandable activity strips.
const storyExpanded = new Set();
function renderStory() {
  showPane('story');
  const evs = state.data.events.slice(0, state.scrub);
  if (!evs.length) { $('story').innerHTML = '<div class="fleet-loading">Pick a session to read its story.</div>'; return; }
  const agentName = id => id === 'main' ? 'Orchestrator' : (state.data.agents.find(a => a.id === id)?.name || 'agent');
  // group into: prompt / reply / activity-burst blocks (main-agent narrative;
  // subagent chatter is folded into the bursts)
  const blocks = [];
  let burst = null;
  const flushBurst = () => { if (burst && burst.evs.length) blocks.push(burst); burst = null; };
  for (const e of evs) {
    const isMainText = e.agent === 'main' && (e.kind === 'user-text' || e.kind === 'user-queued' || e.kind === 'assistant-text');
    if (isMainText) {
      flushBurst();
      blocks.push({ type: e.kind === 'assistant-text' ? 'reply' : 'prompt', e });
    } else {
      if (!burst) burst = { type: 'burst', evs: [], errors: 0, spawns: new Set(), tools: {} };
      burst.evs.push(e);
      if (e.error) burst.errors++;
      if (e.kind === 'spawn' && e.spawnedAgent) burst.spawns.add(e.spawnedAgent);
      if (e.tool) { const t = e.tool.replace(/^mcp__[^_]+__/, ''); burst.tools[t] = (burst.tools[t] || 0) + 1; }
      if (e.agent !== 'main' && e.kind === 'assistant-text') burst.subReplies = (burst.subReplies || 0) + 1;
    }
  }
  flushBurst();
  const shown = blocks.slice(-400);
  $('story').innerHTML = `<div class="story-inner">` + (blocks.length > 400 ? `<div class="dim" style="text-align:center;padding:8px">… ${blocks.length - 400} earlier moments (drag the scrubber back to walk further into the past)</div>` : '') +
    shown.map((b, i) => {
      if (b.type === 'prompt') return `<div class="st-msg st-user" data-seq="${b.e.seq}"><div class="st-who">You · ${b.e.ts ? new Date(b.e.ts).toLocaleString() : ''}</div><div class="st-txt">${esc(b.e.full || b.e.text || '')}</div></div>`;
      if (b.type === 'reply') return `<div class="st-msg st-ai" data-seq="${b.e.seq}"><div class="st-who">${esc(agentName(b.e.agent))}</div><div class="st-txt">${esc(b.e.full || b.e.text || '')}</div></div>`;
      // activity burst
      const key = 'b' + (b.evs[0]?.seq ?? i);
      const open = storyExpanded.has(key);
      const topTools = Object.entries(b.tools).sort((x, y) => y[1] - x[1]).slice(0, 4).map(([t, n]) => `${t}×${n}`).join(' · ');
      const spawnNote = b.spawns.size ? `🚀 ${b.spawns.size} agent${b.spawns.size > 1 ? 's' : ''} spawned` : '';
      return `<div class="st-burst ${b.errors ? 'has-err' : ''}" data-key="${key}">
        <div class="st-burst-head">⚙ ${b.evs.length} actions${topTools ? ' — ' + esc(topTools) : ''}${spawnNote ? ' — ' + spawnNote : ''}${b.errors ? ` — <b class="ferr">${b.errors} error${b.errors > 1 ? 's' : ''}</b>` : ''} <span class="st-tog">${open ? 'collapse ▴' : 'expand ▾'}</span></div>
        ${open ? `<div class="st-burst-body">${b.evs.slice(0, 120).map(e => `
          <div class="st-act ${e.error ? 'err' : ''}" data-seq="${e.seq}">
            <span class="st-act-who">${esc(agentName(e.agent).slice(0, 20))}</span>
            <span class="st-act-what">${EV_ICON[e.kind] || '•'} ${e.tool ? esc(e.tool.replace(/^mcp__[^_]+__/, '')) : (KIND_LABEL[e.kind] || e.kind)}</span>
            <span class="st-act-txt">${esc((e.text || '').slice(0, 90))}</span>
          </div>`).join('')}${b.evs.length > 120 ? `<div class="dim" style="padding:4px 10px">… ${b.evs.length - 120} more (use Waterfall for everything)</div>` : ''}</div>` : ''}
      </div>`;
    }).join('') + `</div>`;
  $('story').querySelectorAll('.st-burst-head').forEach(h => h.onclick = () => {
    const key = h.parentElement.dataset.key;
    if (storyExpanded.has(key)) storyExpanded.delete(key); else storyExpanded.add(key);
    renderStory();
  });
  $('story').querySelectorAll('.st-act, .st-msg').forEach(el => el.ondblclick = () => openDrawer(Number(el.dataset.seq)));
  if (state.live && !renderStory.scrolledOnce) { $('story').scrollTop = $('story').scrollHeight; renderStory.scrolledOnce = true; }
}

// ---------- COST FLOW view (Sankey: where this session's money went) ----------
function renderCostFlow() {
  showPane('costflow');
  const agents = agentStateAt(state.data.events.length);
  if (!agents.length) { $('costflow').innerHTML = '<div class="fleet-loading">Pick a session to see its cost flow.</div>'; return; }
  const useCost = agents.some(a => (a.cost || 0) > 0.001);
  const weight = a => useCost ? (a.cost || 0) : (a.outTokens || 0);
  const unit = v => useCost ? '~' + fmtUsd(v) : fmtTok(v) + ' tok';
  let flows = agents.filter(a => weight(a) > 0).sort((a, b) => weight(b) - weight(a));
  const total = flows.reduce((n, a) => n + weight(a), 0) || 1;
  const TOP = 14;
  let other = null;
  if (flows.length > TOP) {
    const rest = flows.slice(TOP);
    other = { name: `${rest.length} smaller agents`, w: rest.reduce((n, a) => n + weight(a), 0), other: true };
    flows = flows.slice(0, TOP);
  }
  const rows = [...flows.map(a => ({ name: a.id === 'main' ? (a.name || 'Orchestrator') + ' (direct)' : a.name || a.id, w: weight(a), a })), ...(other ? [other] : [])];
  const W = Math.max($('costflow').clientWidth - 60, 600), H = Math.max(rows.length * 34 + 60, 300);
  const RIB_X0 = 210, RIB_X1 = W - 260;
  const plotH = H - 40;
  const totalPx = plotH - rows.length * 6;
  let ySrc = 20, yDst = 20, ribbons = '', labels = '';
  labels += `<text class="cf-src" x="${RIB_X0 - 12}" y="${plotH / 2}" text-anchor="end">${esc((state.fileTitle || 'session').slice(0, 24))} ${unit(total)}</text>`;
  labels += `<rect x="${RIB_X0 - 6}" y="14" width="6" height="${plotH}" rx="3" fill="var(--accent2)" opacity=".8"/>`;
  for (const r of rows) {
    const h = Math.max((r.w / total) * totalPx, 3);
    const srcY = ySrc, dstY = yDst;
    const midX = (RIB_X0 + RIB_X1) / 2;
    const st = r.a ? statusOf(r.a) : 'done';
    const col = r.other ? '#8a93a8' : st === 'failed' ? '#f87171' : r.a && r.a.id === 'main' ? '#818cf8' : '#5eead4';
    ribbons += `<path class="cf-rib" fill="${col}" opacity=".55" d="M ${RIB_X0} ${srcY} C ${midX} ${srcY}, ${midX} ${dstY}, ${RIB_X1} ${dstY} L ${RIB_X1} ${dstY + h} C ${midX} ${dstY + h}, ${midX} ${srcY + h}, ${RIB_X0} ${srcY + h} Z"><title>${esc(r.name)}: ${unit(r.w)} (${Math.round(r.w / total * 100)}%)</title></path>`;
    const pct = Math.round(r.w / total * 100);
    labels += `<text class="cf-lbl" x="${RIB_X1 + 10}" y="${dstY + h / 2 + 4}">${esc(r.name.slice(0, 30))} <tspan class="cf-val">${unit(r.w)} · ${pct}%</tspan></text>`;
    ySrc += h; yDst += h + 6;
  }
  $('costflow').innerHTML =
    `<div class="fleet-head" style="padding:14px 18px 0"><h2>Where the ${useCost ? 'money' : 'output'} went <span class="qi" title="Each ribbon is one agent's share of this session's total ${useCost ? 'estimated cost' : 'output tokens'}. Thicker = more. Red = that agent failed.">ⓘ</span></h2></div>
    <div class="cf-wrap"><svg width="${W}" height="${H}">${ribbons}${labels}</svg></div>`;
}

function renderTimeline() {
  showPane('timeline');
  const agents = agentStateAt(state.data.events.length);
  const evs = state.data.events.filter(e => e.ts);
  if (!evs.length) { $('timeline').innerHTML = '<div class="fleet-loading">Pick a session from the Fleet, Table, or Galaxy to view its timeline.</div>'; return; }
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
$('viewUsage').onclick = () => { if (!BAKED) { state.view = 'usage'; setTabs(); } };
$('viewFlows').onclick = () => { if (!BAKED) { state.view = 'flows'; setTabs(); } };
$('viewPlaybooks').onclick = () => { if (!BAKED) { state.view = 'playbooks'; setTabs(); } };
$('viewConstellation').onclick = () => { if (!BAKED) { state.view = 'constellation'; setTabs(); } };
$('viewMachines').onclick = () => { if (!BAKED) { state.view = 'machines'; setTabs(); } };
$('viewBoard').onclick = () => { state.view = 'board'; setTabs(); render(); };
$('viewWaterfall').onclick = () => { state.view = 'waterfall'; setTabs(); render(); };
$('viewLanes').onclick = () => { state.view = 'lanes'; setTabs(); render(); };
$('viewStory').onclick = () => { state.view = 'story'; setTabs(); render(); };
$('viewCost').onclick = () => { state.view = 'costflow'; setTabs(); render(); };
$('viewTimeline').onclick = () => { state.view = 'timeline'; setTabs(); render(); };
$('exportBtn').onclick = () => {
  if (!state.file) return;
  const title = state.fileTitle || '';
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
window.onresize = () => { clearTimeout(resizeT); resizeT = setTimeout(() => { if (state.view === 'usage') renderUsage(); else if (!OVERVIEW.includes(state.view)) render(); }, 120); };

// ---------- boot ----------
if (BAKED) {
  // standalone replay: no server, no live mode
  state.data = BAKED.data;
  state.scrub = state.data.events.length;
  document.title = 'Replay — ' + BAKED.title;
  $('spicker').style.display = 'none';
  for (const id of ['viewFleet', 'viewTable', 'viewProjects', 'viewUsage', 'viewFlows', 'viewPlaybooks', 'viewConstellation', 'viewMachines']) { const el = $(id); if (el) el.style.display = 'none'; }
  $('exportBtn').style.display = 'none';
  $('liveBtn').style.display = 'none';
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'replay';
  setTabs(); render();
} else {
  loadSessions();
  loadMeta().then(() => setTabs());
  startNotifications();
}
