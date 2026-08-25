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

// ---------- overview tabs ----------
// Tabs that dispatch through setTabs() (own data load, no live feed/footer) rather
// than render() (which drives the session-detail panes). Declared up top because
// the home-view preference below needs it before `state` exists.
const OVERVIEW = ['fleet', 'table', 'projects', 'usage', 'flows', 'playbooks', 'brain', 'audit', 'constellation', 'machines', 'fingerprints', 'calendar', 'rings', 'rhythm'];

// ---------- home-view preference ----------
// The owner is beta-testing which overview becomes his daily screen (Fingerprints,
// Calendar, or the existing Fleet). Persisted locally; falls back to Fleet if the
// stored view was removed in a later update. Each candidate view's header carries
// a "set as home" button built with homeButton()/wireHomeButton() below.
const HOME_KEY = 'mc-home-view';
function getHomeView() { const v = localStorage.getItem(HOME_KEY); return v && OVERVIEW.includes(v) ? v : null; }
function setHomeView(v) { if (v) localStorage.setItem(HOME_KEY, v); else localStorage.removeItem(HOME_KEY); }
function homeButton(id) {
  const isHome = getHomeView() === id;
  return `<button class="home-btn${isHome ? ' is-home' : ''}" data-home="${id}" title="${isHome ? 'This is your home view — it loads first when you open the dashboard' : 'Make this the view that loads when you open the dashboard'}">${isHome ? '⌂ Home view' : '⌂ Set as home'}</button>`;
}
function wireHomeButton(root, id, rerender) {
  const b = root.querySelector('[data-home="' + id + '"]');
  if (b) b.onclick = () => { setHomeView(getHomeView() === id ? null : id); rerender(); };
}

const state = {
  data: { events: [], agents: [], now: 0 },
  es: null, live: !BAKED, scrub: 0, file: null,
  playing: false, speed: 4, playTimer: null,
  view: BAKED ? 'board' : (getHomeView() || 'fleet'),
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
  // update available (once per page load) — name each stale instance + action
  if (!updateNotified) {
    try {
      const u = await (await fetch('/api/update-check')).json();
      const ms = await (await fetch('/api/machines')).json().catch(() => []);
      const stale = [];
      if (u.updateAvailable) stale.push(`this dashboard (v${u.current} → ask Claude to redeploy)`);
      for (const m of ms.filter(x => x.remote && x.version && u.latest && x.version !== u.latest && x.version !== u.current)) {
        stale.push(`${m.name} (v${m.version} → send it the update paste)`);
      }
      if (stale.length) { updateNotified = true; pushNotif('done', `v${u.latest || u.current} is latest — behind: ${stale.join('; ')}`, { title: 'Version check', file: '', machine: '', kind: 'claude', session: 'update' }); }
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

// ---------- FINGERPRINTS view (candidate home screen #1) ----------
// A wall of small multiples: one hand-drawn glyph per session, newest first. Each
// glyph is a 24-bucket band of that session's tool-call activity over time — height
// = intensity, red = a bucket that hit an error, the corner tick = duration, and a
// faint tint by agent kind. The point isn't any one number, it's recognising SHAPE:
// scan the wall and a bad run (spiky, red-flecked) looks different from a clean one
// (smooth, quiet) before you've read a word. Reuses the fleet filters so the wall
// narrows the same way Fleet/Table do.
let fpCache = new Map();  // file -> {buckets[24], errB[24], kind, cost, dur, title, file, errors}
let fpSize = 'medium';
let fpLoading = false;
const FP_CAP = 400;       // cap on sessions whose full event stream gets fetched at once
const FP_DIMS = { small: { w: 54, h: 22 }, medium: { w: 92, h: 36 }, large: { w: 148, h: 54 } };

async function loadFingerprints() {
  // refetch every time: any of these can be the home screen, and a home screen
  // that never updates is worse than no home screen at all
  try { fleetCache = await (await fetch('/api/fleet')).json(); } catch { fleetCache = fleetCache || []; }
  renderFingerprints();
  fetchMissingGlyphs();
}
function computeGlyph(s, d) {
  const events = (d && d.events) || [];
  const N = 24;
  const buckets = new Array(N).fill(0), errB = new Array(N).fill(0);
  const tsMs = events.map(e => e.ts && new Date(e.ts).getTime()).filter(Boolean);
  const t0 = tsMs.length ? Math.min(...tsMs) : (s.mtime - (s.durationMs || 0));
  const t1 = tsMs.length ? Math.max(...tsMs) : s.mtime;
  const span = Math.max(t1 - t0, 1);
  for (const e of events) {
    if (!e.ts) continue;
    const idx = Math.min(N - 1, Math.max(0, Math.floor((new Date(e.ts).getTime() - t0) / span * N)));
    if (e.kind === 'tool-call' || e.kind === 'spawn' || e.kind === 'tool-result') buckets[idx]++;
    if (e.error) errB[idx]++;
  }
  return { buckets, errB, kind: s.kind, cost: s.cost, dur: s.durationMs, title: s.title || s.session.slice(0, 8), file: s.file, errors: s.errors };
}
async function fetchMissingGlyphs() {
  if (fpLoading) return;
  const missing = filteredFleet().slice(0, FP_CAP).filter(s => !fpCache.has(s.file));
  if (!missing.length) return;
  fpLoading = true;
  for (let i = 0; i < missing.length; i += 15) {
    const chunk = missing.slice(i, i + 15);
    await Promise.all(chunk.map(s =>
      fetch('/api/session?file=' + encodeURIComponent(s.file)).then(r => r.json())
        .then(d => fpCache.set(s.file, computeGlyph(s, d)))
        .catch(() => fpCache.set(s.file, computeGlyph(s, { events: [] })))));
    if (state.view === 'fingerprints') renderFingerprints(); // fill the wall in as data arrives
  }
  fpLoading = false;
}
function fpGlyphSvg(g, dims) {
  const { w, h } = dims;
  const N = g.buckets.length, bw = w / N;
  const max = Math.max(...g.buckets, 1);
  const col = kindColor(g.kind);
  const padB = 2, plotH = h - padB - 2;
  let bars = '';
  for (let i = 0; i < N; i++) {
    const v = g.buckets[i];
    if (!v) continue;
    const bh = Math.max((v / max) * plotH, 1);
    const x = i * bw, y = h - padB - bh;
    const err = g.errB[i] > 0;
    bars += `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${Math.max(bw - 0.6, 0.6).toFixed(1)}" height="${bh.toFixed(1)}" fill="${err ? 'var(--red)' : col}" opacity="${err ? .95 : .6}"/>`;
  }
  const durMin = (g.dur || 0) / 60000;
  const durNorm = Math.max(0, Math.min(1, Math.log10(durMin + 1) / Math.log10(181))); // 0..~3h on a log scale
  const tickLen = 3 + durNorm * (w * 0.4);
  return `<svg viewBox="0 0 ${w} ${h}" width="${w}" height="${h}" class="fp-svg">
    <rect x="0.5" y="0.5" width="${w - 1}" height="${h - 1}" rx="3" fill="var(--panel2)" stroke="var(--line)"/>
    ${bars}
    <line x1="${(w - tickLen).toFixed(1)}" y1="${(h - 0.75).toFixed(1)}" x2="${w.toFixed(1)}" y2="${(h - 0.75).toFixed(1)}" stroke="${col}" stroke-width="1.5" opacity=".85"/>
    <title>${esc(g.title)}&#10;~${fmtUsd(g.cost)} · ${fmtDur(g.dur)}${g.errors ? ` · ${g.errors} error${g.errors === 1 ? '' : 's'}` : ''}</title>
  </svg>`;
}
function fpSkeletonSvg(dims) {
  return `<svg viewBox="0 0 ${dims.w} ${dims.h}" width="${dims.w}" height="${dims.h}" class="fp-svg fp-skel"><rect x="0.5" y="0.5" width="${dims.w - 1}" height="${dims.h - 1}" rx="3" fill="var(--panel2)" stroke="var(--line)"/></svg>`;
}
function renderFingerprints() {
  const all = fleetCache || [];
  const list = filteredFleet();
  const shown = list.slice(0, FP_CAP);
  const overflow = list.length - shown.length;
  const totCost = shown.reduce((n, s) => n + s.cost, 0), totAgents = shown.reduce((n, s) => n + s.agents, 0);
  const dims = FP_DIMS[fpSize];
  $('fingerprints').innerHTML =
    fleetControls(shown.length, all.length, totAgents, totCost) +
    `<div class="fp-toolbar">
      <div class="seg" id="fpSizeSeg">${['small', 'medium', 'large'].map(sz => `<button data-sz="${sz}" class="${fpSize === sz ? 'on' : ''}">${sz[0].toUpperCase() + sz.slice(1)}</button>`).join('')}</div>
      ${homeButton('fingerprints')}
      <button id="fpRefresh" class="mini-btn">↻ refresh</button>
    </div>
    <div class="rings-legend">One tile is one session, newest first. Taller bars = a busier stretch of that run, <span style="color:var(--red)">red</span> = a stretch that hit an error, and the tick along the bottom shows how long it ran. You are looking for the odd one out — hover any tile for its name and cost.</div>` +
    (shown.length === 0
      ? `<div class="fp-empty">${all.length === 0 ? 'No sessions yet — once you run something, each session gets its own little shape here.' : 'No sessions match these filters.'}</div>`
      : `<div class="fp-wall sz-${fpSize}">` + shown.map(s => {
        const g = fpCache.get(s.file);
        return `<div class="fp-tile" data-file="${esc(s.file)}">${g ? fpGlyphSvg(g, dims) : fpSkeletonSvg(dims)}</div>`;
      }).join('') + `</div>` +
        (overflow > 0 ? `<div class="fp-overflow">+${overflow} more sessions — narrow with filters to bring them into view</div>` : ''));
  wireFleetControls(() => { renderFingerprints(); fetchMissingGlyphs(); }, $('fingerprints'));
  $('fingerprints').querySelectorAll('.fp-tile').forEach(t => t.onclick = () => openSession(t.dataset.file));
  const sizeSeg = $('fingerprints').querySelector('#fpSizeSeg');
  if (sizeSeg) sizeSeg.querySelectorAll('button').forEach(b => b.onclick = () => { fpSize = b.dataset.sz; renderFingerprints(); });
  wireHomeButton($('fingerprints'), 'fingerprints', renderFingerprints);
  const refresh = $('fingerprints').querySelector('#fpRefresh');
  if (refresh) refresh.onclick = () => { fpCache.clear(); renderFingerprints(); fetchMissingGlyphs(); };
}

// ---------- CALENDAR view (candidate home screen #2) ----------
// A GitHub-contributions-style heatmap of the last ~12 months, one cell per day.
// Colour intensity follows whichever metric the owner picks (sessions/cost/errors/
// agents). A plain-language line above states the busiest day in words, not just
// color. Clicking a day opens a panel below listing that day's sessions.
let calMetric = 'sessions', calSelectedDay = null;
const CAL_METRIC_COLOR = { sessions: '#5eead4', cost: '#818cf8', agents: '#60a5fa', errors: '#f87171' };
const CAL_METRIC_LABEL = { sessions: 'Sessions', cost: 'Cost', errors: 'Errors', agents: 'Agents' };

async function loadCalendar() {
  // refetch every time: any of these can be the home screen, and a home screen
  // that never updates is worse than no home screen at all
  try { fleetCache = await (await fetch('/api/fleet')).json(); } catch { fleetCache = fleetCache || []; }
  renderCalendar();
}
function calDayKey(ts) {
  const d = new Date(ts);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function calMetricVal(rec, metric) {
  if (!rec) return 0;
  if (metric === 'cost') return rec.cost;
  if (metric === 'errors') return rec.errors;
  if (metric === 'agents') return rec.agents;
  return rec.sessions;
}
function calSummary(days) {
  if (!days.size) return 'No sessions yet — this fills in as you work.';
  let best = null;
  for (const d of days.values()) if (!best || d.sessions > best.sessions) best = d;
  const dateStr = best.date.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
  const lead = days.size === 1 ? 'Your only active day so far' : 'Your busiest day was';
  return `${lead} <b>${esc(dateStr)}</b> — ${best.sessions} session${best.sessions === 1 ? '' : 's'}, ~${fmtUsd(best.cost)}${best.errors ? `, ${best.errors} error${best.errors === 1 ? '' : 's'}` : ''}.`;
}
function calDayPanelHtml(key, days) {
  const rec = days.get(key);
  const sessions = (fleetCache || []).filter(s => s.mtime && calDayKey(s.mtime) === key).sort((a, b) => b.mtime - a.mtime);
  const dateStr = rec ? rec.date.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' }) : key;
  return `<div class="cal-day-panel">
    <div class="cal-day-head"><b>${esc(dateStr)}</b><span class="dim">${sessions.length} session${sessions.length === 1 ? '' : 's'}${rec ? ` · ~${fmtUsd(rec.cost)}${rec.errors ? ` · ${rec.errors} errors` : ''}` : ''}</span><button id="calDayClose" class="mini-btn">close</button></div>
    <div class="cal-day-list">${sessions.map(s => {
      const c = kindColor(s.kind);
      return `<div class="cal-day-item" data-file="${esc(s.file)}" style="border-left:3px solid ${c}">
        <span class="cdi-t">${esc(s.title || s.session.slice(0, 8))}</span>
        <span class="cdi-m">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label} · ${esc(s.machine || '')} · ~${fmtUsd(s.cost)}${s.errors ? ` · ${s.errors} err` : ''}</span>
      </div>`;
    }).join('') || '<div class="dim">no sessions this day</div>'}</div>
  </div>`;
}
function renderCalendar() {
  const data = fleetCache || [];
  const days = new Map(); // 'YYYY-MM-DD' -> {sessions, cost, errors, agents, date}
  // Match the other three views: archived sessions are hidden work, so they must
  // not colour the grid or inflate the cost. And never count a day the grid cannot
  // draw — a future mtime could otherwise be named "your busiest day" with no cell.
  const calTodayEnd = new Date(); calTodayEnd.setHours(23, 59, 59, 999);
  for (const s of data) {
    if (!s.mtime || s.mtime > calTodayEnd.getTime()) continue;
    if (metaOf(s).archived) continue;
    const k = calDayKey(s.mtime);
    if (!days.has(k)) days.set(k, { sessions: 0, cost: 0, errors: 0, agents: 0, date: new Date(s.mtime) });
    const d = days.get(k);
    d.sessions++; d.cost += s.cost || 0; d.errors += s.errors || 0; d.agents += s.agents || 0;
  }

  const today = new Date(); today.setHours(0, 0, 0, 0);
  const WEEKS = 53;
  const gridStart = new Date(today);
  gridStart.setDate(gridStart.getDate() - (WEEKS * 7 - 1));
  gridStart.setDate(gridStart.getDate() - gridStart.getDay()); // back up to the preceding Sunday

  const cells = [];
  for (const cur = new Date(gridStart); cur <= today; cur.setDate(cur.getDate() + 1)) {
    cells.push({ date: new Date(cur), key: calDayKey(cur), rec: days.get(calDayKey(cur)) });
  }
  const numWeeks = Math.ceil(cells.length / 7);
  const maxVal = Math.max(...cells.map(c => calMetricVal(c.rec, calMetric)), 0);

  const CELL = 11, GAP = 3, STEP = CELL + GAP, PAD_L = 26, PAD_T = 16;
  const w = PAD_L + numWeeks * STEP, h = PAD_T + 7 * STEP;
  const col = CAL_METRIC_COLOR[calMetric];
  const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  const WD = ['', 'Mon', '', 'Wed', '', 'Fri', ''];

  let svg = '', prevMonth = -1;
  cells.forEach((c, i) => {
    if (i % 7 === 0 && c.date.getMonth() !== prevMonth) {
      svg += `<text class="cal-month-label" x="${PAD_L + Math.floor(i / 7) * STEP}" y="${PAD_T - 5}">${MONTHS[c.date.getMonth()]}</text>`;
      prevMonth = c.date.getMonth();
    }
  });
  for (let r = 0; r < 7; r++) if (WD[r]) svg += `<text class="cal-wd-label" x="0" y="${PAD_T + r * STEP + CELL - 1.5}">${WD[r]}</text>`;
  cells.forEach((c, i) => {
    const x = PAD_L + Math.floor(i / 7) * STEP, y = PAD_T + (i % 7) * STEP;
    const val = calMetricVal(c.rec, calMetric);
    let fill = 'var(--panel2)', op = 1;
    if (val > 0 && maxVal > 0) {
      const r2 = val / maxVal;
      op = r2 > .75 ? 1 : r2 > .5 ? .75 : r2 > .25 ? .5 : .28;
      fill = col;
    }
    const dStr = c.date.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
    const tip = c.rec ? `${dStr}: ${c.rec.sessions} session${c.rec.sessions === 1 ? '' : 's'}, ~${fmtUsd(c.rec.cost)}${c.rec.errors ? `, ${c.rec.errors} err` : ''}` : `${dStr}: no sessions`;
    svg += `<rect class="cal-cell${calSelectedDay === c.key ? ' sel' : ''}" data-key="${c.key}" x="${x}" y="${y}" width="${CELL}" height="${CELL}" rx="2" fill="${fill}" fill-opacity="${op}"><title>${esc(tip)}</title></rect>`;
  });

  $('calendar').innerHTML =
    `<div class="fleet-head"><h2>Calendar — last 12 months</h2>
      <div class="seg" id="calMetricSeg">${Object.keys(CAL_METRIC_LABEL).map(m => `<button data-m="${m}" class="${calMetric === m ? 'on' : ''}">${CAL_METRIC_LABEL[m]}</button>`).join('')}</div>
      ${homeButton('calendar')}
    </div>
    <div class="cal-summary">${calSummary(days)}</div>
    <div class="cal-wrap"><svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}">${svg}</svg></div>
    <div class="cal-legend">less ${[0, .28, .5, .75, 1].map(op => `<span class="cal-lg-cell" style="background:${op === 0 ? 'var(--panel2)' : col};opacity:${op || 1}"></span>`).join('')} more · colour = ${CAL_METRIC_LABEL[calMetric].toLowerCase()}</div>
    ${calSelectedDay ? calDayPanelHtml(calSelectedDay, days) : ''}`;

  $('calendar').querySelector('#calMetricSeg').querySelectorAll('button').forEach(b => b.onclick = () => { calMetric = b.dataset.m; renderCalendar(); });
  wireHomeButton($('calendar'), 'calendar', renderCalendar);
  $('calendar').querySelectorAll('.cal-cell').forEach(rect => rect.onclick = () => { const k = rect.dataset.key; calSelectedDay = calSelectedDay === k ? null : k; renderCalendar(); });
  const close = $('calendar').querySelector('#calDayClose');
  if (close) close.onclick = () => { calSelectedDay = null; renderCalendar(); };
  $('calendar').querySelectorAll('.cal-day-item').forEach(el => el.onclick = () => openSession(el.dataset.file));
}

// ---------- RINGS view (candidate home screen #3) — one growth-ring disc per project ----------
// Each project/repo becomes a disc, drawn like a cut tree trunk: the ring closest
// to the center is that project's OLDEST week of activity (in a bounded window),
// rings grow outward toward its most recent week. Ring thickness = how busy that
// week was; ring colour = how it went — green calm, amber a retry happened, red
// hit errors. A week with too few sessions to judge honestly renders as a thin
// grey band instead of guessing a colour from one data point. Click a disc to
// jump into Fleet filtered to that project.
const RING_MIN_SAMPLE = 3;   // "a handful" — below this, a week's ring is neutral, not dramatic
const RING_MAX_WEEKS = 10;   // ring cap per disc, so a years-old project doesn't sprawl off-screen
const RING_BASE_R = 15, RING_STEP = 9;
const RING_WEEK_MS = 6048e5; // 7 days

async function loadRings() {
  // refetch every time: any of these can be the home screen, and a home screen
  // that never updates is worse than no home screen at all
  try { fleetCache = await (await fetch('/api/fleet')).json(); } catch { fleetCache = fleetCache || []; }
  renderRings();
}
function ringWeekStart(ts) {
  const d = new Date(ts); d.setHours(0, 0, 0, 0);
  const day = (d.getDay() + 6) % 7; // Monday = 0
  d.setDate(d.getDate() - day);
  return d.getTime();
}
// Order matters: the old prefix strip ate the leading "C" that the Windows-path
// pattern needs, so every local project rendered mangled. Try the path form first
// and only fall through to the source-prefix strip when it does not match.
function cleanProjLabel(raw) {
  const s = String(raw || '');
  const win = s.replace(/^[Cc]--Users-[^-]+-/, '');
  return (win !== s ? win : s.replace(/^(⇄|Codex)\s*·?\s*/, '')) || 'Unknown';
}
function ringDiscSvg(sessions) {
  const anchor = ringWeekStart(sessions[sessions.length - 1].mtime); // sessions pre-sorted ascending
  const oldest = ringWeekStart(sessions[0].mtime);
  const spanAll = Math.round((anchor - oldest) / RING_WEEK_MS) + 1;
  const span = Math.max(1, Math.min(RING_MAX_WEEKS, spanAll));
  const byWeek = new Map();
  for (const s of sessions) {
    const wk = ringWeekStart(s.mtime);
    if (!byWeek.has(wk)) byWeek.set(wk, []);
    byWeek.get(wk).push(s);
  }
  const weeksShown = [];
  for (let i = 0; i < span; i++) weeksShown.push(byWeek.get(anchor - (span - 1 - i) * RING_WEEK_MS) || []);
  const maxN = Math.max(...weeksShown.map(w => w.length), 1);
  const R = RING_BASE_R + (span - 1) * RING_STEP + 6;
  let rings = '';
  weeksShown.forEach((wk, i) => {
    const r = RING_BASE_R + i * RING_STEP;
    const n = wk.length;
    let color = 'var(--line)', op = .35, sw = 2;
    // Same rule as the Rhythm clock: colour by the SHARE that went badly, never by
    // "did any of them", which turns one bad run in seventeen into a red year.
    const roughN = wk.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
    const rate = n ? Math.round(roughN / n * 100) : 0;
    if (n > 0) {
      sw = 2 + Math.round((n / maxN) * 6);
      if (n < RING_MIN_SAMPLE) { color = 'var(--dim)'; op = .55; }
      else {
        color = rate > 30 ? 'var(--red)' : rate >= 10 ? 'var(--amber)' : 'var(--green)';
        op = .92;
      }
    }
    const weekStr = new Date(anchor - (span - 1 - i) * RING_WEEK_MS).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
    const tip = n
      ? `Week of ${weekStr}: ${n} session${n === 1 ? '' : 's'}` + (n < RING_MIN_SAMPLE ? ' (too few to judge)' : ` · ${roughN} of ${n} hit trouble (${rate}%)`)
      : `Week of ${weekStr}: no runs`;
    rings += `<circle cx="0" cy="0" r="${r}" fill="none" stroke="${color}" stroke-width="${sw}" opacity="${op}"><title>${esc(tip)}</title></circle>`;
  });
  return `<svg viewBox="${-R - 4} ${-R - 4} ${(R + 4) * 2} ${(R + 4) * 2}" width="${(R + 4) * 2}" height="${(R + 4) * 2}" class="ring-svg">
    <circle cx="0" cy="0" r="${RING_BASE_R - 5}" fill="var(--panel2)" stroke="var(--line)"/>
    ${rings}
  </svg>`;
}
function renderRings() {
  const all = (fleetCache || []).filter(s => !metaOf(s).archived && s.mtime);
  const groups = new Map();
  for (const s of all) {
    if (!groups.has(s.project)) groups.set(s.project, []);
    groups.get(s.project).push(s);
  }
  const discs = [...groups.entries()].map(([proj, sessions]) => {
    sessions.sort((a, b) => a.mtime - b.mtime);
    const cost = sessions.reduce((n, s) => n + s.cost, 0);
    const rough = sessions.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
    return { proj, sessions, cost, rough };
  }).sort((a, b) => b.sessions.length - a.sessions.length || b.sessions[b.sessions.length - 1].mtime - a.sessions[a.sessions.length - 1].mtime);

  $('rings').innerHTML =
    `<div class="fleet-head"><h2>Projects — rings — ${discs.length} project${discs.length === 1 ? '' : 's'}</h2>${homeButton('rings')}</div>
    <div class="rings-legend">Each ring is a week — colour is the <b>share</b> of that week's runs that hit trouble: <span style="color:var(--green)">green</span> under 10%, <span style="color:var(--amber)">amber</span> 10–30%, <span style="color:var(--red)">red</span> over 30%, <span style="color:var(--dim)">grey</span> too few runs that week to judge. Thicker ring = busier week. Only the last ${RING_MAX_WEEKS} weeks are drawn: centre = ${RING_MAX_WEEKS} weeks ago, edge = most recent. Hover any ring for the real numbers; click a disc to see it in Fleet.</div>` +
    (discs.length === 0
      ? `<div class="fp-empty">No sessions yet — once you run something, its project gets a ring disc here.</div>`
      : `<div class="rings-grid">` + discs.map(d => {
        // A relayed session's "project" is its MACHINE name, so every remote repo
        // collapses into one disc. Say that plainly rather than let it read as a repo.
        const remote = d.sessions.every(s => /^(relay|otel|archive):/.test(s.file || ''));
        const label = (remote ? '🖥 ' : '') + cleanProjLabel(d.proj) + (remote ? ' — all remote work' : '');
        const n = d.sessions.length;
        // The disc only draws the last RING_MAX_WEEKS weeks, so the caption has to
        // describe that same window — an all-time total under a truncated picture
        // reads as if every one of those sessions is shown.
        const cutoff = ringWeekStart(d.sessions[d.sessions.length - 1].mtime) - (RING_MAX_WEEKS - 1) * RING_WEEK_MS;
        const shown = d.sessions.filter(s => s.mtime >= cutoff);
        const older = n - shown.length;
        const sn = shown.length;
        const sCost = shown.reduce((a, s) => a + (s.cost || 0), 0);
        const sRough = shown.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
        const summary = (sn < RING_MIN_SAMPLE
          ? `${sn} session${sn === 1 ? '' : 's'} shown — not enough runs to say yet`
          : `${sn} sessions shown, ~${fmtUsd(sCost)}${sRough ? `, ${sRough} rough run${sRough === 1 ? '' : 's'}` : ', all clean'}`)
          + (older ? ` · ${older} older not shown` : '');
        return `<div class="ring-card" data-proj="${esc(d.proj)}" title="Click to see ${esc(label)} in Fleet">
          ${ringDiscSvg(d.sessions)}
          <div class="ring-name">${esc(label)}</div>
          <div class="ring-summary">${esc(summary)}</div>
        </div>`;
      }).join('') + `</div>`);
  wireHomeButton($('rings'), 'rings', renderRings);
  $('rings').querySelectorAll('.ring-card').forEach(c => c.onclick = () => {
    fleetFilter = c.dataset.proj; fleetKind = 'all'; fleetMachine = 'all'; fleetProject = 'all'; fleetArchived = 'hide';
    state.view = 'fleet'; setTabs();
  });
}

// ---------- RHYTHM view (candidate home screen #4) — 24h polar clock + weekday strip ----------
// Wedge length = how many sessions started that hour, all-time, local clock time.
// Wedge colour = how those runs went, same green/amber/red/grey health scale (and
// same small-sample gate) as Rings. The weekday strip below is pure volume — no
// judgement painted onto it. The one interpretive claim on this page — do sessions
// that started overnight behave differently from the ones you were awake for —
// only prints when BOTH sides have at least 15 sessions; below that it says so
// plainly and shows the raw counts, nothing more. Small samples are exactly where
// a solo operator's "chronotype" story would otherwise lie.
const RHY_MIN_SAMPLE = 3;
const RHY_CHRONO_MIN = 15;
const RHY_WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

async function loadRhythm() {
  // refetch every time: any of these can be the home screen, and a home screen
  // that never updates is worse than no home screen at all
  try { fleetCache = await (await fetch('/api/fleet')).json(); } catch { fleetCache = fleetCache || []; }
  renderRhythm();
}
function rhySessionStart(s) { return s.mtime - (s.durationMs || 0); }
function polarWedgePath(cx, cy, rInner, rOuter, aStartDeg, aEndDeg) {
  const rad = d => (d - 90) * Math.PI / 180;
  const p = (r, a) => [cx + r * Math.cos(rad(a)), cy + r * Math.sin(rad(a))];
  const [x1, y1] = p(rInner, aStartDeg), [x2, y2] = p(rOuter, aStartDeg);
  const [x3, y3] = p(rOuter, aEndDeg), [x4, y4] = p(rInner, aEndDeg);
  const large = aEndDeg - aStartDeg > 180 ? 1 : 0;
  return `M${x1.toFixed(1)},${y1.toFixed(1)} L${x2.toFixed(1)},${y2.toFixed(1)} A${rOuter},${rOuter} 0 ${large} 1 ${x3.toFixed(1)},${y3.toFixed(1)} L${x4.toFixed(1)},${y4.toFixed(1)} A${rInner},${rInner} 0 ${large} 0 ${x1.toFixed(1)},${y1.toFixed(1)} Z`;
}
function rhyRoughRate(list) { return list.length ? Math.round(list.filter(s => s.errors > 0 || s.retrying || s.stalled).length / list.length * 100) : 0; }
function renderRhythm() {
  const all = (fleetCache || []).filter(s => !metaOf(s).archived && s.mtime);
  const hours = Array.from({ length: 24 }, () => []);
  const weekdays = Array.from({ length: 7 }, () => []);
  for (const s of all) {
    const d = new Date(rhySessionStart(s));
    hours[d.getHours()].push(s);
    weekdays[(d.getDay() + 6) % 7].push(s);
  }
  const maxHour = Math.max(...hours.map(h => h.length), 1);
  const CX = 130, CY = 130, R_IN = 26, R_OUT = 118;
  let wedges = '';
  for (let h = 0; h < 24; h++) {
    const list = hours[h], n = list.length;
    const len = R_IN + (n / maxHour) * (R_OUT - R_IN);
    // Colour from the SHARE of rough runs, never `some(...)`. A boolean OR over an
    // all-time bucket is monotone in sample size: the hours you use most are the
    // ones guaranteed to contain one bad run eventually, so they'd go red and stay
    // red forever — telling you the exact opposite of the truth.
    let color = 'var(--line)', op = .35;
    const roughN = list.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
    const rate = n ? Math.round(roughN / n * 100) : 0;
    if (n > 0) {
      if (n < RHY_MIN_SAMPLE) { color = 'var(--dim)'; op = .55; }
      else {
        color = rate > 30 ? 'var(--red)' : rate >= 10 ? 'var(--amber)' : 'var(--green)';
        op = .92;
      }
    }
    const hourLabel = h === 0 ? '12am' : h < 12 ? `${h}am` : h === 12 ? '12pm' : `${h - 12}pm`;
    const tip = n
      ? `${hourLabel}: ${n} session${n === 1 ? '' : 's'}` + (n < RHY_MIN_SAMPLE ? ' (too few to judge)' : ` · ${roughN} of ${n} hit trouble (${rate}%)`)
      : `${hourLabel}: no runs`;
    wedges += `<path d="${polarWedgePath(CX, CY, R_IN, n > 0 ? len : R_IN + 4, h * 15 - 6.5, h * 15 + 6.5)}" fill="${color}" opacity="${op}"><title>${esc(tip)}</title></path>`;
  }
  let ticks = '';
  [[0, '12am'], [6, '6am'], [12, '12pm'], [18, '6pm']].forEach(([h, label]) => {
    const rad = (h * 15 - 90) * Math.PI / 180;
    const lx = CX + (R_OUT + 15) * Math.cos(rad), ly = CY + (R_OUT + 15) * Math.sin(rad);
    ticks += `<text x="${lx.toFixed(1)}" y="${(ly + 4).toFixed(1)}" text-anchor="middle" class="rhy-tick">${label}</text>`;
  });
  const clockSvg = `<svg viewBox="0 0 260 260" width="260" height="260" class="rhy-clock">
    ${wedges}
    <circle cx="${CX}" cy="${CY}" r="${R_IN - 4}" fill="var(--panel2)" stroke="var(--line)"/>
    <text x="${CX}" y="${CY - 3}" text-anchor="middle" class="rhy-center-n">${all.length}</text>
    <text x="${CX}" y="${CY + 13}" text-anchor="middle" class="rhy-center-l">sessions</text>
    ${ticks}
  </svg>`;

  const maxWd = Math.max(...weekdays.map(w => w.length), 1);
  const WD_W = 34, WD_GAP = 10, WD_H = 90;
  let wdBars = '';
  weekdays.forEach((list, i) => {
    const n = list.length;
    const rough = list.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
    const h = (n / maxWd) * WD_H;
    const roughH = n ? (rough / n) * h : 0;
    const x = i * (WD_W + WD_GAP);
    // The red overlay is a rate too, so it obeys the same small-sample gate as the
    // clock: below the threshold the bar is plain volume and the tooltip says why.
    const judged = n >= RHY_MIN_SAMPLE;
    const tip = `${RHY_WEEKDAYS[i]}: ${n} session${n === 1 ? '' : 's'}` + (judged ? `${rough ? `, ${rough} of ${n} hit trouble` : ', all clean'}` : n ? ' (too few to judge)' : '');
    wdBars += `<g>
      <rect x="${x}" y="${(WD_H - h).toFixed(1)}" width="${WD_W}" height="${h.toFixed(1)}" rx="3" fill="var(--accent2)" opacity=".55"><title>${esc(tip)}</title></rect>
      ${rough && judged ? `<rect x="${x}" y="${(WD_H - h).toFixed(1)}" width="${WD_W}" height="${roughH.toFixed(1)}" rx="3" fill="var(--red)" opacity=".8"><title>${esc(tip)}</title></rect>` : ''}
      <text x="${x + WD_W / 2}" y="${WD_H + 16}" text-anchor="middle" class="rhy-wd-label">${RHY_WEEKDAYS[i]}</text>
    </g>`;
  });
  const wdSvg = `<svg viewBox="0 0 ${7 * (WD_W + WD_GAP) - WD_GAP} ${WD_H + 26}" width="${7 * (WD_W + WD_GAP) - WD_GAP}" height="${WD_H + 26}" class="rhy-wd">${wdBars}</svg>`;

  // chronotype: sessions started overnight (12am-6am) vs everything else
  const overnight = hours.slice(0, 6).flat();
  const daytime = hours.slice(6).flat();
  let chrono;
  if (overnight.length >= RHY_CHRONO_MIN && daytime.length >= RHY_CHRONO_MIN) {
    const rOv = rhyRoughRate(overnight), rDa = rhyRoughRate(daytime);
    const diff = rOv - rDa;
    // At 15 runs a side, a single extra bad session moves the rate ~7 points — so a
    // fixed 10-point threshold let the verdict flip on one run. Only call a real
    // difference when the gap clears the sampling error of BOTH samples.
    const se = p => Math.sqrt((p / 100) * (1 - p / 100));
    const band = 2 * 100 * Math.max(se(rOv) / Math.sqrt(overnight.length), se(rDa) / Math.sqrt(daytime.length));
    const verdict = Math.abs(diff) <= Math.max(band, 10)
      ? `too close to call — ${rOv}% vs ${rDa}% hit an error or retry, which is inside what ${overnight.length} and ${daytime.length} runs can actually tell you apart`
      : diff > 0
        ? `rougher: ${rOv}% hit an error or retry, vs ${rDa}% for the ones you started awake`
        : `actually cleaner: ${rOv}% hit an error or retry, vs ${rDa}% for the ones you started awake`;
    chrono = `<div class="rhy-chrono"><b>Overnight runs (started 12am–6am)</b> came out ${verdict}. Based on ${overnight.length} overnight and ${daytime.length} daytime/evening sessions.</div>`;
  } else {
    chrono = `<div class="rhy-chrono dim">Not enough overnight runs yet to say whether they behave differently — ${overnight.length} started 12am–6am so far, ${daytime.length} during the day/evening. Need at least ${RHY_CHRONO_MIN} on both sides before judging.</div>`;
  }

  $('rhythm').innerHTML =
    `<div class="fleet-head"><h2>Rhythm — when you run, and how it goes</h2>${homeButton('rhythm')}</div>
    <div class="rings-legend">Spoke length = sessions started that hour, all-time. Colour = how they went — <span style="color:var(--green)">green</span> calm, <span style="color:var(--amber)">amber</span> a retry happened, <span style="color:var(--red)">red</span> hit errors, <span style="color:var(--dim)">grey</span> too few runs that hour to judge.</div>
    <div class="rhy-wrap">
      <div class="rhy-col">${clockSvg}</div>
      <div class="rhy-col">
        <h3 class="rhy-h3">By day of week</h3>
        ${wdSvg}
        ${chrono}
      </div>
    </div>`;
  wireHomeButton($('rhythm'), 'rhythm', renderRhythm);
}

// ---------- PLAYBOOK STUDIO (mine the fleet, generate reusable plays) ----------
// Generation only — the studio designs plays from YOUR fleet's track record;
// you paste them to an agent yourself. It never executes anything.
let triageState = {}, triageFilter = 'open';
async function loadPlaybooks() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  if (!flowsCache) { $('playbooks').innerHTML = '<div class="fleet-loading">Studying your fleet’s track record…</div>'; await loadFlows.fetchOnly(); }
  await loadPlaybookLib();
  try { triageState = (await (await fetch('/api/triage')).json()).triage || {}; } catch { triageState = {}; }
  renderPlaybooks();
}
async function setTriage(key, status) {
  const r = await fetch('/api/triage', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ key, status }) });
  if (r.ok) { triageState = (await r.json()).triage || {}; renderPlaybooks(); }
}
const triageExpanded = new Set();
async function setTriageSilent(key, status) {
  try { await fetch('/api/triage', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ key, status }) }); } catch { /* ignore */ }
}
// classify an insight so each type gets the RIGHT action, not one template
function classifyInsight(i) {
  if (i.sev === 'good') return 'info';
  if (i.key.startsWith('failrole:') || i.key.startsWith('envrole:')) return 'failure';
  if (i.key.startsWith('hoard:') || i.key.startsWith('tier:') || i.key.startsWith('flatdeleg:')) return 'cost';
  return 'other';
}
function insightMachine(i) {
  if (i.detail && i.detail.byMachine) return Object.keys(i.detail.byMachine)[0] || 'This machine';
  if (i.file && i.file.startsWith('relay:')) return i.file.split(':')[1];
  return 'This machine';
}
// "Handle it for me": triage the noise, and give the right kind of action for
// each remaining issue — failures get one SYSTEMIC per-machine investigation
// (they usually share root causes), cost issues get the delegation/tier lever.
async function handleTriageForMe(insights, mined) {
  const open = insights.filter(i => i.key && !triageState[i.key]?.status);
  const info = open.filter(i => classifyInsight(i) === 'info');
  const failures = open.filter(i => classifyInsight(i) === 'failure');
  const costs = open.filter(i => classifyInsight(i) === 'cost');
  for (const i of info) await setTriageSilent(i.key, 'dismissed'); // not problems

  // failures grouped by machine → ONE systemic investigation each (don't fix 75 roles one-by-one)
  const byMachine = {};
  for (const i of failures) (byMachine[insightMachine(i)] = byMachine[insightMachine(i)] || []).push(i);
  const machineActions = Object.entries(byMachine).map(([machine, list]) => {
    const worst = [...list].sort((a, b) => (b.detail?.runs ? (1 - b.detail.clean / b.detail.runs) : 0) - (a.detail?.runs ? (1 - a.detail.clean / a.detail.runs) : 0));
    const names = worst.map(i => (i.detail?.role || (i.text.match(/"([^"]+)"/) || [])[1] || '').trim()).filter(Boolean);
    const lines = [
      `Agent Mission Control flags ${list.length} failing agent role${list.length !== 1 ? 's' : ''} on ${machine}. Do NOT fix them one at a time — find the SHARED root causes.`, '',
      `Worst offenders: ${names.slice(0, 8).join(', ')}${names.length > 8 ? ', …' : ''}.`, '',
      `1. Read the errored tool calls from 8-12 of the worst roles.`,
      `2. Cluster the failures by cause (e.g. wrong tool/DB access, path/permission errors, guessed schema, missing context). Most roles here likely share 2-4 root causes.`,
      `3. Fix the SHARED scaffolding — the common RULES block / workflow template these roles inherit — not each role separately. Report: the clusters, how many roles each affects, and the before/after of the shared block.`,
      `Don't apply anything without showing me the plan first.`,
    ];
    return { machine, count: list.length, paste: lines.join('\n') };
  });

  // cost issues → the delegation/tier lever (not a prompt fix)
  const costLines = costs.map(i => '• ' + i.text.replace(/\s+/g, ' '));
  const tieredPaste = buildPlaybook('tiered', mined);

  const ov = document.createElement('div');
  ov.className = 'pb-editor-ov'; ov.onclick = e => { if (e.target === ov) ov.remove(); };
  ov.innerHTML = `<div class="handle-modal">
    <div class="hm-head"><b>✨ Here's what I did</b><button class="mini-btn" id="hmClose">✕</button></div>
    <div class="hm-body">
      <p>Closed <b>${info.length}</b> informational note${info.length !== 1 ? 's' : ''} (not problems). Sorted the rest into two kinds, each with the right fix:</p>

      ${failures.length ? `<h4 class="hm-h">🛠 ${failures.length} failing role${failures.length !== 1 ? 's' : ''} — one investigation per machine</h4>
        <p class="dim">Roles failing in the same repo almost always share a few root causes. Send each machine ONE systemic investigation, not a list of every role.</p>
        ${machineActions.map((m, i) => `<div class="hm-machine"><div class="hm-m-head"><b>🖥 ${esc(m.machine)}</b><span class="dim">${m.count} role${m.count !== 1 ? 's' : ''}</span><button class="mini-btn hm-copy" data-t="fail" data-i="${i}" style="margin-left:auto">📋 copy</button></div><pre class="hm-paste">${esc(m.paste)}</pre></div>`).join('')}` : ''}

      ${costs.length ? `<h4 class="hm-h">💸 ${costs.length} cost / efficiency finding${costs.length !== 1 ? 's' : ''} — not prompt bugs</h4>
        <p class="dim">These are spend, not failures. The fix is architectural: let the orchestrator plan and delegate the grunt work to cheaper models. Copy the tiered playbook and use it going forward.</p>
        <div class="hm-machine"><div class="hm-m-head"><b>The findings</b><span style="margin-left:auto;display:flex;gap:6px"><button class="mini-btn" id="hmDirective" title="make it permanent: plant the tiering rule into repo guidance files">🛰 make it a rule</button><button class="mini-btn hm-copy" data-t="tiered">📋 copy tiered playbook</button></span></div><pre class="hm-paste">${esc(costLines.join('\n'))}\n\n— Best fix: 🛰 plant the tiering rule (permanent), or copy the playbook (one session) —</pre></div>` : ''}

      ${!failures.length && !costs.length ? '<p class="dim">Nothing action-needed — you\'re clear. 🎉</p>' : ''}
    </div>
    <div class="hm-foot">
      <span class="dim">Clear these from the Open list once sent:</span>
      <button class="mini-btn pbe-save" id="hmResolve">✓ Mark all handled</button>
    </div></div>`;
  document.body.appendChild(ov);
  ov.querySelector('#hmClose').onclick = () => { ov.remove(); loadPlaybooks(); };
  const hmDir = ov.querySelector('#hmDirective');
  if (hmDir) hmDir.onclick = () => openDirectiveComposer(directiveTemplate({ key: 'tier:handle' }));
  ov.querySelectorAll('.hm-copy').forEach(b => b.onclick = () => {
    const txt = b.dataset.t === 'tiered' ? tieredPaste : machineActions[+b.dataset.i].paste;
    navigator.clipboard.writeText(txt); const o = b.textContent; b.textContent = '✓ copied'; setTimeout(() => { b.textContent = o; }, 1500);
  });
  ov.querySelector('#hmResolve').onclick = async () => {
    for (const i of [...failures, ...costs]) await setTriageSilent(i.key, 'resolved');
    ov.remove(); loadPlaybooks();
  };
  triageState = (await (await fetch('/api/triage')).json()).triage || {};
}
// ---------- standing orders (directives planted into repo guidance files) ----------
let directiveReg = { items: [], targets: [] };
let directivesLoaded = false;
let ordersView = 'list'; // 'list' | 'coverage' — standing orders panel toggle
let coverageChecks = {}; // directive id -> statuses array from the last "check" run (drives ⚠ in the coverage grid)
let gitStates = {};      // directive id -> per-target git state; filled only when the owner asks, since each check runs real git commands
let gitNotes = {};       // `${directive id}|${file path}` -> plain result of the last commit/send on that file
const trunc = (s, n) => (s && s.length > n) ? s.slice(0, n - 1) + '…' : (s || '');
async function loadDirectiveReg() {
  try { directiveReg = await (await fetch('/api/directives')).json(); } catch { directiveReg = { items: [], targets: [] }; }
}
// prefill the composer with the right standing order for each insight type
function directiveTemplate(i) {
  const k = (i && i.key) || '';
  if (k.startsWith('tier:') || k.startsWith('flatdeleg:') || k.startsWith('hoard:')) return {
    title: 'Tier your models — premium only where judgment matters',
    body: [
      'When orchestrating multi-agent work in this project:',
      '- Orchestrator / final synthesis / adversarial verdict → premium (Opus 5); flagship (Fable 5) only for the hardest long-horizon planning.',
      '- Review / research / accuracy-check workers → claude-sonnet-5.',
      '- Pure fetch / grep / read / summarize helpers → claude-haiku-4-5.',
      '- Delegation only saves money when workers run a CHEAPER tier than the orchestrator — never let subagents silently inherit the premium model.',
      '',
      'Reserve premium tokens for planning and the final judgment gate. Report a per-tier cost breakdown when a workflow finishes so the savings are visible.',
    ].join('\n') };
  if (k.startsWith('failrole:')) {
    const role = i.detail?.role || 'this role';
    const failPct = i.detail?.runs ? Math.round((1 - i.detail.clean / i.detail.runs) * 100) : null;
    return { title: `Fix before reuse: the failing "${role}" agent`, body: [
      `Fleet history: the subagent role "${role}" fails ${failPct != null ? failPct + '% of' : 'many of'} its runs in this project.`,
      'Before spawning it again:',
      "- State the environment facts it keeps guessing wrong (real paths, schemas, access patterns) IN its prompt — don't make it rediscover them.",
      '- Give it one narrow task and an explicit output contract.',
      '- If it fails, read its transcript and fix the prompt or the shared scaffolding it inherits — never just retry.',
    ].join('\n') };
  }
  if (k.startsWith('envrole:')) return {
    title: `Pin the environment for "${i.detail?.role || 'this role'}"`,
    body: `Fleet history: "${i.detail?.role || 'this role'}" succeeds on one machine and fails on another — the difference is environment, not the model. Before delegating this role, state which machine and tools it needs, and don't spawn it where its dependencies are missing.\n\nContext: ${i.text}` };
  return { title: 'Fleet learning', body: (i && i.text) || '' };
}
// honest before/after: top-tier share of spend in sessions since the plant date
function directiveImpact(d, mined) {
  const before = { top: 0, total: 0, n: 0 }, after = { top: 0, total: 0, n: 0 };
  for (const x of mined.sessions) {
    const t = x.mainCost + x.subCost; if (t <= 0) continue;
    const b = (x.s.mtime || 0) >= d.createdAt ? after : before;
    b.top += x.topCost; b.total += t; b.n++;
  }
  if (after.n < 5) return `${after.n} costed session${after.n === 1 ? '' : 's'} since planted — too early to judge impact.`;
  if (!before.total) return `${after.n} sessions since planted; no before-data to compare against.`;
  const bp = Math.round(before.top / before.total * 100), ap = Math.round(after.top / after.total * 100);
  const dir = ap < bp ? '↓ improving' : ap > bp ? '↑ not helping yet' : '→ flat';
  return `Top-tier spend share: ${bp}% before → ${ap}% after (${after.n} sessions) ${dir}. Small samples drift — treat as a trend, not proof.`;
}
// a planted rule shouldn't live forever unquestioned — due when reviewEveryDays have
// passed since it was last looked at (or since it was planted, if never reviewed)
const DAY_MS = 86400000;
function directiveDue(d) {
  const every = d.reviewEveryDays || 30;
  const since = d.lastReviewedAt || d.createdAt;
  return Date.now() - since >= every * DAY_MS;
}
// ---- conflict sentry: two standing orders on the same topic can fight each other ----
// Topic is derived deterministically from the insight key + title text — no LLM, same
// answer every time. Small rule table; first match wins, else 'general'.
const TOPIC_RULES = [
  [/tier|model|premium|sonnet|haiku|opus/i, 'model-tiering'],
  [/retry|fail|flaky/i, 'failure-handling'],
  [/machine|environment/i, 'environment'],
];
function directiveTopic(insightKey, title) {
  const s = `${insightKey || ''} ${title || ''}`;
  for (const [re, topic] of TOPIC_RULES) if (re.test(s)) return topic;
  return 'general';
}
const TOPIC_LABELS = { 'model-tiering': 'model tiering', 'failure-handling': 'failure handling', environment: 'machine/environment setup', general: 'this topic' };
const topicLabel = t => TOPIC_LABELS[t] || t;
// backward-compatible: old records planted before topics existed don't have d.topic
const dirTopic = d => d.topic || directiveTopic(d.insightKey, d.title);
// active orders sharing a topic AND at least one planted file with the given directive
function overlappingDirectives(topic, targetPaths, excludeId) {
  return (directiveReg.items || []).filter(o => o.id !== excludeId && dirTopic(o) === topic && (o.targets || []).some(t => targetPaths.has(t.path)));
}
// coverage matrix: rows = every plantable target, columns = active standing orders,
// cells show whether that order reaches that file. Answers "where does each rule
// actually reach?" at a glance instead of reading each dir-item card one at a time.
function coverageGridHTML() {
  const targets = directiveReg.targets || [];
  const items = directiveReg.items || [];
  if (!targets.length) return '<div class="dim">No known repos yet — open some sessions first so I can learn where your projects live.</div>';
  if (!items.length) return '<div class="dim">None yet — hit <b>🛰 make it a rule</b> on any insight above, or ＋ new order.</div>';
  const coveredTargets = targets.filter(t => items.some(d => (d.targets || []).some(x => x.path === t.path)));
  return `
    <div class="dim" style="margin-bottom:8px">${coveredTargets.length} of ${targets.length} places are covered by at least one rule. Click an empty cell to plant that order there too.</div>
    <div class="cov-wrap">
      <table class="cov-table">
        <thead><tr><th class="cov-corner"></th>${items.map(d => `<th title="${esc(d.title)}">${esc(trunc(d.title, 16))}</th>`).join('')}</tr></thead>
        <tbody>${targets.map(t => `<tr>
            <th class="cov-row-label" title="${esc(t.path)}">${esc(t.label)}<div class="dim">${esc(t.name)}</div></th>
            ${items.map(d => {
              const planted = (d.targets || []).some(x => x.path === t.path);
              const checked = (coverageChecks[d.id] || []).find(s => s.path === t.path);
              const drifted = planted && checked && checked.status !== 'ok';
              const cls = drifted ? 'warn' : planted ? 'on' : 'off';
              const symbol = drifted ? '⚠' : planted ? '✓' : '·';
              const label = drifted ? 'Drifted — removed or edited away since it was planted. "check all" to refresh.'
                : planted ? 'Covered.' : `Not covered — click to plant "${d.title}" here too.`;
              return `<td class="cov-cell ${cls}" data-target="${esc(t.id)}" data-dir="${esc(d.id)}" title="${esc(label)}">${symbol}</td>`;
            }).join('')}
          </tr>`).join('')}</tbody>
      </table>
    </div>
    <button id="covCheckAll" class="mini-btn" style="margin-top:8px">🔍 check all, refresh drift</button>`;
}
// ---- share via git: the safe way a planted rule reaches your other machines ----
// Nothing is written to another machine. The rule is committed into the repo it
// already lives in; the other machines get it with git pull, and git revert takes
// it back everywhere. Commit and send are deliberately two separate buttons —
// send is the only step that leaves this computer, so it is never automatic.
function gitRowsHTML(d) {
  const states = gitStates[d.id];
  if (!states) return '<button class="mini-btn dir-git-check">🔗 see where this can be shared</button>';
  if (!states.length) return '<div class="dim">No files recorded for this order yet.</div>';
  return states.map(s => {
    if (s.truncated) return `<div class="dg-row dim">…and ${s.truncated} more place${s.truncated === 1 ? '' : 's'} not shown here.</div>`;
    const note = gitNotes[d.id + '|' + s.path] || '';
    if (!s.isRepo) return `<div class="dg-row"><div><b>${esc(s.label)}</b> <span class="dim">${esc(s.name)} — ${s.gitMissing ? 'git is not installed on this computer, so nothing can be shared from here.' : 'not a git repo, so there is nothing to commit.'}</span></div></div>`;
    // .gitignore'd files are invisible to git: a clean `status` here means "never
    // tracked", not "already saved". Say the true thing and offer no dead buttons.
    if (s.ignored) return `<div class="dg-row"><div><b>${esc(s.label)}</b> <span class="dim">${esc(s.name)} — this file is listed in .gitignore, so git deliberately does not track it. It cannot be shared this way.</span></div></div>`;
    const onBranch = !!s.branch;
    const committed = s.committed === true;
    const bits = [
      onBranch ? `on branch ${s.branch}` : 'not on a branch right now — commit would be lost',
      committed ? 'this rule is saved in the repo history' : 'this rule is not committed yet',
      s.ahead == null ? 'no remote set up, so nothing can be sent' : s.ahead ? `${s.ahead} commit${s.ahead === 1 ? '' : 's'} waiting to be sent` : 'nothing waiting to be sent',
    ];
    return `<div class="dg-row">
      <div><b>${esc(s.label)}</b> <span class="dim">${esc(s.name)} — ${esc(bits.join(' · '))}</span></div>
      <div class="dg-btns">
        <button class="mini-btn dg-commit" data-path="${esc(s.path)}"${(!onBranch || committed) ? ' disabled' : ''} title="${onBranch ? 'save this rule into the repo\'s history on this computer only' : 'switch to a branch first — a commit made here would be thrown away'}">💾 Commit it here</button>
        <button class="mini-btn dg-push" data-path="${esc(s.path)}"${s.ahead ? '' : ' disabled'} title="send everything committed on this branch to the shared remote — this leaves your computer">⬆ Send to the remote</button>
      </div>
      ${note ? `<div class="dg-note dim">${esc(note)}</div>` : ''}
    </div>`;
  }).join('') + '<button class="mini-btn dir-git-check">↻ re-check</button>';
}
// checkbox list for the composer — pulled out so add/remove-root can redraw it in place
function dirTargetsHTML(targets) {
  // Two folders can share a basename ("app" under two different projects) and would
  // render as two identical checkboxes. Name the parent on the ones that collide,
  // and hang the full path off every row as a tooltip.
  const seen = {};
  for (const t of targets) seen[t.label + '|' + t.name] = (seen[t.label + '|' + t.name] || 0) + 1;
  return targets.map(t => {
    const parent = seen[t.label + '|' + t.name] > 1 ? (String(t.path || '').split(/[\\/]+/).slice(-3, -2)[0] || '') : '';
    return `
        <label class="dir-target" title="${esc(t.path || t.label)}"><input type="checkbox" data-id="${esc(t.id)}"> <b>${esc(t.label)}</b>${parent ? ` <span class="dim">in ${esc(parent)}</span>` : ''} <span class="dim">${esc(t.name)}${t.exists ? '' : ' · will be created'}</span></label>`;
  }).join('') || '<div class="dim">No known repos yet — open some sessions first so I can learn where your projects live.</div>';
}
// owner-added folders list — remove only stops offering it, never touches planted files
function dirRootsHTML(roots) {
  if (!roots || !roots.length) return '<div class="dim">No folders added by hand yet.</div>';
  return roots.map(r => `
        <div class="dir-root-row${r.ok === false ? ' dir-root-dead' : ''}"><span title="${esc(r.path)}"><b>${esc(r.label || r.path)}</b> <span class="dim">${esc(r.path)}${r.ok === false ? ' — can’t find this folder right now, so it offers nothing' : ''}</span></span><button class="mini-btn dir-root-rm" data-path="${esc(r.path)}">✕ remove</button></div>`).join('');
}
function openDirectiveComposer(pre) {
  pre = pre || {};
  const ov = document.createElement('div');
  ov.className = 'pb-editor-ov'; ov.onclick = e => { if (e.target === ov) ov.remove(); };
  let targets = directiveReg.targets || [];
  let roots = directiveReg.roots || [];
  ov.innerHTML = `<div class="handle-modal">
    <div class="hm-head"><b>🛰 Plant a standing order</b><button class="mini-btn" id="dcClose">✕</button></div>
    <div class="hm-body">
      <p class="dim">This appends a marked block to the guidance file (CLAUDE.md / AGENTS.md) of each repo you pick — every future agent session there reads it automatically. Fully reversible: retire it later and the block is removed. Every file is snapshotted before it's touched.</p>
      <label class="dim">Title</label>
      <input id="dcTitle" class="pbe-name" style="width:100%;margin:4px 0 8px" value="${esc(pre.title || '')}" placeholder="e.g. Tier your models">
      <label class="dim">The order (markdown)</label>
      <textarea id="dcBody" class="dc-body">${esc(pre.body || '')}</textarea>
      <label class="dim">Remind me to review this every <input id="dcReview" type="number" min="1" max="3650" value="${esc(pre.reviewEveryDays || 30)}" style="width:56px"> days</label>
      <div class="hm-m-head" style="margin-top:10px"><b>Where to plant it</b><button class="mini-btn" id="dcAll" style="margin-left:auto">toggle all</button></div>
      <div class="dir-targets" id="dcTargets">${dirTargetsHTML(targets)}</div>
      <p class="dim" style="margin-top:8px">Only folders you've used Claude Code in show up automatically — add any other project folder here.</p>
      <div class="dir-root-add">
        <input id="dcRootPath" type="text" placeholder="full folder path on this computer, e.g. C:\\Users\\you\\GitHub\\my-repo">
        <button class="mini-btn" id="dcRootAddBtn">＋ Add</button>
      </div>
      <div id="dcRootErr" class="dim" style="margin-top:4px"></div>
      <div class="dir-roots" id="dcRootList">${dirRootsHTML(roots)}</div>
      <div id="dcConflict" class="dir-conflict" style="display:none"></div>
      <div id="dcResult" class="dim" style="margin-top:8px"></div>
    </div>
    <div class="hm-foot">
      <span class="dim">Local machine only. Every plant is snapshotted + audited.</span>
      <button class="mini-btn pbe-save" id="dcPlant">🛰 Plant</button>
    </div></div>`;
  document.body.appendChild(ov);
  ov.querySelector('#dcClose').onclick = () => ov.remove();
  // NB: setting .checked in code fires no 'change' event, so re-arm the conflict
  // check by hand — otherwise "toggle all" could smuggle unchecked repos past it.
  ov.querySelector('#dcAll').onclick = () => { const cbs = [...ov.querySelectorAll('.dir-target input')]; const on = cbs.some(c => !c.checked); cbs.forEach(c => { c.checked = on; }); resetConfirm(); };
  // conflict sentry: any edit after a warning invalidates the "plant anyway" confirmation
  let overlapConfirmed = false;
  const resetConfirm = () => { overlapConfirmed = false; ov.querySelector('#dcConflict').style.display = 'none'; ov.querySelector('#dcPlant').textContent = '🛰 Plant'; };
  ov.querySelector('#dcTitle').addEventListener('input', resetConfirm);
  ov.querySelectorAll('.dir-target input').forEach(c => c.addEventListener('change', resetConfirm));
  // remove-root buttons — rewired each time the roots list is redrawn
  const wireRootRemove = () => {
    ov.querySelectorAll('.dir-root-rm').forEach(b => b.onclick = async () => {
      b.disabled = true;
      try {
        const r = await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'remove-root', path: b.dataset.path }) });
        const j = await r.json();
        if (!r.ok || j.error) throw new Error(j.error || 'failed');
        // rules planted there stay live but stop appearing anywhere — say so
        if (j.stranded) ov.querySelector('#dcRootErr').textContent = `Removed from the list. Note: ${j.stranded} rule${j.stranded === 1 ? ' is' : 's are'} still planted in that folder and stay${j.stranded === 1 ? 's' : ''} active — add the folder back if you want to retire ${j.stranded === 1 ? 'it' : 'them'}.`;
        await refreshDirTargets();
      } catch (e) { b.disabled = false; ov.querySelector('#dcRootErr').textContent = 'Failed: ' + e.message; }
    });
  };
  wireRootRemove();
  // re-fetch the registry and redraw the checkbox list + owner-added-folder list in
  // place, preserving whatever the owner had already checked — used after add/remove-root
  const refreshDirTargets = async () => {
    const checkedIds = new Set([...ov.querySelectorAll('.dir-target input:checked')].map(c => c.dataset.id));
    await loadDirectiveReg();
    targets = directiveReg.targets || [];
    roots = directiveReg.roots || [];
    ov.querySelector('#dcTargets').innerHTML = dirTargetsHTML(targets);
    ov.querySelector('#dcRootList').innerHTML = dirRootsHTML(roots);
    ov.querySelectorAll('.dir-target input').forEach(c => { if (checkedIds.has(c.dataset.id)) c.checked = true; c.addEventListener('change', resetConfirm); });
    wireRootRemove();
    resetConfirm();
  };
  const rootInput = ov.querySelector('#dcRootPath');
  const rootErr = ov.querySelector('#dcRootErr');
  const doAddRoot = async () => {
    const p = rootInput.value.trim();
    if (!p) { rootErr.textContent = 'Paste a folder path first.'; return; }
    const btn = ov.querySelector('#dcRootAddBtn');
    btn.disabled = true; rootErr.textContent = 'Checking…';
    try {
      const r = await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'add-root', path: p }) });
      const j = await r.json();
      if (!r.ok || j.error) throw new Error(j.error || "That folder doesn't exist on this computer.");
      rootInput.value = '';
      rootErr.textContent = j.note ? 'Already on the list.' : 'Added.';
      await refreshDirTargets();
    } catch (e) { rootErr.textContent = e.message; }
    btn.disabled = false;
  };
  ov.querySelector('#dcRootAddBtn').onclick = doAddRoot;
  rootInput.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); doAddRoot(); } };
  ov.querySelector('#dcPlant').onclick = async () => {
    const ids = [...ov.querySelectorAll('.dir-target input:checked')].map(c => c.dataset.id);
    const title = ov.querySelector('#dcTitle').value.trim();
    const body = ov.querySelector('#dcBody').value.trim();
    const reviewEveryDays = parseInt(ov.querySelector('#dcReview').value, 10) || 30;
    const out = ov.querySelector('#dcResult');
    const conflictBox = ov.querySelector('#dcConflict');
    if (!title || !body) { out.textContent = 'Give it a title and a body first.'; return; }
    if (!ids.length) { out.textContent = 'Pick at least one repo to plant it in.'; return; }
    const topic = directiveTopic(pre.insightKey, title);
    if (!overlapConfirmed) {
      const idToPath = new Map(targets.map(t => [t.id, t.path]));
      const chosenPaths = new Set(ids.map(id => idToPath.get(id)).filter(Boolean));
      const overlaps = overlappingDirectives(topic, chosenPaths, null);
      if (overlaps.length) {
        overlapConfirmed = true;
        conflictBox.style.display = '';
        conflictBox.innerHTML = overlaps.map(o => {
          const sharedLabel = (o.targets || []).find(t => chosenPaths.has(t.path))?.label || 'a shared repo';
          return `<div>Heads up — you already have a rule about <b>${esc(topicLabel(topic))}</b> planted in ${esc(sharedLabel)}: <b>${esc(o.title)}</b>. Two rules on the same topic can contradict each other. <button type="button" class="mini-btn dc-view-overlap" data-id="${esc(o.id)}">see it</button></div>`;
        }).join('');
        conflictBox.querySelectorAll('.dc-view-overlap').forEach(b => b.onclick = () => {
          const id = b.dataset.id;
          ov.remove();
          // the card only exists in list view — coverage view renders a grid instead
          if (ordersView !== 'list') { ordersView = 'list'; if (state.view === 'playbooks') renderPlaybooks(); }
          if (state.view !== 'playbooks') { state.view = 'playbooks'; setTabs(); render(); }
          setTimeout(() => {
            const card = document.querySelector(`.dir-item[data-id="${CSS.escape(id)}"]`);
            if (card) { card.scrollIntoView({ behavior: 'smooth', block: 'center' }); card.classList.add('dir-flash'); setTimeout(() => card.classList.remove('dir-flash'), 2000); }
          }, 60);
        });
        ov.querySelector('#dcPlant').textContent = '🛰 Plant anyway';
        return; // don't plant yet — owner has to see the warning and click again
      }
    }
    out.textContent = 'Planting…';
    try {
      const r = await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'plant', title, body, reviewEveryDays, targets: ids, insightKey: pre.insightKey || null, topic }) });
      const j = await r.json();
      if (!r.ok || j.error) throw new Error(j.error || 'failed');
      directiveReg.items = j.items;
      const ok = (j.results || []).filter(x => x.status === 'planted').length;
      const skipped = (j.results || []).filter(x => x.status !== 'planted');
      out.textContent = `Planted into ${ok} file${ok === 1 ? '' : 's'}.` + (skipped.length ? ' Skipped: ' + skipped.map(x => `${x.label || '?'} (${x.status})`).join(', ') : '');
      if (pre.insightKey && ok) await setTriageSilent(pre.insightKey, 'resolved');
      setTimeout(() => { ov.remove(); if (state.view === 'playbooks') renderPlaybooks(); }, 1400);
    } catch (e) { out.textContent = 'Failed: ' + e.message; }
  };
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

// model tier from an id string (for token-saving analysis)
function modelTier(model) {
  const m = String(model || '').toLowerCase();
  if (/fable|mythos/.test(m)) return 'flagship';
  if (/opus|gpt-5|o3|o1/.test(m)) return 'premium';
  if (/sonnet|gpt-4|codex/.test(m)) return 'mid';
  if (/haiku|mini|flash|gpt-3/.test(m)) return 'cheap';
  return 'unknown';
}
const TIER_RATE = { flagship: 50, premium: 25, mid: 15, cheap: 5 }; // rough $/Mtok output, for savings math

// ---------- anatomy of a failure (deterministic — no LLM, no statistics) ----------
// One plain sentence for what a failed/stalled session got stuck on, computed by
// pure arithmetic over the event list AMC already parses. No caption beats a wrong
// one, so this returns null the moment the evidence isn't clean rather than guess.
function diagSig(e) {
  // stable short form of the key argument so repeats of the same call compare equal
  const raw = String(e.full || e.text || '').trim().replace(/\s+/g, ' ');
  return (e.tool || '') + '::' + raw.slice(0, 160);
}
function diagTarget(e) {
  let s = String(e.full || e.text || '').trim().replace(/\s+/g, ' ');
  if (/[\\/]/.test(s)) { const seg = s.split(/[\\/]/).filter(Boolean); if (seg.length > 2) s = seg.slice(-2).join('/'); }
  return s.length > 60 ? s.slice(0, 59) + '…' : s;
}
function diagnoseFailure(sessionData) {
  const events = (sessionData && sessionData.events) || [];
  const calls = events.filter(e => e.kind === 'tool-call' && e.tool);
  if (calls.length < 2) return null;

  // a long-pending final tool call with no result, and nothing meaningful after it
  const lastCall = calls[calls.length - 1];
  const lastIdx = events.indexOf(lastCall);
  const after = events.slice(lastIdx + 1);
  const hasResult = lastCall.toolUseId && after.some(e => e.kind === 'tool-result' && e.toolUseId === lastCall.toolUseId);
  const movedOn = after.some(e => e.kind === 'tool-call' || e.kind === 'assistant-text' || e.kind === 'tool-result');
  // OTEL sessions never emit tool-result events at all, so "no result" proves
  // nothing there and every finished trace looked stalled. Without a correlation
  // id we cannot tell "hung" from "results aren't recorded" — so say nothing.
  const canSeeResults = events.some(e => e.kind === 'tool-result');
  if (lastCall.toolUseId && canSeeResults && !hasResult && !movedOn) {
    return { kind: 'stalled', tool: lastCall.tool, target: diagTarget(lastCall), count: 1 };
  }

  // runs of consecutive identical call signatures, and whether any call in the
  // run was answered with an error (that's what turns a loop into a retry storm)
  const errored = new Set();
  for (const e of events) if (e.kind === 'tool-result' && e.error && e.toolUseId) errored.add(e.toolUseId);
  // Repeatedly editing ONE file is ordinary work, not a symptom — and the event
  // summary collapses every Edit to its file path, so those runs looked identical
  // when the actual edits differed. Write-style tools are excluded from run
  // detection entirely. Runs are also scanned PER AGENT: five agents each calling
  // a tool once is not one agent calling it five times.
  const WRITEY = /^(edit|write|multiedit|notebookedit)$/i;
  const runs = [];
  const byAgent = {};
  for (const c of calls) (byAgent[c.agent || 'main'] = byAgent[c.agent || 'main'] || []).push(c);
  for (const seq of Object.values(byAgent)) {
    for (let i = 0; i < seq.length;) {
      let j = i + 1;
      while (j < seq.length && diagSig(seq[j]) === diagSig(seq[i])) j++;
      const len = j - i;
      if (len >= 2 && !WRITEY.test(seq[i].tool || '')) {
        const slice = seq.slice(i, j);
        const errs = slice.filter(c => c.toolUseId && errored.has(c.toolUseId)).length;
        runs.push({ len, errs, hasError: errs > 0, allErrored: errs === len, tool: seq[i].tool, target: diagTarget(seq[i]) });
      }
      i = j;
    }
  }
  if (!runs.length) return null;
  // most recent qualifying run — that's what the session was doing right before it stopped
  const retryRuns = runs.filter(r => r.hasError);
  if (retryRuns.length) { const r = retryRuns[retryRuns.length - 1]; return { kind: 'retry-storm', tool: r.tool, target: r.target, count: r.len, errs: r.errs, allErrored: r.allErrored }; }
  const loopRuns = runs.filter(r => r.len >= 3);
  if (loopRuns.length) { const r = loopRuns[loopRuns.length - 1]; return { kind: 'looping', tool: r.tool, target: r.target, count: r.len }; }
  return null;
}
function failureSentence(diag) {
  if (!diag) return '';
  const tool = diag.tool || 'a tool', target = diag.target;
  if (diag.kind === 'stalled') return `Stopped waiting on ${tool}${target ? ' (' + target + ')' : ''} — no response ever came back.`;
  // only claim "failing each time" when every call in the run actually errored
  if (diag.kind === 'retry-storm') return diag.allErrored
    ? `Ran the same ${tool}${target ? ' — ' + target : ''} ${diag.count} times, failing each time.`
    : `Ran the same ${tool}${target ? ' — ' + target : ''} ${diag.count} times, failing ${diag.errs} of them.`;
  if (diag.kind === 'looping') return `Repeated the same ${tool}${target ? ' to ' + target : ''} ${diag.count} times, then stopped.`;
  return '';
}
// does the currently open session actually look broken? (never caption a healthy one)
function sessionFailed() {
  const agents = state.data.agents || [];
  const evs = state.data.events || [];
  const now = state.data.now || Date.now();
  if (evs.some(e => e.error)) return true;
  return agents.some(a => a.lastErrored || a.retrying || (a.pendingTool && a.pendingTool.since && now - new Date(a.pendingTool.since) > 120000));
}

function mineFleet() {
  const data = flowsCache || [];
  const norm = n => String(n || '').replace(/\s*#\d+$/, '').replace(/[0-9a-f-]{12,}/g, '·').slice(0, 30);
  const roles = new Map();
  const sessions = [];
  const insights = []; // built up while walking sessions below, and again after
  const models = new Map(); // model -> {agents, cost, outTok, roles:Set}
  const isTop = t => t === 'flagship' || t === 'premium';
  for (const { s, d } of data) {
    const main = d.agents.find(a => a.id === 'main');
    const subs = d.agents.filter(a => a.id !== 'main');
    const subCost = subs.reduce((n, a) => n + (a.cost || 0), 0);
    const subTopCost = subs.reduce((n, a) => n + (isTop(modelTier(a.model)) ? (a.cost || 0) : 0), 0);
    const mainCost = main ? (main.cost || 0) : 0;
    const topCost = subTopCost + (main && isTop(modelTier(main.model)) ? mainCost : 0);
    const sessErrors = d.events.filter(e => e.error).length;
    sessions.push({ s, agents: d.agents.length, errors: sessErrors, mainCost, subCost, subTopCost, topCost, roles: subs.map(a => norm(a.name)) });
    // anatomy of a failure: only for sessions that actually broke, and only
    // when the arithmetic finds real evidence — never a caption on a guess
    const broke = sessErrors > 0 || s.stalled || s.retrying || d.agents.some(a => a.lastErrored);
    if (broke) {
      const diag = diagnoseFailure(d);
      if (diag) insights.push({ key: 'diag:' + s.session, sev: 'bad', icon: '💥', text: `"${(s.title || s.session.slice(0, 8)).slice(0, 40)}" — ${failureSentence(diag)}`, file: s.file });
    }
    for (const a of d.agents) {
      if (a.model) {
        const mm = models.get(a.model) || { agents: 0, cost: 0, outTok: 0, roles: new Set(), tier: modelTier(a.model) };
        mm.agents++; mm.cost += a.cost || 0; mm.outTok += a.outTokens || 0;
        if (a.id !== 'main') mm.roles.add(norm(a.name));
        models.set(a.model, mm);
      }
      if (a.id === 'main') continue;
      const key = norm(a.name || 'subagent');
      const r = roles.get(key) || { n: 0, clean: 0, cost: 0, byMachine: {}, models: {}, example: null };
      r.n++; if (!a.errors) r.clean++; r.cost += a.cost || 0;
      if (a.model) r.models[a.model] = (r.models[a.model] || 0) + 1;
      const m = s.machine || 'local';
      r.byMachine[m] = r.byMachine[m] || { n: 0, clean: 0 };
      r.byMachine[m].n++; if (!a.errors) r.byMachine[m].clean++;
      if (!r.example || s.mtime > r.example.mtime) r.example = s;
      roles.set(key, r);
    }
  }
  // insights (rule-based, plain-language) — role/economics insights, added to the
  // per-session failure diagnoses already pushed into `insights` above
  for (const [name, r] of roles) {
    if (r.n >= 3 && r.clean / r.n < 0.7) insights.push({ key: 'failrole:' + name, sev: 'bad', icon: '🛠', text: `"${name}" fails ${Math.round((1 - r.clean / r.n) * 100)}% of the time (${r.n} runs). Its prompt or task definition needs work — open a failed run and read what goes wrong.`, file: r.example?.file, detail: { role: name, runs: r.n, clean: r.clean, byMachine: r.byMachine, models: r.models } });
    const machines = Object.entries(r.byMachine).filter(([, v]) => v.n >= 2);
    if (machines.length >= 2) {
      const rates = machines.map(([m, v]) => ({ m, rate: v.clean / v.n }));
      rates.sort((a, b) => b.rate - a.rate);
      if (rates[0].rate - rates[rates.length - 1].rate > 0.3) insights.push({ key: 'envrole:' + name, sev: 'warn', icon: '🖥', text: `"${name}" succeeds ${Math.round(rates[0].rate * 100)}% on ${rates[0].m} but only ${Math.round(rates[rates.length - 1].rate * 100)}% on ${rates[rates.length - 1].m} — the environment matters for this role.`, file: r.example?.file, detail: { role: name, byMachine: r.byMachine } });
    }
  }
  // Delegation economics — honest version. Delegating is NOT automatically cheaper:
  // if the subagents run the same premium tier as the orchestrator, delegation saved
  // nothing. So we flag two distinct problems: (a) work that never got delegated at
  // all, and (b) work that was delegated but to equally-expensive models.
  for (const x of sessions.filter(x => x.mainCost + x.subCost > 20)) {
    const total = x.mainCost + x.subCost;
    const share = x.mainCost / total;
    if (x.subCost > 10 && x.subTopCost / x.subCost > 0.7) {
      const wouldCost = x.subTopCost * (TIER_RATE.mid / TIER_RATE.premium);
      insights.push({ key: 'flatdeleg:' + x.s.session, sev: 'warn', icon: '🪙', text: `"${(x.s.title || '').slice(0, 40)}" delegated ~${fmtUsd(x.subCost)} to ${x.agents - 1} agents — but ${Math.round(x.subTopCost / x.subCost * 100)}% of that ran on the same premium tier as the orchestrator, so the delegation saved nothing. The same subagent work one tier down (Sonnet) ≈ ${fmtUsd(wouldCost)} instead of ${fmtUsd(x.subTopCost)}.`, file: x.s.file, detail: { subCost: x.subCost, subTopCost: x.subTopCost, agents: x.agents } });
    } else if (share > 0.6 && x.agents > 5) {
      insights.push({ key: 'hoard:' + x.s.session, sev: 'warn', icon: '💸', text: `"${(x.s.title || '').slice(0, 40)}" spent ${Math.round(share * 100)}% of ~${fmtUsd(total)} in the orchestrator itself despite having ${x.agents} agents. Delegating more only saves money if the workers run a CHEAPER tier — pair delegation with the tiering directive.`, file: x.s.file, detail: { mainCost: x.mainCost, subCost: x.subCost, agents: x.agents } });
    }
  }
  // fleet-wide: is delegation actually saving money?
  const fleetSub = sessions.reduce((n, x) => n + x.subCost, 0);
  const fleetSubTop = sessions.reduce((n, x) => n + x.subTopCost, 0);
  if (fleetSub > 20 && fleetSubTop / fleetSub < 0.4) insights.push({ key: 'goodtier', sev: 'good', icon: '🎯', text: `Delegation economics are healthy: ${Math.round((1 - fleetSubTop / fleetSub) * 100)}% of your ~${fmtUsd(fleetSub)} subagent spend runs on cheaper tiers than the orchestrator — that's where real savings come from.` });
  const cleanRoles = [...roles.entries()].filter(([, r]) => r.n >= 3 && r.clean / r.n >= 0.85).sort((a, b) => b[1].n - a[1].n);
  if (cleanRoles.length >= 2) insights.push({ key: 'reliable', sev: 'good', icon: '🏆', text: `Your most reliable roles: ${cleanRoles.slice(0, 3).map(([n]) => `"${n}"`).join(', ')} — proven building blocks for new playbooks below.` });

  // model-tier / token-saving analysis: are premium models doing cheap work?
  const tierCost = { flagship: 0, premium: 0, mid: 0, cheap: 0, unknown: 0 };
  for (const [, mm] of models) tierCost[mm.tier] += mm.cost;
  const totalModelCost = Object.values(tierCost).reduce((a, b) => a + b, 0);
  const topCost = tierCost.flagship + tierCost.premium; // apex + premium both burn expensive tokens
  if (totalModelCost > 20 && topCost / totalModelCost > 0.6) {
    // roles that ran ONLY on top-tier models (premium/flagship) but succeed
    // reliably = downgrade candidates. Savings = counterfactual: same work at
    // mid-tier rates, not a flat guess.
    const downgradable = [...roles.entries()].filter(([, r]) => {
      const ms = Object.keys(r.models); return r.n >= 3 && r.clean / r.n >= 0.85 && ms.length && ms.every(m => modelTier(m) === 'premium' || modelTier(m) === 'flagship');
    }).sort((a, b) => b[1].cost - a[1].cost).slice(0, 3);
    if (downgradable.length) {
      const save = downgradable.reduce((n, [, r]) => {
        const rate = Math.max(...Object.keys(r.models).map(m => TIER_RATE[modelTier(m)] || TIER_RATE.premium));
        return n + r.cost * (1 - TIER_RATE.mid / rate);
      }, 0);
      insights.push({ key: 'tier:premium', sev: 'warn', icon: '⚖️', text: `${Math.round(topCost / totalModelCost * 100)}% of your model spend is top-tier (Opus/Fable/GPT-5). Reliable roles like ${downgradable.slice(0, 2).map(([n]) => `"${n}"`).join(', ')} run only on those models but rarely fail — the same tokens at mid tier (Sonnet) would cost ~${fmtUsd(save)} less. Plant the tiering directive to make every future session do this.`, file: downgradable[0][1].example?.file, detail: { downgradable: downgradable.map(([n, r]) => ({ role: n, cost: r.cost })) } });
    }
  }
  const tierBreak = Object.entries(tierCost).filter(([, c]) => c > 0.01).map(([t, c]) => `${t} ~${fmtUsd(c)}`).join(' · ');
  return { roles, sessions, insights, cleanRoles, models, tierCost, tierBreak };
}

function buildPlaybook(kind, mined) {
  const { cleanRoles } = mined;
  const roster = cleanRoles.slice(0, kind === 'review' ? 4 : 6).map(([n, r]) => ({ name: n, success: Math.round(r.clean / r.n * 100), avgCost: r.cost / r.n }));
  const budget = Math.max(5, Math.ceil(roster.reduce((n, r) => n + r.avgCost, 0) * 1.5));
  const L = [];
  if (kind === 'tiered') {
    L.push(`# Playbook: Tiered token-saving team (generated from your fleet)`, '');
    L.push(`Paste to a Claude Code / Codex session. Fill in the GOAL.`, '');
    L.push(`GOAL: <describe the task>`, '');
    L.push(`Orchestrate this as a TIERED team to minimize token cost — expensive models only where judgment matters:`, '');
    L.push(`ORCHESTRATOR (you): a flagship or premium model — Fable 5 for the hardest long-horizon planning, or Opus 5 for most work. You plan, delegate, and integrate. Do NOT do the bulk reading/searching yourself — that burns flagship tokens on cheap work.`, '');
    L.push(`WORKERS: spawn subagents on a MID or CHEAP model (Sonnet, or Haiku for pure fetch/grep/summarize). Give each a narrow, well-scoped task:`);
    for (const r of roster.slice(0, 4)) L.push(`- ${r.name} → Sonnet (proven ${r.success}% success)`);
    L.push(`- fetch / search / file-reading helpers → Haiku (no judgment needed)`, '');
    L.push(`REVIEWERS/VERIFIERS: premium model, but only for the final gate — one adversarial reviewer that checks the integrated result.`, '');
    L.push(`Rule of thumb from your history: reserve premium tokens for planning + final judgment; push all reading, searching, and first-draft work to cheaper tiers. Report a per-tier token/cost breakdown when done.`);
    L.push(`Target budget: ~$${budget}.`);
    return L.join('\n');
  }
  L.push(`# Playbook: ${kind === 'review' ? 'Adversarial review pass' : 'Parallel build team'} (generated from your fleet's track record)`, '');
  L.push(`Paste this to a Claude Code session. Fill in the GOAL.`, '');
  L.push(`GOAL: <describe what you want built/reviewed>`, '');
  L.push(`Please orchestrate this with a team of subagents, using roles this fleet has proven:`);
  for (const r of roster) L.push(`- ${r.name} (historical success ${r.success}%${r.avgCost > 0.01 ? `, avg ~${fmtUsd(r.avgCost)}/run` : ''})`);
  L.push('');
  L.push(kind === 'review'
    ? `Pattern: have each reviewer attack the work independently through a different lens, then a final agent merges confirmed findings. Don't let reviewers see each other's output before verdicts.`
    : `Pattern: split the goal into independent workstreams, one agent per stream, working in parallel; one integrator agent merges results and runs a final consistency check.`);
  L.push(`Keep total spend under ~$${budget} (1.5× your historical average for this team size). Report per-agent results before merging.`);
  return L.join('\n');
}

let playbookLib = { items: [] };
async function loadPlaybookLib() { try { playbookLib = await (await fetch('/api/playbooks')).json(); } catch { playbookLib = { items: [] }; } }
async function savePlaybookToLib(name, kind, body, source) {
  const r = await fetch('/api/playbooks', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'save', name, kind, body, source }) });
  if (r.ok) { playbookLib = await r.json(); return true; } return false;
}

function insightDetailHTML(i) {
  const d = i.detail; if (!d) return '';
  if (d.byMachine) {
    const rows = Object.entries(d.byMachine).map(([m, v]) => `<tr><td>${esc(m)}</td><td class="num">${v.n} runs</td><td class="num ${v.clean / v.n < 0.6 ? 'bad' : ''}">${Math.round(v.clean / v.n * 100)}% clean</td></tr>`).join('');
    const models = d.models ? '<div class="pbd-line">Models used: ' + Object.entries(d.models).map(([m, n]) => `${esc(m.replace(/^claude-|-\d+$/g, ''))} ×${n}`).join(', ') + '</div>' : '';
    return `<div class="pbd-line">Role <b>${esc(d.role)}</b>${d.runs ? ` — ${d.runs} runs, ${d.clean} clean (${Math.round(d.clean / d.runs * 100)}%)` : ''}</div>
      <table class="pbd-table"><tbody>${rows}</tbody></table>${models}
      <div class="pbd-hint">To fix: open a failed session (↗) and read the Story tab for the actual error, then adjust this role's prompt. Save the fix as a playbook so it's reusable.</div>`;
  }
  if (d.mainCost != null) return `<div class="pbd-line">Orchestrator kept ~${fmtUsd(d.mainCost)}; subagents used ~${fmtUsd(d.subCost)} across ${d.agents} agents.</div><div class="pbd-hint">Delegate more of the reading/searching to subagents (Sonnet/Haiku) so premium orchestrator tokens go to planning + integration. Try the Tiered playbook.</div>`;
  if (d.downgradable) return `<div class="pbd-line">Downgrade candidates (premium-only, reliable):</div><table class="pbd-table"><tbody>${d.downgradable.map(x => `<tr><td>${esc(x.role)}</td><td class="num fcost">~${fmtUsd(x.cost)}</td></tr>`).join('')}</tbody></table><div class="pbd-hint">Move these to Sonnet in a tiered playbook and re-measure.</div>`;
  return '';
}
const PB_GEN = [
  { k: 'build', label: '🔨 Parallel build team' },
  { k: 'review', label: '⚔️ Adversarial review pass' },
  { k: 'tiered', label: '⚖️ Tiered token-saving team' },
];
function renderPlaybooks() {
  if (!directivesLoaded) { directivesLoaded = true; loadDirectiveReg().then(() => { if (state.view === 'playbooks') renderPlaybooks(); }); }
  const mined = mineFleet();
  const { insights, tierBreak } = mined;
  const sevOrder = { bad: 0, warn: 1, good: 2 };
  insights.sort((a, b) => sevOrder[a.sev] - sevOrder[b.sev]);
  $('playbooks').innerHTML =
    `<div class="fleet-head"><h2>Playbook Studio <span class="qi" title="Studies your fleet's real track record — role success rates, where money leaks, model-tier spend, recurring patterns — and turns it into insights + reusable playbooks saved to your Library. Generation only; nothing runs from here.">ⓘ</span></h2>
      <span class="dim">${(flowsCache || []).length} sessions studied${tierBreak ? ' · model spend: ' + tierBreak : ''}</span>
      <button id="pbRefresh" class="mini-btn">↻ re-study</button></div>

    <div class="pb-method dim">How this is computed: I aggregate every subagent across your recent sessions by role (name with the run-number stripped), tracking success rate, cost, model tier, and which machine it ran on — then flag failing roles, environment-dependent roles, and dishonest delegation economics (top-tier overuse, and workers running the same premium tier as the orchestrator, where delegating saves nothing). All rule-based and local; recomputed on demand, no schedule, and no session content leaves your machine.</div>

    <div class="flows-grid">
      <div class="flows-panel">
        <h3>What your fleet is telling you — triage <button id="handleAll" class="mini-btn" style="float:right" title="I'll clear the informational ones and give you a plain-language action list for the rest">✨ Handle it for me</button></h3>
        <div class="dim" style="margin-bottom:8px">Issues found across ${(flowsCache || []).length} sessions. Expand for detail, open the session to read the conversation, or resolve / dismiss to close it out.</div>
        <div class="seg triage-seg" id="triageSeg">
          ${[['open', 'Open'], ['resolved', 'Resolved'], ['dismissed', 'Dismissed'], ['all', 'All']].map(([v, l]) => {
            const n = insights.filter(i => i.key && (v === 'all' || (v === 'open' ? !triageState[i.key] : triageState[i.key]?.status === v))).length;
            return `<button data-t="${v}" class="${triageFilter === v ? 'on' : ''}">${l}${n ? ' ' + n : ''}</button>`;
          }).join('')}
        </div>
        ${(() => {
          const shown = insights.filter(i => {
            const st = i.key ? triageState[i.key]?.status : null;
            if (triageFilter === 'all') return true;
            if (triageFilter === 'open') return !st;
            return st === triageFilter;
          });
          return shown.length ? shown.map(i => {
            const st = i.key ? triageState[i.key]?.status : null;
            const open = i.key && triageExpanded.has(i.key);
            return `<div class="pb-insight pb-${i.sev} ${st ? 'triaged' : ''}" data-key="${esc(i.key || '')}">
              <div class="pbi-main">
                <span class="pb-icon">${i.icon}</span>
                <span class="pb-text">${esc(i.text)}</span>
              </div>
              <div class="pbi-actions">
                ${i.detail ? `<button class="pbi-btn pbi-expand">${open ? '▴ less' : '▾ detail'}</button>` : ''}
                ${i.file ? `<button class="pbi-btn pbi-open">↗ session</button>` : ''}
                ${st ? `<button class="pbi-btn pbi-reopen">↺ reopen</button><span class="pbi-status st-${st}">${st}</span>`
                     : `${i.sev !== 'good' ? `<button class="pbi-btn pbi-directive" title="plant this learning as a standing order in repo guidance files — future sessions obey it automatically">🛰 make it a rule</button>` : ''}<button class="pbi-btn pbi-resolve">✓ resolve</button><button class="pbi-btn pbi-dismiss">✕ dismiss</button>`}
              </div>
              ${open && i.detail ? `<div class="pbi-detail">${insightDetailHTML(i)}</div>` : ''}
            </div>`;
          }).join('') : `<div class="dim" style="padding:14px">Nothing ${triageFilter === 'open' ? 'open' : 'here'} — ${triageFilter === 'open' ? 'you\'ve triaged everything. 🎉' : 'switch filters above.'}</div>`;
        })()}
      </div>
    </div>

    <div class="flows-panel" style="margin-top:16px">
      <h3>🛰 Standing orders <span class="dim">(${(directiveReg.items || []).length} planted)</span>
        <span style="float:right;display:flex;gap:8px;align-items:center">
          <span class="seg" id="ordersSeg"><button data-v="list" class="${ordersView === 'list' ? 'on' : ''}">list</button><button data-v="coverage" class="${ordersView === 'coverage' ? 'on' : ''}">coverage</button></span>
          <button id="dirNew" class="mini-btn">＋ new order</button>
        </span></h3>
      <div class="dim" style="margin-bottom:10px">Rules this dashboard has planted into repo guidance files (CLAUDE.md / AGENTS.md). Every future agent session in those repos reads them automatically — this is how a learning becomes permanent instead of a prompt you have to remember. ${ordersView === 'list' ? '<b>Check</b> verifies each is still in place; <b>retire</b> removes the block cleanly. To reach your <b>other computers</b>, use <b>share via git</b> on a card: the rule travels with the code, so they pick it up on their next <code>git pull</code> — and one <code>git revert</code> takes it back off every machine at once.' : 'This view shows where each rule does and doesn’t reach yet — click an empty cell to plant it somewhere new.'}</div>
      ${(() => {
        const dueCount = (directiveReg.items || []).filter(directiveDue).length;
        return dueCount ? `<div class="dir-due-summary">⏰ ${dueCount} order${dueCount === 1 ? ' is' : 's are'} up for review.</div>` : '';
      })()}
      ${ordersView === 'coverage' ? coverageGridHTML() : ((directiveReg.items || []).length ? [...directiveReg.items].sort((a, b) => directiveDue(b) - directiveDue(a)).map(d => {
        const due = directiveDue(d);
        const myPaths = new Set((d.targets || []).map(t => t.path));
        const overlaps = overlappingDirectives(dirTopic(d), myPaths, d.id);
        return `
        <div class="dir-item${due ? ' dir-due' : ''}" data-id="${esc(d.id)}">
          <div class="dir-head"><b>🛰 ${esc(d.title)}</b><span class="dim" style="margin-left:8px">planted ${fmtAgo(d.createdAt)}</span>
            ${due ? '<span class="dir-chip st-drifted" style="margin-left:8px">⏰ up for review</span>' : ''}
            <span style="margin-left:auto;display:flex;gap:6px">
              ${due ? `<button class="mini-btn dir-reviewed" title="mark this order reviewed and reset the review clock">👍 Still good — keep it</button>` : ''}
              <button class="mini-btn dir-check" title="verify the block is still in every file">🔍 check</button>
              <button class="mini-btn dir-copy" title="copy the order text">📋</button>
              <button class="mini-btn dir-retire" title="remove the block from every file and forget it">🗑 retire</button>
            </span></div>
          <div class="dir-chips">${(d.targets || []).map(t => `<span class="dir-chip">${esc(t.label)}</span>`).join('')}</div>
          <div class="dir-impact dim">${esc(directiveImpact(d, mined))} Reviewed ${fmtAgo(d.lastReviewedAt || d.createdAt)}, every ${d.reviewEveryDays || 30} days.</div>
          ${overlaps.length ? `<div class="dir-overlap">⚠ overlaps: ${overlaps.map(o => esc(o.title)).join(', ')}</div>` : ''}
          <div class="dir-status dim"></div>
          <div class="dir-git">
            <div class="dg-head dim">🔗 Share via git — commit the rule where it lives, then send it when you're ready.</div>
            ${gitRowsHTML(d)}
          </div>
        </div>`;
      }).join('') : '<div class="dim">None yet — hit <b>🛰 make it a rule</b> on any insight above, or ＋ new order.</div>')}
    </div>

    <div class="flows-panel" style="margin-top:16px">
      <h3>📚 Your Playbook Library <span class="dim">(${playbookLib.items.length} saved)</span>
        <span style="float:right;display:flex;gap:6px">
          ${PB_GEN.map(g => `<button class="mini-btn pb-gen" data-kind="${g.k}" title="generate from your fleet, then edit">${esc(g.label.replace(/^\S+\s/, ''))}</button>`).join('')}
          <button id="pbNew" class="mini-btn">＋ blank</button>
        </span></h3>
      <div class="dim" style="margin-bottom:10px">Your durable collection — starter plays, your own directives, and fleet-generated ones. Click any to open the editor. Kept in <code>~/.claude/mission-control/playbooks.json</code>.</div>
      ${playbookLib.items.length ? `<div class="pb-lib">` + playbookLib.items.map(p => `
        <div class="pb-lib-item" data-id="${esc(p.id)}">
          <div class="pli-head"><b>${esc(p.name)}</b><span class="pli-kind">${esc(p.kind)}</span><span class="dim">${fmtAgo(p.updatedAt)}</span></div>
          <div class="pli-actions"><button class="mini-btn pli-copy">📋</button><button class="mini-btn pli-edit">✎</button><button class="mini-btn pli-del">🗑</button></div>
        </div>`).join('') + `</div>` : '<div class="dim">Empty. Generate one from your fleet (buttons above), or start blank.</div>'}
    </div>`;

  $('pbRefresh').onclick = () => { flowsCache = null; loadPlaybooks(); };
  $('triageSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => { triageFilter = b.dataset.t; renderPlaybooks(); });
  $('playbooks').querySelectorAll('.pb-insight[data-key]').forEach(el => {
    const key = el.dataset.key;
    const ins = insights.find(i => i.key === key);
    el.querySelector('.pbi-open')?.addEventListener('click', e => { e.stopPropagation(); openSession(ins.file); state.view = 'story'; setTabs(); render(); });
    el.querySelector('.pbi-expand')?.addEventListener('click', e => { e.stopPropagation(); if (triageExpanded.has(key)) triageExpanded.delete(key); else triageExpanded.add(key); renderPlaybooks(); });
    el.querySelector('.pbi-directive')?.addEventListener('click', e => { e.stopPropagation(); openDirectiveComposer({ ...directiveTemplate(ins), insightKey: key }); });
    el.querySelector('.pbi-resolve')?.addEventListener('click', e => { e.stopPropagation(); setTriage(key, 'resolved'); });
    el.querySelector('.pbi-dismiss')?.addEventListener('click', e => { e.stopPropagation(); setTriage(key, 'dismissed'); });
    el.querySelector('.pbi-reopen')?.addEventListener('click', e => { e.stopPropagation(); setTriage(key, 'open'); });
  });
  $('playbooks').querySelectorAll('.pb-gen').forEach(b => b.onclick = () => {
    // generate from the fleet, then hand it to the editor to save/tweak
    const g = PB_GEN.find(x => x.k === b.dataset.kind);
    openPlaybookEditor({ id: null, name: g.label.replace(/^\S+\s/, '') + ' (from my fleet)', kind: b.dataset.kind, body: buildPlaybook(b.dataset.kind, mined), source: 'studio' });
  });
  $('handleAll').onclick = () => handleTriageForMe(insights, mined);
  $('playbooks').querySelectorAll('.pb-save').forEach(b => b.onclick = async () => {
    const g = PB_GEN.find(x => x.k === b.dataset.kind);
    const name = prompt('Save to library as:', g.label.replace(/^\S+\s/, '')); if (!name) return;
    if (await savePlaybookToLib(name, b.dataset.kind, buildPlaybook(b.dataset.kind, mined), 'studio')) renderPlaybooks();
  });
  $('pbNew').onclick = () => openPlaybookEditor(null);
  $('dirNew') && ($('dirNew').onclick = () => openDirectiveComposer(directiveTemplate({ key: 'tier:new' })));
  $('ordersSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => { ordersView = b.dataset.v; renderPlaybooks(); });
  if (ordersView === 'coverage') {
    $('covCheckAll')?.addEventListener('click', async () => {
      const btn = $('covCheckAll'); btn.textContent = 'Checking…'; btn.disabled = true;
      await Promise.all((directiveReg.items || []).map(async d => {
        try {
          const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'check', id: d.id }) })).json();
          coverageChecks[d.id] = j.statuses || [];
        } catch { /* leave stale */ }
      }));
      renderPlaybooks();
    });
    $('playbooks').querySelectorAll('.cov-cell.off').forEach(td => td.onclick = async () => {
      const d = (directiveReg.items || []).find(x => x.id === td.dataset.dir);
      const t = (directiveReg.targets || []).find(x => x.id === td.dataset.target);
      if (!d || !t) return;
      if (!confirm(`Plant "${d.title}" into ${t.label} (${t.name}) too?`)) return;
      td.textContent = '…';
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'plant-existing', id: d.id, targets: [t.id] }) })).json();
        if (!j.ok) throw new Error(j.error || 'failed');
        directiveReg.items = j.items;
        renderPlaybooks();
      } catch (e) { alert('Failed: ' + e.message); renderPlaybooks(); }
    });
  }
  $('playbooks').querySelectorAll('.dir-item').forEach(el => {
    const d = (directiveReg.items || []).find(x => x.id === el.dataset.id); if (!d) return;
    el.querySelector('.dir-copy').onclick = e => { navigator.clipboard.writeText(d.body); e.target.textContent = '✓'; setTimeout(() => { e.target.textContent = '📋'; }, 1200); };
    el.querySelector('.dir-reviewed')?.addEventListener('click', async () => {
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'reviewed', id: d.id }) })).json();
        if (j.items) { directiveReg.items = j.items; renderPlaybooks(); }
      } catch { /* leave as-is */ }
    });
    el.querySelector('.dir-check').onclick = async () => {
      const st = el.querySelector('.dir-status'); st.textContent = 'Checking…';
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'check', id: d.id }) })).json();
        coverageChecks[d.id] = j.statuses || [];
        st.innerHTML = (j.statuses || []).map(s => `<span class="dir-chip st-${s.status}">${esc(s.label)} ${s.status === 'ok' ? '✓ in place' : s.status === 'drifted' ? '⚠ removed or edited away' : '⚠ file missing'}</span>`).join(' ') || 'No targets recorded.';
      } catch { st.textContent = 'Check failed.'; }
    };
    // share via git — each button runs real git commands, so none of this fires on its own
    el.querySelectorAll('.dir-git-check').forEach(b => b.onclick = async () => {
      b.textContent = 'Looking…'; b.disabled = true;
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'git-status', id: d.id }) })).json();
        if (j.error) throw new Error(j.error);
        gitStates[d.id] = j.states || [];
        renderPlaybooks();
      } catch (e) { b.textContent = 'Could not read git: ' + e.message; b.disabled = false; }
    });
    el.querySelectorAll('.dg-commit').forEach(b => b.onclick = async () => {
      const p = b.dataset.path;
      b.textContent = 'Committing…'; b.disabled = true;
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'git-commit', id: d.id, path: p }) })).json();
        gitNotes[d.id + '|' + p] = j.error ? 'Failed: ' + j.error : j.note;
      } catch (e) { gitNotes[d.id + '|' + p] = 'Failed: ' + e.message; }
      // re-read git afterwards so the row tells the truth about what is left to send
      try {
        const s = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'git-status', id: d.id }) })).json();
        if (s.states) gitStates[d.id] = s.states;
      } catch { /* keep the older state */ }
      renderPlaybooks();
    });
    el.querySelectorAll('.dg-push').forEach(b => b.onclick = async () => {
      const p = b.dataset.path;
      const s = (gitStates[d.id] || []).find(x => x.path === p) || {};
      if (!confirm(`Send to the remote?\n\nThis pushes everything already committed on ${s.branch ? `branch "${s.branch}"` : 'this branch'} in this repo — not only this rule — to wherever that repo sends to. Your other computers pick it up with git pull.\n\nThis is the one step that leaves this computer.`)) return;
      b.textContent = 'Sending…'; b.disabled = true;
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'git-push', id: d.id, path: p }) })).json();
        gitNotes[d.id + '|' + p] = j.error ? 'Failed: ' + j.error : j.note;
      } catch (e) { gitNotes[d.id + '|' + p] = 'Failed: ' + e.message; }
      try {
        const st = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'git-status', id: d.id }) })).json();
        if (st.states) gitStates[d.id] = st.states;
      } catch { /* keep the older state */ }
      renderPlaybooks();
    });
    el.querySelector('.dir-retire').onclick = async () => {
      if (!confirm(`Retire "${d.title}"? The block is removed from ${(d.targets || []).length} file(s); each file is snapshotted first.`)) return;
      try {
        const j = await (await fetch('/api/directives', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'retire', id: d.id }) })).json();
        // Retiring edits files here. If the rule was ever committed and sent, other
        // machines keep it until the REMOVAL is committed too — say so, don't imply gone.
        if ((j.needsCommit || []).length) alert(`Removed here.\n\nBut ${j.needsCommit.join(', ')} ${j.needsCommit.length === 1 ? 'is a git repo' : 'are git repos'} — if you ever sent this rule to a remote, other computers keep following it until you commit and send the removal too.`);
        if (j.items) { directiveReg.items = j.items; renderPlaybooks(); }
      } catch { /* leave as-is */ }
    };
  });
  $('playbooks').querySelectorAll('.pb-lib-item').forEach(el => {
    const p = playbookLib.items.find(x => x.id === el.dataset.id);
    el.onclick = () => openPlaybookEditor(p); // whole card opens the editor
    el.style.cursor = 'pointer';
    el.querySelector('.pli-copy').onclick = e => { e.stopPropagation(); navigator.clipboard.writeText(p.body); e.target.textContent = '✓'; setTimeout(() => { e.target.textContent = '📋'; }, 1200); };
    el.querySelector('.pli-edit').onclick = e => { e.stopPropagation(); openPlaybookEditor(p); };
    el.querySelector('.pli-del').onclick = async e => { e.stopPropagation(); if (!confirm(`Delete "${p.name}"?`)) return; await fetch('/api/playbooks', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'delete', id: p.id }) }).then(r => r.json()).then(j => { playbookLib = j; renderPlaybooks(); }); };
  });
}
// Common directive phrases you can drop into a playbook with one click.
const PB_SNIPPETS = [
  { g: 'Structure', items: [
    ['GOAL block', 'GOAL: <describe the task in one or two sentences>\n\n'],
    ['Success criteria', 'DONE WHEN:\n- [ ] <criterion>\n- [ ] <criterion>\n\n'],
    ['Constraints', 'CONSTRAINTS:\n- Do not <forbidden action>.\n- Stay within <scope>.\n\n'],
    ['Report format', 'REPORT: when finished, give me: (1) what changed, (2) per-agent results, (3) anything unverified.\n\n'],
  ] },
  { g: 'Orchestration', items: [
    ['Parallel workers', 'Split the goal into independent workstreams; spawn one subagent per stream in parallel; a final integrator merges results and runs a consistency check.\n'],
    ['Adversarial review', 'Have each reviewer attack the work independently through a different lens, then merge only confirmed findings. Reviewers must not see each other before verdicts.\n'],
    ['Tiered (save tokens)', 'Orchestrator = premium model (plan + integrate only). Workers = Sonnet. Pure fetch/grep/summarize = Haiku. Reserve premium tokens for judgment; report a per-tier cost breakdown.\n'],
    ['Loop until dry', 'Keep spawning finders until 2 consecutive rounds surface nothing new; dedup against everything already found.\n'],
  ] },
  { g: 'Guardrails', items: [
    ['Budget cap', 'Keep total spend under ~$<N>. Stop and report if you approach it.\n'],
    ['Read-only', 'READ-ONLY: do not edit, write, commit, or run migrations.\n'],
    ['Ground in data', 'Ground every finding in live data or a cited file:line. Default to "this is wrong" and try to break it before calling it correct.\n'],
    ['Ask before destructive', 'Confirm with me before any irreversible action (delete, deploy, send, overwrite).\n'],
  ] },
  { g: 'Checklists', items: [
    ['Checkbox item', '- [ ] '],
    ['Numbered step', '1. '],
    ['Section heading', '\n## Section\n\n'],
  ] },
];
let pbEditorState = null;
function openPlaybookEditor(pb) {
  pbEditorState = { id: pb?.id || null, source: pb?.source || 'manual' };
  const ov = document.createElement('div');
  ov.className = 'pb-editor-ov'; ov.id = 'pbEditorOv';
  ov.innerHTML = `
    <div class="pb-editor">
      <div class="pbe-head">
        <input id="pbeName" class="pbe-name" placeholder="Directive / playbook name" value="${esc(pb?.name || '')}">
        <input id="pbeKind" class="pbe-kind" placeholder="tag (e.g. review, build, tiered)" value="${esc(pb?.kind || 'custom')}">
        <button id="pbeClose" class="mini-btn">✕</button>
      </div>
      <div class="pbe-body">
        <div class="pbe-palette">
          <div class="pbe-pal-title">Insert common phrases</div>
          ${PB_SNIPPETS.map(sec => `<div class="pbe-pal-g">${esc(sec.g)}</div>` +
            sec.items.map((it, i) => `<button class="pbe-snip" data-g="${esc(sec.g)}" data-i="${i}">${esc(it[0])}</button>`).join('')).join('')}
        </div>
        <div class="pbe-edit">
          <div class="pbe-toolbar">
            <button class="pbe-tool" data-ins="- [ ] ">☐ checkbox</button>
            <button class="pbe-tool" data-ins="\\n## ">H heading</button>
            <button class="pbe-tool" data-wrap="**">B</button>
            <button class="pbe-tool" data-wrap="\`">code</button>
            <span class="pbe-count" id="pbeCount"></span>
          </div>
          <textarea id="pbeBody" spellcheck="false" placeholder="Write your directive package here, or click phrases on the left to build it.">${esc(pb?.body || '# Playbook\n\nGOAL: \n')}</textarea>
        </div>
        <div class="pbe-preview" id="pbePreview"></div>
      </div>
      <div class="pbe-foot">
        ${pb ? '<button id="pbeDelete" class="mini-btn pbe-del">🗑 Delete</button>' : '<span></span>'}
        <div>
          <button id="pbeSaveAs" class="mini-btn">Save as new</button>
          <button id="pbeSave" class="mini-btn pbe-save">💾 Save</button>
        </div>
      </div>
    </div>`;
  document.body.appendChild(ov);
  const ta = ov.querySelector('#pbeBody');
  const insertAtCursor = (text) => {
    const s = ta.selectionStart, e = ta.selectionEnd;
    ta.value = ta.value.slice(0, s) + text + ta.value.slice(e);
    ta.selectionStart = ta.selectionEnd = s + text.length;
    ta.focus(); syncPreview();
  };
  const wrapSelection = (mark) => {
    const s = ta.selectionStart, e = ta.selectionEnd;
    const sel = ta.value.slice(s, e) || 'text';
    ta.value = ta.value.slice(0, s) + mark + sel + mark + ta.value.slice(e);
    ta.selectionStart = s + mark.length; ta.selectionEnd = s + mark.length + sel.length;
    ta.focus(); syncPreview();
  };
  const syncPreview = () => {
    ov.querySelector('#pbePreview').innerHTML = renderMD(ta.value);
    ov.querySelector('#pbeCount').textContent = ta.value.length + ' chars';
  };
  ta.oninput = syncPreview; syncPreview();
  ov.querySelectorAll('.pbe-snip').forEach(b => b.onclick = () => {
    const sec = PB_SNIPPETS.find(x => x.g === b.dataset.g); insertAtCursor(sec.items[+b.dataset.i][1]);
  });
  ov.querySelectorAll('.pbe-tool').forEach(b => b.onclick = () => { if (b.dataset.ins) insertAtCursor(b.dataset.ins.replace(/\\n/g, '\n')); else wrapSelection(b.dataset.wrap); });
  ov.querySelector('#pbeClose').onclick = () => ov.remove();
  ov.onclick = e => { if (e.target === ov) ov.remove(); };
  const doSave = async (asNew) => {
    const name = ov.querySelector('#pbeName').value.trim() || 'Untitled directive';
    const kind = ov.querySelector('#pbeKind').value.trim() || 'custom';
    const body = ta.value;
    const j = await fetch('/api/playbooks', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'save', id: asNew ? undefined : pbEditorState.id, name, kind, body, source: pbEditorState.source }) }).then(r => r.json()).catch(() => null);
    if (j && j.items) { playbookLib = j; ov.remove(); if (state.view === 'playbooks') renderPlaybooks(); }
    else alert('Save failed');
  };
  ov.querySelector('#pbeSave').onclick = () => doSave(false);
  ov.querySelector('#pbeSaveAs').onclick = () => doSave(true);
  if (pb) ov.querySelector('#pbeDelete').onclick = async () => {
    if (!confirm(`Delete "${pb.name}"?`)) return;
    const j = await fetch('/api/playbooks', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ op: 'delete', id: pb.id }) }).then(r => r.json()).catch(() => null);
    if (j && j.items) { playbookLib = j; ov.remove(); renderPlaybooks(); }
  };
  ta.focus();
}

// ---------- AUDIT view (plain-language activity log of every change) ----------
async function loadAudit() {
  let entries = [];
  try { entries = (await (await fetch('/api/audit')).json()).entries || []; } catch { /* empty */ }
  renderAudit(entries);
}
function humanizeAudit(e) {
  const k = e.kind;
  if (k === 'brain-write') return { icon: e.hooks ? '⚠️' : '🧠', text: `Edited <b>${esc(e.name || 'a config file')}</b>${e.hooks ? ' (hooks — runs shell commands)' : ''}`, sub: e.path, file: e.path, kind: 'brain' };
  if (k === 'triage') return { icon: e.status === 'resolved' ? '✓' : e.status === 'dismissed' ? '✕' : '↺', text: `${e.status === 'open' ? 'Reopened' : e.status === 'resolved' ? 'Resolved' : 'Dismissed'} an issue`, sub: e.key, kind: 'triage' };
  if (k === 'playbook-save') return { icon: '💾', text: `Saved playbook <b>${esc(e.name || '')}</b>`, kind: 'playbook' };
  if (k === 'playbook-delete') return { icon: '🗑', text: 'Deleted a playbook', kind: 'playbook' };
  if (k === 'launch') return { icon: '🚀', text: `Launched a ${esc(e.agent || 'agent')} session`, sub: e.cwd, kind: 'control' };
  if (k === 'launch-end') return { icon: '🏁', text: `A launched ${esc(e.agent || 'agent')} finished (exit ${e.code})`, kind: 'control' };
  if (k === 'kill') return { icon: '🛑', text: `Stopped a running ${esc(e.agent || 'agent')} session`, kind: 'control' };
  if (k === 'approval') return { icon: '☑', text: `${e.decision === 'approved' ? 'Approved' : 'Denied'} an agent request${e.tool ? ' (' + esc(e.tool) + ')' : ''}`, sub: e.machine, kind: 'control' };
  if (k === 'enqueue') return { icon: '📤', text: `Queued a ${esc(e.cmd || 'command')} for ${esc(e.machine || 'a machine')}`, kind: 'control' };
  return { icon: '•', text: esc(k), sub: JSON.stringify(e).slice(0, 80), kind: 'other' };
}
let auditFilter = 'all';
function renderAudit(entries) {
  const kinds = { all: 'All', brain: 'Config edits', triage: 'Triage', playbook: 'Playbooks', control: 'Control' };
  const rows = entries.map(e => ({ e, h: humanizeAudit(e) })).filter(x => auditFilter === 'all' || x.h.kind === auditFilter);
  $('audit').innerHTML =
    `<div class="fleet-head"><h2>Activity log <span class="qi" title="Every change this dashboard made — config edits, triage decisions, saved playbooks, and any control actions — newest first. Nothing that changes state happens without an entry here.">ⓘ</span></h2>
      <span class="dim">${entries.length} recorded</span></div>
    <div class="seg" id="auditSeg" style="margin-bottom:14px">${Object.entries(kinds).map(([v, l]) => `<button data-a="${v}" class="${auditFilter === v ? 'on' : ''}">${l}</button>`).join('')}</div>
    <div class="audit-list">${rows.length ? rows.map(({ e, h }) => `
      <div class="audit-row" ${h.file ? `data-brainpath="${esc(encodeURIComponent(h.file))}"` : ''}>
        <span class="au-icon">${h.icon}</span>
        <div class="au-body"><div class="au-text">${h.text}</div>${h.sub ? `<div class="au-sub">${esc(String(h.sub))}</div>` : ''}</div>
        <span class="au-when">${fmtAgo(e.at)}</span>
      </div>`).join('') : '<div class="dim" style="padding:16px">No activity yet. Edits, triage, and saves will appear here.</div>'}</div>`;
  $('auditSeg').querySelectorAll('button').forEach(b => b.onclick = () => { auditFilter = b.dataset.a; renderAudit(entries); });
  $('audit').querySelectorAll('.audit-row[data-brainpath]').forEach(el => el.onclick = async () => {
    // jump into the Brain editor for the file this entry touched
    state.view = 'brain'; setTabs(); await loadBrain();
    const item = brainItems.find(i => i.id === el.dataset.brainpath);
    if (item) { const r = await (await fetch('/api/brain/file?id=' + item.id)).json(); if (!r.error) { brainCurrent = r; brainDirty = false; brainMode = 'view'; renderBrain(); } }
  });
}

// ---------- BRAIN view (memories, hooks, agent configs on this machine) ----------
let brainItems = [], brainCurrent = null, brainDirty = false, brainMode = 'view';

// plain-language meanings for common settings.json keys
const SETTINGS_HELP = {
  hooks: 'Shell commands Claude Code runs automatically at lifecycle moments (before/after a tool, on session start, etc.). These execute with full shell access — the most powerful and most dangerous section.',
  permissions: 'Allow/deny rules for what tools and commands run without asking. "allow" auto-approves matching actions; "deny" blocks them.',
  env: 'Environment variables set for every session.',
  model: 'Default model for this scope.',
  statusLine: 'Custom status-line command shown in the terminal.',
  includeCoAuthoredBy: 'Whether commits add the "Co-Authored-By: Claude" trailer.',
  enableAllProjectMcpServers: 'Auto-enable all MCP servers defined in the project.',
  autoMode: 'Autonomous-mode environment/permission configuration.',
};
const HOOK_EVENT_HELP = {
  PreToolUse: 'Runs BEFORE a tool executes — can inspect or block it.',
  PostToolUse: 'Runs AFTER a tool finishes — good for formatting, linting, notifications.',
  UserPromptSubmit: 'Runs when you submit a message.',
  SessionStart: 'Runs when a session begins.',
  Stop: 'Runs when the assistant finishes responding.',
  SubagentStop: 'Runs when a subagent finishes.',
  Notification: 'Runs on notifications (e.g. permission prompts).',
};
// Render a structured, annotated view of settings.json with hook enable/disable toggles.
// safe, cross-platform-ish starter hooks a non-technical user can add with one click
const IS_WIN = /win/i.test(navigator.platform || navigator.userAgent || '');
const HOOK_STARTERS = [
  { label: 'Ping when a task finishes', desc: 'Desktop notification each time the agent stops.', event: 'Stop',
    hook: { hooks: [{ type: 'command', command: IS_WIN ? 'powershell -c "[console]::beep(800,300)"' : 'printf "\\a"' }] } },
  { label: 'Log every edit', desc: 'Append the file path to ~/.claude/edit-log.txt after each file edit.', event: 'PostToolUse',
    hook: { matcher: 'Edit|Write', hooks: [{ type: 'command', command: 'echo "$(date): edited a file" >> ~/.claude/edit-log.txt' }] } },
  { label: 'Announce session start', desc: 'Writes a marker line when a session begins.', event: 'SessionStart',
    hook: { hooks: [{ type: 'command', command: 'echo "session started $(date)" >> ~/.claude/session-log.txt' }] } },
];
function addStarterHook(i) {
  const s = HOOK_STARTERS[i]; if (!s) return;
  let cfg; try { cfg = JSON.parse(brainCurrent.content); } catch { return; }
  cfg.hooks = cfg.hooks || {};
  cfg.hooks[s.event] = cfg.hooks[s.event] || [];
  cfg.hooks[s.event].push(JSON.parse(JSON.stringify(s.hook)));
  brainCurrent.content = JSON.stringify(cfg, null, 2);
  brainDirty = true;
  renderBrain();
}
function renderHooksExplainer(content) {
  let cfg;
  try { cfg = JSON.parse(content); } catch { return '<div class="dim" style="padding:16px">This file isn\'t valid JSON right now — fix it in Edit mode first.</div>'; }
  let html = '<div class="hooks-explain">';
  // top-level sections explained
  html += '<div class="he-sec"><h4>Sections in this file</h4>';
  for (const k of Object.keys(cfg)) {
    html += `<div class="he-row"><b>${esc(k)}</b><span>${esc(SETTINGS_HELP[k] || 'Configuration for ' + k + '.')}</span></div>`;
  }
  html += '</div>';
  // hooks detail with toggles
  const hooks = cfg.hooks || {};
  const disabled = cfg._disabledHooks || {};
  html += '<div class="he-sec"><h4>Hooks — enable / disable</h4><div class="dim" style="margin-bottom:8px">Toggling moves a hook between the live <code>hooks</code> block and a parked <code>_disabledHooks</code> block (Claude Code ignores unknown keys), so you can turn one off without deleting it. Save to apply.</div>';
  const allEvents = [...new Set([...Object.keys(hooks), ...Object.keys(disabled)])];
  if (!allEvents.length) html += `<div class="hooks-empty">
      <p>This file has no hooks yet. Hooks are little commands that run automatically at moments in a session — like a desktop ping when a task finishes, or auto-formatting after an edit.</p>
      <p class="dim">Add a ready-made one below (you can turn it off or tweak it anytime). It saves into this file when you hit Save.</p>
      <div class="hook-starters">
        ${HOOK_STARTERS.map((s, i) => `<button class="hook-starter" data-i="${i}"><b>＋ ${esc(s.label)}</b><span>${esc(s.desc)}</span></button>`).join('')}
      </div>
    </div>`;
  for (const ev of allEvents) {
    html += `<div class="he-event"><div class="he-event-h">${esc(ev)} <span class="dim">— ${esc(HOOK_EVENT_HELP[ev] || 'lifecycle event')}</span></div>`;
    const live = hooks[ev] || [];
    const off = disabled[ev] || [];
    live.forEach((h, i) => { html += hookRow(ev, i, h, true); });
    off.forEach((h, i) => { html += hookRow(ev, i, h, false); });
    html += '</div>';
  }
  html += '</div></div>';
  return html;
}
function hookRow(ev, idx, hook, on) {
  const cmds = (hook.hooks || []).map(h => h.command || h.type).join('; ');
  const matcher = hook.matcher ? ` [${hook.matcher}]` : '';
  return `<div class="he-hook ${on ? '' : 'off'}">
    <label class="he-toggle"><input type="checkbox" ${on ? 'checked' : ''} data-ev="${esc(ev)}" data-idx="${idx}" data-on="${on}"><span></span></label>
    <div class="he-hook-body"><code>${esc(matcher)}</code> ${esc(cmds.slice(0, 200))}</div>
  </div>`;
}
// apply a hook toggle to the JSON and stage it for save
function toggleHook(ev, idx, wasOn) {
  let cfg; try { cfg = JSON.parse(brainCurrent.content); } catch { return; }
  cfg.hooks = cfg.hooks || {}; cfg._disabledHooks = cfg._disabledHooks || {};
  const from = wasOn ? cfg.hooks : cfg._disabledHooks;
  const to = wasOn ? cfg._disabledHooks : cfg.hooks;
  if (!from[ev] || !from[ev][idx]) return;
  const [moved] = from[ev].splice(idx, 1);
  to[ev] = to[ev] || []; to[ev].push(moved);
  if (!from[ev].length) delete from[ev];
  if (cfg._disabledHooks && !Object.keys(cfg._disabledHooks).length) delete cfg._disabledHooks;
  brainCurrent.content = JSON.stringify(cfg, null, 2);
  brainDirty = true;
  renderBrain();
}

// line-level diff (added / removed counts + a preview) for the confirm dialog
function miniDiff(before, after) {
  const a = before.split('\n'), b = after.split('\n');
  const setA = new Set(a), setB = new Set(b);
  const added = b.filter(l => !setA.has(l));
  const removed = a.filter(l => !setB.has(l));
  return { added, removed };
}
function confirmDiff(name, diff, isHooks) {
  const lines = [];
  lines.push(`Save changes to ${name}?`);
  lines.push(`  +${diff.added.length} added, −${diff.removed.length} removed lines`);
  if (diff.removed.length) lines.push('\nRemoving:\n' + diff.removed.slice(0, 6).map(l => '  − ' + l.slice(0, 70)).join('\n'));
  if (diff.added.length) lines.push('\nAdding:\n' + diff.added.slice(0, 6).map(l => '  + ' + l.slice(0, 70)).join('\n'));
  if (isHooks) lines.push('\n⚠ THIS FILE DEFINES HOOKS/PERMISSIONS. Hooks run as shell commands with full trust every time an agent acts. A bad edit here can execute arbitrary code. Only save if you wrote this.');
  lines.push('\nThis write will be recorded in the audit log.');
  return confirm(lines.join('\n'));
}

// pretty JSON with lightweight syntax coloring (tokenize raw text, escape per token)
function highlightJSON(src) {
  let text = src;
  try { text = JSON.stringify(JSON.parse(src), null, 2); } catch { /* leave as-is */ }
  const re = /("(?:[^"\\]|\\.)*")(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g;
  let out = '', last = 0, m;
  while ((m = re.exec(text))) {
    out += esc(text.slice(last, m.index));
    if (m[1]) out += m[2] ? `<span class="hj-key">${esc(m[1])}</span>${m[2]}` : `<span class="hj-str">${esc(m[1])}</span>`;
    else if (m[3]) out += `<span class="hj-kw">${m[3]}</span>`;
    else out += `<span class="hj-num">${esc(m[0])}</span>`;
    last = m.index + m[0].length;
  }
  return out + esc(text.slice(last));
}
// minimal, safe markdown renderer (escape first, then transform)
function renderMD(src) {
  const lines = esc(src).split('\n');
  let out = '', inCode = false, inList = false;
  const closeList = () => { if (inList) { out += '</ul>'; inList = false; } };
  for (const raw of lines) {
    if (raw.startsWith('```')) { closeList(); out += inCode ? '</pre>' : '<pre class="md-code">'; inCode = !inCode; continue; }
    if (inCode) { out += raw + '\n'; continue; }
    let l = raw
      .replace(/`([^`]+)`/g, '<code class="md-ic">$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>')
      .replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<i>$2</i>')
      .replace(/\[\[([^\]]+)\]\]/g, '<span class="md-wiki">$1</span>');
    const h = /^(#{1,4})\s+(.*)/.exec(l);
    if (h) { closeList(); out += `<div class="md-h md-h${h[1].length}">${h[2]}</div>`; continue; }
    if (/^\s*[-*]\s+/.test(l)) { if (!inList) { out += '<ul class="md-ul">'; inList = true; } out += `<li>${l.replace(/^\s*[-*]\s+/, '')}</li>`; continue; }
    closeList();
    if (/^---+\s*$/.test(l)) { out += '<hr class="md-hr">'; continue; }
    out += l ? `<p class="md-p">${l}</p>` : '';
  }
  closeList(); if (inCode) out += '</pre>';
  return out;
}
async function loadBrain() {
  try {
    const r = await (await fetch('/api/brain')).json();
    brainItems = r.items || [];
  } catch { brainItems = []; }
  renderBrain();
}
function renderBrain() {
  const cats = [...new Set(brainItems.map(i => i.category))];
  $('brain').innerHTML = `
    <div class="brain-side">
      <div class="fleet-head" style="margin-bottom:8px"><h2>🧠 Brain <span class="qi" title="The files that steer your agents on THIS machine: Claude's memories, hook settings, and Codex instructions. Edits save locally with a one-step backup (.mc-backup). Remote machines' brains are read-only for now — a future relay update will surface them.">ⓘ</span></h2></div>
      ${cats.map(c => `<div class="brain-cat">${esc(c)}</div>` + brainItems.filter(i => i.category === c).map(i => `
        <div class="brain-item ${brainCurrent?.id === i.id ? 'on' : ''}" data-id="${esc(i.id)}">
          <span class="bi-name">${esc(i.name)}</span>
          <span class="bi-meta">${(i.size / 1024).toFixed(1)}KB · ${fmtAgo(i.mtime)}</span>
        </div>`).join('')).join('') || '<div class="dim" style="padding:10px">no brain files found</div>'}
    </div>
    <div class="brain-main">
      ${brainCurrent ? (() => {
        const isJSON = /\.json$/i.test(brainCurrent.name) || /^\s*[{[]/.test(brainCurrent.content);
        const isMD = /\.md/i.test(brainCurrent.name);
        const isHooks = /settings\.json/i.test(brainCurrent.name) || /"hooks"\s*:/.test(brainCurrent.content);
        const viewable = isJSON || isMD;
        let bodyHtml;
        if (brainMode === 'hooks' && isHooks) bodyHtml = `<div id="brainViewer">${renderHooksExplainer(brainCurrent.content)}</div>`;
        else if (brainMode === 'view' && viewable) bodyHtml = `<div id="brainViewer" class="${isJSON ? 'bv-json' : 'bv-md'}">${isJSON ? `<pre class="hj">${highlightJSON(brainCurrent.content)}</pre>` : renderMD(brainCurrent.content)}</div>`;
        else bodyHtml = `<textarea id="brainEditor" spellcheck="false">${esc(brainCurrent.content)}</textarea>`;
        return `
        <div class="brain-bar">
          <b>${esc(brainCurrent.name)}</b>
          <span class="dim" style="font-size:10.5px">${esc(brainCurrent.path)}</span>
          ${viewable ? `<div class="seg" id="brainModeSeg"><button data-m="view" class="${brainMode === 'view' ? 'on' : ''}">Read</button>${isHooks ? `<button data-m="hooks" class="${brainMode === 'hooks' ? 'on' : ''}">Hooks</button>` : ''}<button data-m="edit" class="${brainMode === 'edit' ? 'on' : ''}">Edit</button></div>` : ''}
          <button id="brainHist" class="mini-btn" title="version history">⟲ History</button>
          <button id="brainSave" class="mini-btn" ${brainDirty ? '' : 'disabled'}>${brainDirty ? '💾 Save' : 'Saved'}</button>
        </div>
        ${bodyHtml}
        <div id="brainHistPanel"></div>`;
      })()
        : '<div class="brain-empty">Pick a file on the left.<br><span class="dim">These are the instructions and memories your agents wake up with — editing them here changes how every future session behaves.</span></div>'}
    </div>`;
  $('brain').querySelectorAll('.brain-item').forEach(el => el.onclick = async () => {
    if (brainDirty && !confirm('Discard unsaved changes?')) return;
    const r = await (await fetch('/api/brain/file?id=' + el.dataset.id)).json();
    if (r.error) return alert(r.error);
    brainCurrent = r; brainDirty = false; brainMode = 'view'; renderBrain();
  });
  $('brain').querySelector('#brainModeSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => {
    // hooks mode keeps staged edits (toggles live there); switching to Read discards raw-text edits
    if (brainDirty && b.dataset.m === 'view' && !confirm('Switch to Read and discard unsaved edits?')) return;
    if (brainDirty && b.dataset.m === 'view') brainDirty = false;
    brainMode = b.dataset.m; renderBrain();
  });
  $('brain').querySelectorAll('.he-toggle input').forEach(t => t.onchange = () => toggleHook(t.dataset.ev, Number(t.dataset.idx), t.dataset.on === 'true'));
  $('brain').querySelectorAll('.hook-starter').forEach(b => b.onclick = () => addStarterHook(Number(b.dataset.i)));
  const hb = $('brainHist');
  if (hb) hb.onclick = async () => {
    const panel = $('brainHistPanel');
    if (panel.dataset.open === '1') { panel.dataset.open = '0'; panel.innerHTML = ''; return; }
    panel.dataset.open = '1';
    const h = (await (await fetch('/api/brain/history?id=' + brainCurrent.id)).json()).history || [];
    panel.innerHTML = `<div class="brain-hist"><div class="bh-head">Version history (${h.length})</div>` +
      (h.length ? h.map(v => `<div class="bh-row" data-stamp="${v.stamp}"><span>${new Date(Number(v.stamp)).toLocaleString()}</span><span class="dim">${(v.size/1024).toFixed(1)}KB</span><button class="mini-btn bh-restore">restore</button></div>`).join('')
        : '<div class="dim" style="padding:8px">No prior versions yet — history starts at your first save here.</div>') + '</div>';
    panel.querySelectorAll('.bh-row').forEach(row => row.querySelector('.bh-restore').onclick = async () => {
      const snap = await (await fetch('/api/brain/snapshot?id=' + brainCurrent.id + '&stamp=' + row.dataset.stamp)).json();
      if (snap.error) return alert(snap.error);
      if (!confirm('Restore this version? Your current content will be snapshotted first, then replaced.')) return;
      const r = await fetch('/api/brain/file', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf }, body: JSON.stringify({ id: brainCurrent.id, content: snap.content, baseMtime: brainCurrent.mtime }) });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) return alert(j.error || 'restore failed');
      brainCurrent.content = snap.content; brainCurrent.mtime = j.mtime; brainDirty = false; brainMode = 'view'; renderBrain();
    });
  };
  const ed = $('brainEditor');
  if (ed) {
    ed.oninput = () => { if (!brainDirty) { brainDirty = true; const b = $('brainSave'); b.disabled = false; b.textContent = '💾 Save'; } };
    $('brainSave').onclick = async () => {
      // diff + confirm before writing; loudest warning for hooks/settings (they execute)
      const before = brainCurrent.content, after = ed.value;
      if (before === after) return;
      const isHooks = /settings/i.test(brainCurrent.name);
      const diff = miniDiff(before, after);
      if (!confirmDiff(brainCurrent.name, diff, isHooks)) return;
      const r = await fetch('/api/brain/file', {
        method: 'POST', headers: { 'Content-Type': 'application/json', 'X-MC-CSRF': metaCsrf },
        body: JSON.stringify({ id: brainCurrent.id, content: after, baseMtime: brainCurrent.mtime }),
      });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) return alert(j.error || 'save failed');
      brainCurrent.content = after; brainCurrent.mtime = j.mtime; brainDirty = false;
      const b = $('brainSave'); b.disabled = true; b.textContent = '✓ Saved (audited)';
    };
  }
}

// ---------- AUDIT view (immutable record of state-changing actions) ----------
const AUDIT_ICON = { 'brain-write': '✍️', launch: '🚀', 'launch-end': '🏁', kill: '🛑', approval: '✅', enqueue: '📤' };
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
  const archives = await (await fetch('/api/archive')).json().catch(() => ({ archives: [] }));
  const archByMachine = {}; for (const a of (archives.archives || [])) archByMachine[a.machine] = a;
  $('machines').innerHTML =
    `<div class="fleet-head"><h2>Machines — ${machinesData.length}</h2></div>` +
    `<div class="machine-grid">` + machinesData.map(m => {
      const st = byMachine[m.name] || { sessions: 0, agents: 0, cost: 0, kinds: {} };
      const fresh = Date.now() - m.lastSeen < 120000;
      const kindDots = Object.entries(st.kinds).map(([k, n]) => `<span class="mkind" style="color:${kindColor(k)}">● ${(AGENT_KIND[k] || AGENT_KIND.claude).label} ${n}</span>`).join('');
      const hubV = machinesData.find(x => !x.remote)?.version;
      const drift = m.version && hubV && m.version !== hubV;
      const arch = archByMachine[safeName(m.name)] || archByMachine[m.name];
      return `<div class="mcard ${fresh ? 'fresh' : ''}">
        <h3>${m.remote ? '⇄' : '★'} ${esc(m.name)} ${m.version ? `<span class="mver ${drift ? 'drift' : ''}" title="${drift ? 'version differs from hub v' + esc(hubV) : 'app version'}">v${esc(m.version)}${drift ? ' ⚠' : ''}</span>` : ''} <span class="mstatus ${fresh ? 'on' : ''}">${fresh ? 'live' : 'idle'}</span></h3>
        <div class="mips">${(m.ips || []).map(ip => `<span class="ip">${esc(ip)}</span>`).join('') || '<span class="ip dim">no IPs reported</span>'}</div>
        <div class="mstats"><span><b>${st.sessions}</b> sessions</span><span><b>${st.agents}</b> agents</span><span class="fcost"><b>~${fmtUsd(st.cost)}</b></span></div>
        <div class="mkinds">${kindDots}</div>
        ${arch && arch.files ? `<button class="mini-btn arch-browse" data-machine="${esc(arch.machine)}">📚 ${arch.files} archived transcripts · ${fmtBytes(arch.bytes)} — browse</button>` : ''}
        <div class="fdate">last seen ${new Date(m.lastSeen).toLocaleString()}</div>
      </div>`;
    }).join('') + `</div>`;
  $('machines').querySelectorAll('.arch-browse').forEach(b => b.onclick = () => openArchiveBrowser(b.dataset.machine));
}
const fmtBytes = n => n >= 1e9 ? (n / 1e9).toFixed(1) + 'GB' : n >= 1e6 ? (n / 1e6).toFixed(0) + 'MB' : (n / 1e3).toFixed(0) + 'KB';
function safeName(m) { return String(m || '').replace(/[^\w.-]+/g, '_').slice(0, 60); }
async function openArchiveBrowser(machine) {
  const data = await (await fetch('/api/archive/list?machine=' + encodeURIComponent(machine))).json();
  const ov = document.createElement('div'); ov.className = 'pb-editor-ov'; ov.onclick = e => { if (e.target === ov) ov.remove(); };
  ov.innerHTML = `<div class="handle-modal">
    <div class="hm-head"><b>📚 Archived transcripts — ${esc(machine)}</b><input id="archSearch" placeholder="filter…" style="margin-left:12px;flex:1;background:var(--panel2);border:1px solid var(--line);color:var(--text);border-radius:6px;padding:5px 9px;font-size:12px"><button class="mini-btn" id="archClose">✕</button></div>
    <div class="hm-body"><div class="dim" style="margin-bottom:8px">${data.items.length} full transcripts, pulled raw from ${esc(machine)}. Click one to open it in the session viewers (Board / Story / Waterfall).</div>
      <div id="archList">${archListHTML(data.items)}</div></div></div>`;
  document.body.appendChild(ov);
  ov.querySelector('#archClose').onclick = () => ov.remove();
  const wire = () => ov.querySelectorAll('.arch-item').forEach(el => el.onclick = () => { ov.remove(); openSession(el.dataset.file); state.view = 'story'; setTabs(); render(); });
  wire();
  ov.querySelector('#archSearch').oninput = e => {
    const q = e.target.value.toLowerCase();
    ov.querySelector('#archList').innerHTML = archListHTML(data.items.filter(i => (i.title + i.rel).toLowerCase().includes(q)));
    wire();
  };
}
function archListHTML(items) {
  return items.map(i => `<div class="arch-item" data-file="${esc(i.file)}">
    <div class="ai-t">${esc(i.title)}</div>
    <div class="ai-m dim">${esc(i.rel)} · ${fmtBytes(i.size)} · ${i.mtime ? fmtAgo(i.mtime) : ''}</div>
  </div>`).join('') || '<div class="dim" style="padding:12px">no matches</div>';
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
  loadStickiness(file); // lazy, one session at a time — never on fleet load
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

// ---------- did it stick (is this session's work still in the code?) ----------
// An agent can report success and leave nothing behind. Asked once per opened
// session, never for the whole fleet — the server has to run git to answer, so
// scoring every card on every refresh would be dozens of git runs a minute.
// A session that edited nothing, or that the server could not prove either way,
// gets a plain sentence or nothing at all — never a scary badge.
let stickState = { file: null, data: null };
async function loadStickiness(file) {
  stickState = { file, data: null };
  renderStickbar();
  if (BAKED) return; // exported replay: no server to ask, and no repo to ask about
  try {
    const d = await (await fetch('/api/stickiness?file=' + encodeURIComponent(file))).json();
    if (stickState.file !== file) return; // opened something else while we waited
    stickState.data = d;
  } catch { /* stay silent rather than guess */ }
  renderStickbar();
}
function stickSentence(d) {
  const n = d.changed, miss = (d.missing || []).length;
  const files = n === 1 ? '1 file' : `${n} files`;
  if (miss === 0) return `Changed ${files} — ${n === 1 ? 'that change is' : `all ${n} of those changes are`} in your code now.`;
  if (d.landed === 0) return `Changed ${files} — ${n === 1 ? 'that change is not' : 'none of those changes are'} in your code now.`;
  return `Changed ${files} — ${d.landed} of those changes ${d.landed === 1 ? 'is' : 'are'} in your code now, ${miss} ${miss === 1 ? 'is' : 'are'} not.`;
}
function renderStickbar() {
  const bar = $('stickbar');
  const d = stickState.data;
  if (!d || OVERVIEW.includes(state.view)) { bar.style.display = 'none'; return; }
  const scored = d.status === 'stuck' || d.status === 'partial' || d.status === 'gone';
  if (!scored) {
    // 'unknown' / 'not-scored': the plain reason, no colour, no verdict
    if (!d.reason) { bar.style.display = 'none'; return; }
    bar.className = '';
    bar.innerHTML = `<span class="stick-note">${esc(d.reason)}</span>`;
    bar.style.display = '';
    return;
  }
  const names = (d.missing || []).slice(0, 4).map(p => esc(p.split('/').pop()));
  const more = (d.missing || []).length - names.length;
  bar.className = d.status === 'gone' ? 'stick-bad' : d.status === 'partial' ? 'stick-partial' : 'stick-good';
  bar.innerHTML =
    `<div><b>${d.status === 'gone' ? 'This session reported work, but nothing it wrote in this project is in your code now.' : esc(stickSentence(d))}</b></div>` +
    `<div class="stick-note">${d.status === 'gone' ? esc(stickSentence(d)) + ' ' : ''}` +
    (names.length ? `Unchanged: ${names.join(', ')}${more > 0 ? ` and ${more} more` : ''}. ` : '') +
    `${esc(d.reason || '')}</div>`;
  bar.style.display = '';
}

// one plain sentence on what a broken session got stuck on — reuses the same
// deterministic detector Playbook Studio uses, computed client-side (no fetch)
function renderFailbar() {
  const bar = $('failbar');
  if (!bar) return;
  if (OVERVIEW.includes(state.view) || !state.data.events.length || !sessionFailed()) { bar.style.display = 'none'; return; }
  const diag = diagnoseFailure(state.data);
  if (!diag) { bar.style.display = 'none'; return; }
  bar.innerHTML = `💥 ${esc(failureSentence(diag))}`;
  bar.style.display = '';
}

// ---------- render ----------
// (OVERVIEW is declared near `state` above — the home-view preference needs it first)
function setTabs() {
  for (const [btn, v] of [['viewFleet', 'fleet'], ['viewTable', 'table'], ['viewFingerprints', 'fingerprints'], ['viewCalendar', 'calendar'], ['viewRings', 'rings'], ['viewRhythm', 'rhythm'], ['viewProjects', 'projects'], ['viewUsage', 'usage'], ['viewFlows', 'flows'], ['viewPlaybooks', 'playbooks'], ['viewBrain', 'brain'], ['viewAudit', 'audit'], ['viewConstellation', 'constellation'], ['viewMachines', 'machines'], ['viewBoard', 'board'], ['viewStory', 'story'], ['viewLanes', 'lanes'], ['viewWaterfall', 'waterfall'], ['viewCost', 'costflow'], ['viewTimeline', 'timeline']]) {
    const el = $(btn); if (el) el.classList.toggle('on', state.view === v);
  }
  const overview = OVERVIEW.includes(state.view);
  document.querySelector('main').classList.toggle('no-feed', overview);
  $('empty').style.display = 'none'; // only board/timeline turn it back on
  for (const id of ['fleet', 'tableView', 'fingerprints', 'calendar', 'rings', 'rhythm', 'projects', 'usage', 'flows', 'playbooks', 'brain', 'audit', 'constellation', 'machines']) $(id).style.display = (state.view === id.replace('View', '')) ? '' : 'none';
  if (overview) for (const p of SESSION_PANES) $(p).style.display = 'none';
  $('feed').style.display = overview ? 'none' : '';
  document.querySelector('footer').style.display = overview ? 'none' : '';
  $('statbar').style.display = overview ? 'none' : '';
  renderStickbar(); // hides itself on the overview tabs
  renderFailbar(); // ditto
  stopConstellation();
  if (state.view === 'fleet') loadFleet();
  else if (state.view === 'table') loadTable();
  else if (state.view === 'fingerprints') loadFingerprints();
  else if (state.view === 'calendar') loadCalendar();
  else if (state.view === 'rings') loadRings();
  else if (state.view === 'rhythm') loadRhythm();
  else if (state.view === 'projects') loadProjects();
  else if (state.view === 'usage') loadUsage();
  else if (state.view === 'flows') loadFlows();
  else if (state.view === 'playbooks') loadPlaybooks();
  else if (state.view === 'brain') loadBrain();
  else if (state.view === 'audit') loadAudit();
  else if (state.view === 'constellation') loadConstellation();
  else if (state.view === 'machines') loadMachines();
}

function render() {
  if (OVERVIEW.includes(state.view)) return;
  renderStatbar();
  renderStickbar();
  renderFailbar();
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
      // orchestrator: a tiny tail of the actual conversation + dispatches
      let convo = '';
      if (a.id === 'main') {
        const tail = state.data.events.slice(0, state.scrub)
          .filter(e => e.agent === 'main' && ['user-text', 'assistant-text', 'spawn'].includes(e.kind))
          .slice(-4);
        if (tail.length) convo = `<div class="mini-convo">` + tail.map(e => {
          if (e.kind === 'spawn') {
            const who = state.data.agents.find(x => x.id === e.spawnedAgent)?.name || 'agent';
            return `<div class="mc-line mc-spawn" data-seq="${e.seq}">🚀 → <b>${esc(who.slice(0, 20))}</b>: ${esc((e.text || '').slice(0, 54))}</div>`;
          }
          const you = e.kind !== 'assistant-text';
          return `<div class="mc-line ${you ? 'mc-you' : 'mc-ai'}" data-seq="${e.seq}"><b>${you ? 'You' : 'AI'}:</b> ${esc((e.text || '').replace(/\s+/g, ' ').slice(0, 76))}</div>`;
        }).join('') + `</div>`;
      }
      el.innerHTML =
        `<h2>${a.id === 'main' ? '🛰️' : '🤖'} <span class="nm">${esc(a.name || a.id)}</span> <span class="status ${st}">${st}</span></h2>` +
        (a.task ? `<div class="task">${esc(a.task)}</div>` : '') +
        `<div class="meta"><span>ev <b>${a.events}</b></span><span>out <b>${fmtTok(a.outTokens)}</b></span><span><b>${fmtDur(dur)}</b></span>${costTag}` +
        (a.errors ? `<span class="err">err <b>${a.errors}</b></span>` : '') + `</div>` +
        convo +
        sparkline(a.recent, a.firstTs || firstTs, a.lastTs || lastTs) +
        (chips ? `<div class="chips">${chips}</div>` : '');
    }
    el.querySelectorAll('.mc-line').forEach(l => l.onclick = ev2 => { ev2.stopPropagation(); openDrawer(Number(l.dataset.seq)); });
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
$('viewFingerprints').onclick = () => { if (!BAKED) { state.view = 'fingerprints'; setTabs(); } };
$('viewCalendar').onclick = () => { if (!BAKED) { state.view = 'calendar'; setTabs(); } };
$('viewRings').onclick = () => { if (!BAKED) { state.view = 'rings'; setTabs(); } };
$('viewRhythm').onclick = () => { if (!BAKED) { state.view = 'rhythm'; setTabs(); } };
$('viewProjects').onclick = () => { if (!BAKED) { state.view = 'projects'; setTabs(); } };
$('viewUsage').onclick = () => { if (!BAKED) { state.view = 'usage'; setTabs(); } };
$('viewFlows').onclick = () => { if (!BAKED) { state.view = 'flows'; setTabs(); } };
$('viewPlaybooks').onclick = () => { if (!BAKED) { state.view = 'playbooks'; setTabs(); } };
$('viewBrain').onclick = () => { if (!BAKED) { state.view = 'brain'; setTabs(); } };
$('viewAudit').onclick = () => { if (!BAKED) { state.view = 'audit'; setTabs(); } };
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
  for (const id of ['viewFleet', 'viewTable', 'viewFingerprints', 'viewCalendar', 'viewRings', 'viewRhythm', 'viewProjects', 'viewUsage', 'viewFlows', 'viewPlaybooks', 'viewBrain', 'viewAudit', 'viewConstellation', 'viewMachines']) { const el = $(id); if (el) el.style.display = 'none'; }
  $('exportBtn').style.display = 'none';
  $('liveBtn').style.display = 'none';
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'replay';
  setTabs(); render();
} else {
  loadSessions();
  loadMeta().then(() => setTabs());
  startNotifications();
}
