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

// ---------- model + tier identity (server already sorted each agent's model into
// a tier — flagship/premium/mid/cheap/unknown — see modelTier() in server.js;
// this is just the ONE color/label scheme every view below shares, so a wall of
// pricey runs reads the same way in Fleet, Table, Fingerprints, and the Galaxy).
// Colour runs hot (flagship, priciest) to cool (cheap) so "expensive" is legible
// at a glance without reading a single number.
const TIERS = ['flagship', 'premium', 'mid', 'cheap', 'unknown'];
const TIER_COLOR = { flagship: 'var(--red)', premium: 'var(--amber)', mid: 'var(--blue)', cheap: 'var(--green)', unknown: 'var(--dim)' };
// Literal hex twin of TIER_COLOR, kept in sync with the --red/--amber/--blue/
// --green/--dim values in style.css by hand (same pattern kindColor() uses).
// Needed anywhere a var(--x) reference won't work: Canvas fillStyle can't
// resolve CSS custom properties at all, and an inline `background:${col}22`
// alpha-suffix trick needs a real hex string — `var(--amber)22` is invalid CSS.
const TIER_COLOR_HEX = { flagship: '#f87171', premium: '#fbbf24', mid: '#60a5fa', cheap: '#34d399', unknown: '#8a93a8' };
const TIER_LABEL = { flagship: 'Flagship', premium: 'Premium', mid: 'Mid', cheap: 'Cheap', unknown: 'Unknown' };
// Shorten a model id for a chip without losing the version — 'claude-opus-4-8'
// -> 'Opus 4.8', 'claude-fable-5' -> 'Fable 5'. The FULL id always stays in the
// tooltip (see modelChips) because that's the only place a '4' vs '5' is provable.
function modelShortName(id) {
  const s = String(id || '').replace(/^(us\.|eu\.)?anthropic\./, '').replace(/^claude-/, '');
  const parts = s.split(/[-_]/).filter(Boolean);
  if (!parts.length) return id ? String(id) : 'unknown';
  const fam = parts[0].charAt(0).toUpperCase() + parts[0].slice(1);
  const rest = parts.slice(1).join('.');
  return rest ? `${fam} ${rest}` : fam;
}
// Which tier a session's money (or, lacking any cost, its agent headcount)
// leans on hardest. Returns null only when the session has no model data at
// all — callers fall back to TIER_COLOR.unknown / var(--line) for that case.
function dominantTier(s) {
  const mix = (s && s.tierMix) || {};
  let best = null, bestV = 0;
  for (const t of TIERS) { const v = mix[t] || 0; if (v > bestV) { bestV = v; best = t; } }
  if (best) return best;
  const byTier = {};
  for (const m of (s && s.models) || []) byTier[m.tier] = (byTier[m.tier] || 0) + m.agents;
  let bestA = null, bestAV = 0;
  for (const t of TIERS) { const v = byTier[t] || 0; if (v > bestAV) { bestAV = v; bestA = t; } }
  return bestA;
}
function tierColorOf(s) { const t = dominantTier(s); return t ? TIER_COLOR[t] : TIER_COLOR.unknown; }
// Compact tier-coloured chips: model short name + how many agents ran it. Full
// id + per-model cost in the tooltip. agentsNoModel gets its own grey chip so
// the counts always reconcile in the UI, same as they do in the data.
function modelChips(s, opts) {
  const max = (opts && opts.max) || 5;
  const models = s.models || [];
  if (!models.length && !s.agentsNoModel) return '';
  const shown = models.slice(0, max);
  const overflow = models.length - shown.length;
  let html = '<div class="model-chips">';
  html += shown.map(m => {
    // Inline style needs a literal hex to append an alpha suffix (var(--x)22 is
    // invalid CSS) — TIER_COLOR_HEX below, same trick kindColor() chips already use.
    const col = TIER_COLOR_HEX[m.tier] || TIER_COLOR_HEX.unknown;
    const tip = `${m.id} — ${m.agents} agent${m.agents === 1 ? '' : 's'} · ~${fmtUsd(m.cost)} · ${TIER_LABEL[m.tier] || m.tier}`;
    return `<span class="model-chip" style="background:${col}22;color:${col};border-color:${col}66" title="${esc(tip)}">${esc(modelShortName(m.id))} ×${m.agents}</span>`;
  }).join('');
  if (overflow > 0) html += `<span class="model-chip model-chip-more" title="${overflow} more model${overflow === 1 ? '' : 's'} not shown">+${overflow}</span>`;
  if (s.agentsNoModel) html += `<span class="model-chip model-chip-unknown" title="${s.agentsNoModel} agent${s.agentsNoModel === 1 ? '' : 's'} whose model was never recorded">no model ×${s.agentsNoModel}</span>`;
  html += '</div>';
  return html;
}
// Real dollars per tier for a table-cell tooltip — never a bare percentage
// with nothing behind it.
function tierMixTooltip(s) {
  const mix = s.tierMix || {};
  const parts = TIERS.filter(t => mix[t] > 0).map(t => `${TIER_LABEL[t]}: ${fmtUsd(mix[t])}`);
  return parts.length ? parts.join(' · ') : 'no cost recorded';
}

const KINDS = ['user-text', 'user-queued', 'assistant-text', 'tool-call', 'tool-result', 'spawn', 'spawn-result'];
const KIND_LABEL = { 'user-text': 'user', 'user-queued': 'queued', 'assistant-text': 'reply', 'tool-call': 'tool', 'tool-result': 'result', 'spawn': 'spawn', 'spawn-result': 'return' };
const KIND_COLOR = { 'user-text': '#f87171', 'user-queued': '#f87171', 'assistant-text': '#fbbf24', 'tool-call': '#818cf8', 'tool-result': '#60a5fa', 'spawn': '#5eead4', 'spawn-result': '#34d399' };

// ---------- overview tabs ----------
// Tabs that dispatch through setTabs() (own data load, no live feed/footer) rather
// than render() (which drives the session-detail panes). Declared up top because
// the home-view preference below needs it before `state` exists.
//
// VIEW_META + NAV_MENUS are the SINGLE source of truth for every overview view:
// icon/label/pane come from VIEW_META, menu grouping + order from NAV_MENUS. Adding
// a view later = one VIEW_META entry + one id in a menu's `views` array — OVERVIEW,
// the nav bar markup, the click dispatch, and the BAKED hide-list (which just hides
// the whole #navBar element) all derive from these two automatically.
const VIEW_META = {
  fleet:         { label: 'Fleet',         icon: '',   pane: 'fleet' },
  table:         { label: 'Table',         icon: '',   pane: 'tableView' },
  constellation: { label: 'Galaxy',        icon: '✦',  pane: 'constellation' },
  calendar:      { label: 'Calendar',      icon: '📅', pane: 'calendar' },
  fingerprints:  { label: 'Fingerprints',  icon: '🧬', pane: 'fingerprints' },
  rings:         { label: 'Rings',         icon: '🎯', pane: 'rings' },
  rhythm:        { label: 'Rhythm',        icon: '🕛', pane: 'rhythm' },
  trouble:       { label: 'Trouble files', icon: '🔥', pane: 'trouble' },
  unsaved:       { label: 'Unsaved',       icon: '👻', pane: 'unsaved' },
  leaks:         { label: 'Secrets',       icon: '🔑', pane: 'leaks' },
  flows:         { label: 'Flows',         icon: '⇶',  pane: 'flows' },
  playbooks:     { label: 'Playbooks',     icon: '📖', pane: 'playbooks' },
  brain:         { label: 'Brain',         icon: '🧠', pane: 'brain' },
  machines:      { label: 'Machines',      icon: '',   pane: 'machines' },
  projects:      { label: 'Projects',      icon: '',   pane: 'projects' },
  usage:         { label: 'Usage',         icon: '📈', pane: 'usage' },
  audit:         { label: 'Audit',         icon: '🛡', pane: 'audit' },
  divergence:    { label: 'Divergence',    icon: '🪜', pane: 'divergence' },
  graveyard:     { label: 'Graveyard',     icon: '🪦', pane: 'graveyard' },
  hookprops:     { label: 'Hook ideas',    icon: '🪝', pane: 'hookprops' },
  dejavu:        { label: 'Deja Vu',       icon: '🔁', pane: 'dejavu' },
  economics:     { label: 'Economics',     icon: '⚖',  pane: 'economics' },
};
const NAV_MENUS = [
  { key: 'sessions', label: 'Sessions', views: ['fleet', 'table', 'constellation', 'calendar', 'fingerprints', 'rings', 'rhythm'] },
  { key: 'health',   label: 'Health',   views: ['trouble', 'unsaved', 'leaks', 'flows', 'divergence', 'graveyard'] },
  { key: 'improve',  label: 'Improve',  views: ['economics', 'playbooks', 'brain', 'hookprops', 'dejavu'] },
  { key: 'system',   label: 'System',   views: ['machines', 'projects', 'usage', 'audit'] },
];
const OVERVIEW = NAV_MENUS.flatMap(m => m.views);

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
  if (b) b.onclick = () => { setHomeView(getHomeView() === id ? null : id); updateNavHome(); rerender(); };
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
    .filter(s => !q || ((s.title || '') + ' ' + (s.machine || '') + ' ' + machineLabel(s.machine) + ' ' + s.project).toLowerCase().includes(q))
    .slice(0, 80);
  $('spickerList').innerHTML = items.map(s => {
    const col = kindColor(s.kind);
    const m = metaMap[s.stableKey] || {};
    const proj = projectById(m.projectId);
    return `<div class="sp-item" data-file="${esc(s.file)}" style="border-left:3px solid ${col}">
      <div class="sp-title">${m.pinned ? '★ ' : ''}${esc(s.title || s.session.slice(0, 8))}</div>
      <div class="sp-meta"><span class="kind-badge" style="background:${col}22;color:${col}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span>
        ${proj ? `<span class="mini-badge" style="background:${proj.color}22;color:${proj.color}">${esc(proj.name)}</span>` : ''}
        <span title="${esc(machineTitle(s.machine))}">${esc(machineLabel(s.machine))}</span><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span>${s.agentCount ? `<span>${s.agentCount} agents</span>` : ''}</div>
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
let metaMachineNames = {}; // real machine name -> friendly display name
async function loadMeta() {
  try {
    const m = await (await fetch('/api/meta')).json();
    metaMap = m.sessions || {}; metaProjects = m.projects || []; metaTags = m.tags || [];
    metaCsrf = m.csrf; metaVersion = m.metaVersion; metaReadOnly = !!m.readOnly;
    metaMachineNames = m.machineNames || {};
  } catch { /* first load may race boot */ }
}
// Friendly label for a machine, everywhere one is shown — falls back to the
// real name when no rename has been set. Real name always stays available in
// a tooltip (machineTitle) so nothing becomes untraceable.
function machineLabel(name) { return metaMachineNames[name] || name || ''; }
function machineTitle(name) { const d = metaMachineNames[name]; return d ? `real name: ${name}` : ''; }
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
  if (e.key === 'Escape') { closeSearch(); closeNavMenus(); }
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
        <div class="so-t">${esc((h.title || '').slice(0, 60))} <span class="dim">· ${esc(machineLabel(h.machine))} · ${fmtAgo(h.mtime)}${h.tool ? ' · ' + esc(h.tool) : ''}</span></div>
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

// ---------- header version ----------
// The dashboard's own version was already computed server-side (APP_VERSION,
// read from package.json) and already served via /api/update-check for the
// stale-version notification below — this just also puts it somewhere the
// owner can actually see it. One fetch at boot, no polling of its own.
function loadAppVersion() {
  const el = $('appVersion');
  if (!el) return;
  fetch('/api/update-check').then(r => r.json()).then(v => {
    el.textContent = 'v' + v.current;
    el.title = v.updateAvailable ? `v${v.current} installed — v${v.latest} is available` : `v${v.current} — up to date`;
    el.classList.toggle('update-avail', !!v.updateAvailable);
  }).catch(() => { /* offline at boot — leave it blank rather than guess */ });
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
const machineSeen = new Map(); // name -> wasQuiet (learned-rhythm silence, not a flat timeout)
let updateNotified = false, budgetNotifiedDay = null;
async function pollNotifications() {
  let fleet;
  try { fleet = await (await fetch('/api/fleet')).json(); } catch { return; }
  // This poll runs every 15s regardless of which overview tab is open — piggyback
  // on it to keep the "at work now" strip current without a fetch of its own.
  fleetCache = fleet;
  renderLiveNowStrip();
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
  // machine silence: a live machine broke its OWN normal check-in rhythm.
  // Uses the same learned-rhythm signal as the Machines view — a flat timeout
  // can't tell a machine that just broke its rhythm from one that's merely
  // between check-ins (that used to fire a false "went silent" bell for any
  // machine that only reports occasionally). The bell fires once on the
  // transition into quiet; the persistent banner (renderMachineWarnBar) stays
  // up the whole time it's true, so seeing it never depends on the tab being
  // open at the exact moment the line was crossed.
  try {
    const ms = await (await fetch('/api/machines')).json();
    renderMachineWarnBar(ms);
    for (const m of ms.filter(x => x.remote)) {
      const isQuiet = !!(m.quiet && m.quiet.quiet);
      const was = machineSeen.get(m.name);
      if (was === false && isQuiet) pushNotif('error', `hasn't checked in for ${fmtDurWords(m.quiet.silenceMs)} — usually every ${fmtDurWords(m.quiet.medianGapMs)}`, { title: machineLabel(m.name), file: '', machine: m.name, kind: 'claude', session: m.name });
      machineSeen.set(m.name, isQuiet);
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
    try { new Notification(`${type === 'error' ? '⚠️' : '✅'} ${n.title}`, { body: `${msg} · ${n.machine ? machineLabel(n.machine) : ''}`, silent: type !== 'error' }); } catch { /* blocked */ }
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
      <div><div class="nt">${esc(n.title)}</div><div class="nm">${esc(n.msg)} · ${esc(machineLabel(n.machine))}</div></div>
      <span class="ndot" style="background:${kindColor(n.kind)}"></span>
    </div>`).join('') : '<div class="notif-empty">No alerts yet. Errors and finished long runs show here.</div>';
  $('notifList').querySelectorAll('.notif[data-file]').forEach(el => el.onclick = () => { $('notifPanel').classList.remove('open'); if (el.dataset.file) openSession(el.dataset.file); });
}
$('bell').onclick = () => { $('notifPanel').classList.toggle('open'); notifs.forEach(n => n.read = true); renderBell(); };
$('notifClear').onclick = (e) => { e.stopPropagation(); notifs.length = 0; renderBell(); };
document.addEventListener('click', e => { if (!$('notifPanel').contains(e.target) && e.target !== $('bell') && !$('bell').contains(e.target)) $('notifPanel').classList.remove('open'); });

let fleetCache = null;

// ---------- "at work now" strip ----------
// A one-line pulse of the fleet: who is actually working right now, across every
// session — driven entirely by liveAgents/liveAgentCount that the server already
// computed per session (10-min recency window, see liveAgentsOf() in server.js).
// No extra fetch of its own: this reads whatever fleetCache already holds, and
// gets refreshed by every view that (re)loads the fleet plus the existing 15s
// notification poll — so it costs nothing extra to keep current.
const LIVE_STRIP_CAP = 8; // agents shown before "+N more" — a glance, not a roster
function renderLiveNowStrip() {
  const el = $('liveNowStrip');
  if (!el || el.style.display === 'none') return;
  const fleet = fleetCache || [];
  const items = [];
  let total = 0;
  for (const s of fleet) {
    if (!s.liveAgentCount) continue;
    total += s.liveAgentCount;
    for (const a of (s.liveAgents || [])) items.push({ ...a, sessionFile: s.file, sessionTitle: s.title || s.session.slice(0, 8) });
  }
  if (!total) { el.innerHTML = `<span class="lns-idle">○ Nothing running right now.</span>`; return; }
  const shown = items.slice(0, LIVE_STRIP_CAP);
  const overflow = total - shown.length;
  el.innerHTML = `<span class="lns-label">● ${total} agent${total === 1 ? '' : 's'} working now</span>` +
    shown.map(a => {
      const tip = `${a.sessionTitle}${a.model ? ' · ' + a.model : ''}`;
      return `<span class="lns-item" data-file="${esc(a.sessionFile)}" title="${esc(tip)}">
        <b>${esc(a.name)}</b>${a.model ? ` · ${esc(modelShortName(a.model))}` : ''}${a.tool ? ` · running <i>${esc(a.tool)}</i>` : ' · thinking'}
      </span>`;
    }).join('') +
    (overflow > 0 ? `<span class="lns-more">+${overflow} more</span>` : '');
  el.querySelectorAll('.lns-item[data-file]').forEach(x => x.onclick = () => openSession(x.dataset.file));
}

async function loadFleet() {
  if (!fleetCache) $('fleet').innerHTML = '<div class="fleet-loading">Scanning sessions…</div>';
  fleetCache = await (await fetch('/api/fleet')).json();
  renderFleet();
  renderLiveNowStrip();
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
      // machine · project, with the project half dropped when it IS the machine
      // (a relay too old to name projects) so the line never says it twice. The
      // inline strip this used to do ate the leading "C" of a Windows slug —
      // cleanProjLabel() is the one place that gets that order right.
      const proj = cleanProjLabel(s.project, s.machine);
      return `
      <div class="fcard${m.archived ? ' is-archived' : ''}" data-file="${esc(s.file)}" data-sk="${esc(s.stableKey || '')}" draggable="true" style="border-left:3px solid ${col}">
        ${s.stableKey ? `<div class="fcard-actions"><button class="fcard-arch" title="${m.archived ? 'unarchive' : 'archive'}">${m.archived ? '⤴' : '🗄'}</button><button class="fcard-menu" title="organize">⋯</button></div>` : ''}
        <h3>${esc(s.title || s.session.slice(0, 8))}</h3>
        <div class="fproj"${s.projPath ? ` title="${esc(s.projPath)}"` : ''}><span class="kind-badge" style="background:${col}22;color:${col}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span> <span${machineTitle(s.machine) ? ` title="${esc(machineTitle(s.machine))}"` : ''}>${esc(machineLabel(s.machine))}</span>${proj && proj !== s.machine ? ' · ' + esc(proj) : ''}</div>
        ${cardBadges(s) ? `<div class="fbadges">${cardBadges(s)}</div>` : ''}
        ${modelChips(s)}
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
    <select id="machineSel"><option value="all">all machines</option>${machinesInFleet.map(m => `<option value="${esc(m)}" ${fleetMachine === m ? 'selected' : ''}>${esc(machineLabel(m))}</option>`).join('')}</select>
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
  renderLiveNowStrip();
}
function renderTable() {
  const cols = [
    { k: 'title', label: 'Session', num: false },
    { k: 'kind', label: 'Agent', num: false },
    { k: 'machine', label: 'Machine', num: false },
    { k: 'models', label: 'Models', num: false, sortKey: 'topTierShare', dir0: -1 },
    { k: 'topTierShare', label: 'Top-tier %', num: true },
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
  const sortField = (cols.find(c => c.k === col) || {}).sortKey || col;
  rows.sort((a, b) => {
    let av = a[sortField], bv = b[sortField];
    // null topTierShare ("can't tell", no cost) sorts as lowest rather than
    // breaking the numeric comparison or silently reading as 0%.
    if (sortField === 'topTierShare') { av = av == null ? -1 : av; bv = bv == null ? -1 : bv; }
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
      // A confident "100%" next to a cost of $0.000 is noise dressed as a finding.
      // Below a cent of total spend there is nothing worth reporting a share of.
      const spend = s.tierMix ? TIERS.reduce((a, t) => a + (s.tierMix[t] || 0), 0) : 0;
      const tts = spend < 0.01 ? null : s.topTierShare;
      return `<tr data-file="${esc(s.file)}"${m.archived ? ' class="row-archived"' : ''}>
        <td class="tsess" data-sk="${esc(s.stableKey || '')}" title="${s.renamed ? 'renamed — was: ' + esc(s.autoTitle || '') : 'double-click to rename'}"><span class="tsess-t">${esc(s.title || s.session.slice(0, 8))}</span>${s.stableKey ? '<button class="row-rename" title="rename">✎</button>' : ''}</td>
        <td><span class="kind-badge" style="background:${c}22;color:${c}">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label}</span></td>
        <td${machineTitle(s.machine) ? ` title="${esc(machineTitle(s.machine))}"` : ''}>${esc(machineLabel(s.machine))}</td>
        <td class="tmodels">${modelChips(s, { max: 3 }) || '<span class="dim">—</span>'}</td>
        <td class="num" title="${esc(tierMixTooltip(s))}">${tts == null ? '—' : Math.round(tts * 100) + '%'}</td>
        <td class="num">${s.agents}</td><td class="num">${s.events}</td><td class="num">${s.toolCalls}</td>
        <td class="num">${fmtDur(s.durationMs)}</td><td class="num">${fmtTok(s.tokensOut)}</td>
        <td class="num fcost">~${fmtUsd(s.cost)}</td><td class="num ${s.errors ? 'ferr' : ''}">${s.errors || ''}</td>
        <td class="num tdate"><span class="ago ${agoClass(s.mtime)}">${fmtAgo(s.mtime)}</span></td>
        <td class="tact">${s.stableKey ? `<button class="row-arch" data-sk="${esc(s.stableKey)}" title="${m.archived ? 'unarchive' : 'archive'}">${m.archived ? '⤴' : '🗄'}</button><button class="row-menu" title="organize">⋯</button>` : ''}</td>
      </tr>`;
    }).join('') + `</tbody></table></div>`;
  $('tableView').querySelectorAll('th[data-k]').forEach(th => { th.onclick = () => { const k = th.dataset.k; const c = cols.find(c => c.k === k); tableSort = { col: k, dir: tableSort.col === k ? -tableSort.dir : (c.dir0 || (c.num ? -1 : 1)) }; renderTable(); }; });
  $('tableView').querySelectorAll('tr[data-file]').forEach(tr => {
    const s = rows.find(x => x.file === tr.dataset.file);
    tr.onclick = () => openSession(tr.dataset.file);
    const arch = tr.querySelector('.row-arch');
    if (arch && s) arch.onclick = e => { e.stopPropagation(); setSessionMeta(s.stableKey, { archived: !metaOf(s).archived }); };
    const menu = tr.querySelector('.row-menu');
    if (menu && s) menu.onclick = e => showCardMenu(e, s);
    const cell = tr.querySelector('td.tsess');
    if (cell && s && s.stableKey) {
      const start = e => { e.stopPropagation(); beginRename(cell, s, renderTable); };
      cell.ondblclick = start;
      const pen = cell.querySelector('.row-rename');
      if (pen) pen.onclick = start;
    }
  });
  wireFleetControls(renderTable, $('tableView'));
}

// Rename a session in place. A session title is just whatever its first prompt
// happened to say, which is often a paragraph of pasted context — so being able to
// call it something short and true matters more here than in most tools.
// Empty input clears the override and the original title comes back.
function beginRename(cell, s, redraw) {
  if (cell.querySelector('input')) return;
  const current = s.renamed ? s.title : (s.autoTitle || s.title || '');
  const input = document.createElement('input');
  input.className = 'rename-input';
  input.value = s.renamed ? s.title : '';
  input.placeholder = (s.autoTitle || s.title || 'name this session').slice(0, 60);
  input.maxLength = 120;
  cell.innerHTML = '';
  cell.appendChild(input);
  input.focus(); input.select();
  let settled = false;
  const finish = async save => {
    if (settled) return; settled = true;
    const val = input.value.trim();
    if (save && val !== (s.renamed ? s.title : '')) {
      // metaPost already reports its own failures and refreshes the overview.
      await setSessionMeta(s.stableKey, { name: val || null });
      fleetCache = await (await fetch('/api/fleet')).json();
    }
    redraw();
  };
  input.onkeydown = e => { e.stopPropagation(); if (e.key === 'Enter') finish(true); else if (e.key === 'Escape') finish(false); };
  input.onblur = () => finish(true);
  input.onclick = e => e.stopPropagation();
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
            <div class="pchip-m">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label} · ${esc(machineLabel(s.machine))} · ~${fmtUsd(s.cost)}</div>
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
  const totAll = { cost: 0, tokensIn: 0, tokensCache: 0, tokensCacheWrite: 0, tokensOut: 0, agents: 0, sessions: data.length };
  for (const s of data) { totAll.cost += s.cost || 0; totAll.tokensIn += s.tokensIn || 0; totAll.tokensCache += s.tokensCache || 0; totAll.tokensCacheWrite += s.tokensCacheWrite || 0; totAll.tokensOut += s.tokensOut || 0; totAll.agents += s.agents || 0; }
  const range = data.length ? `${new Date(Math.min(...data.map(s => s.mtime))).toLocaleDateString()} – ${new Date(Math.max(...data.map(s => s.mtime))).toLocaleDateString()}` : '—';

  // summary tiles
  const tiles = `<div class="usage-tiles">
    <div class="utile"><div class="ul">Total est. cost</div><div class="uv accent">~${fmtUsd(totAll.cost)}</div></div>
    <div class="utile"><div class="ul">Fresh in / out</div><div class="uv">${fmtTok(totAll.tokensIn)} / ${fmtTok(totAll.tokensOut)}</div></div>
    <div class="utile"><div class="ul">Cache reads <span title="cached prefix re-read each turn, billed at 0.1×">ⓘ</span></div><div class="uv small" style="font-size:16px">${fmtTok(totAll.tokensCache)}</div></div>
    <div class="utile"><div class="ul">Cache writes <span title="context stored for the next turn, billed at 1.25× — the expensive half of hauling context around">ⓘ</span></div><div class="uv small" style="font-size:16px">${fmtTok(totAll.tokensCacheWrite)}</div></div>
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
          // A rate needs enough runs behind it to mean anything. One clean run is
          // not '100% success' — it is 'no idea yet', and colouring it green is a
          // claim the data cannot support. Every other rate here is gated; this
          // one was not.
          const enough = r.n >= ROLE_MIN_RUNS;
          const success = Math.round(r.clean / r.n * 100);
          const sCls = !enough ? 'dim' : success >= 80 ? 'ok' : success >= 50 ? 'warn' : 'bad';
          const sTxt = enough ? success + '%' : r.clean + '/' + r.n;
          return `<div class="rt-row" data-file="${esc(r.example?.file || '')}" title="Click to open the most recent session that used ${esc(name)}">
            <span class="rt-name"><span class="rt-bar" style="width:${Math.max(6, r.n / maxN * 100)}%"></span><b>${esc(name)}</b></span>
            <span>×${r.n}</span>
            <span class="rt-s ${sCls}" title="${enough ? '' : 'only ' + r.n + ' run' + (r.n === 1 ? '' : 's') + ' so far — not enough to call a success rate'}">${sTxt}</span>
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
  renderLiveNowStrip();
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
  return { buckets, errB, kind: s.kind, cost: s.cost, dur: s.durationMs, title: s.title || s.session.slice(0, 8), file: s.file, errors: s.errors, tier: dominantTier(s) };
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
  // Frame colour = the session's dominant model tier (see dominantTier() up top)
  // so a wall of pricey runs is visible before you read a single number.
  const tierCol = g.tier ? TIER_COLOR[g.tier] : 'var(--line)';
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
    <rect x="0.75" y="0.75" width="${w - 1.5}" height="${h - 1.5}" rx="3" fill="var(--panel2)" stroke="${tierCol}" stroke-width="1.5"/>
    ${bars}
    <line x1="${(w - tickLen).toFixed(1)}" y1="${(h - 0.75).toFixed(1)}" x2="${w.toFixed(1)}" y2="${(h - 0.75).toFixed(1)}" stroke="${col}" stroke-width="1.5" opacity=".85"/>
    <title>${esc(g.title)}&#10;~${fmtUsd(g.cost)} · ${fmtDur(g.dur)}${g.errors ? ` · ${g.errors} error${g.errors === 1 ? '' : 's'}` : ''}${g.tier ? ` · ${TIER_LABEL[g.tier]} tier` : ''}</title>
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
    <div class="rings-legend">One tile is one session, newest first. Taller bars = a busier stretch of that run, <span style="color:var(--red)">red</span> bars = a stretch that hit an error, and the tick along the bottom shows how long it ran. The tile's <b>border</b> is coloured by its dominant model tier — see the legend below. You are looking for the odd one out — hover any tile for its name, cost, and tier.</div>
    <div class="usage-legend">Border colour = priciest model tier this session leaned on: ${TIERS.map(t => `<span style="color:${TIER_COLOR[t]}">■ ${TIER_LABEL[t]}</span>`).join('')}</div>` +
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
const CAL_METRIC_COLOR = { sessions: '#5eead4', cost: '#818cf8', agents: '#60a5fa', errors: '#f87171', topTier: '#f87171' };
const CAL_METRIC_LABEL = { sessions: 'Sessions', cost: 'Cost', errors: 'Errors', agents: 'Agents', topTier: 'Top-tier $' };

async function loadCalendar() {
  // refetch every time: any of these can be the home screen, and a home screen
  // that never updates is worse than no home screen at all
  try { fleetCache = await (await fetch('/api/fleet')).json(); } catch { fleetCache = fleetCache || []; }
  renderCalendar();
  renderLiveNowStrip();
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
  if (metric === 'topTier') return rec.topTier;
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
        <span class="cdi-m">${(AGENT_KIND[s.kind] || AGENT_KIND.claude).label} · ${esc(machineLabel(s.machine))} · ~${fmtUsd(s.cost)}${s.errors ? ` · ${s.errors} err` : ''}</span>
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
    if (!days.has(k)) days.set(k, { sessions: 0, cost: 0, errors: 0, agents: 0, topTier: 0, date: new Date(s.mtime) });
    const d = days.get(k);
    d.sessions++; d.cost += s.cost || 0; d.errors += s.errors || 0; d.agents += s.agents || 0;
    d.topTier += (s.tierMix && (s.tierMix.flagship + s.tierMix.premium)) || 0;
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
    const tip = c.rec ? `${dStr}: ${c.rec.sessions} session${c.rec.sessions === 1 ? '' : 's'}, ~${fmtUsd(c.rec.cost)}${c.rec.errors ? `, ${c.rec.errors} err` : ''}${calMetric === 'topTier' ? `, ~${fmtUsd(c.rec.topTier)} on flagship/premium` : ''}` : `${dStr}: no sessions`;
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

// ---------- shared time-bucket coloring for RINGS + RHYTHM ----------
// Both views slice sessions into small time windows (a week, an hour) and paint
// each window green/amber/red by a RATE — never `some(...)`, and never below a
// minimum sample. Two selectable metrics share this: 'trouble' (share of a
// window's runs that hit an error/retry/stall — the original, unchanged
// thresholds) and 'topTier' (share of a window's DOLLARS spent on flagship/
// premium models). topTier is gated on the window's total $, not just session
// count — a window can be full of sessions and still have zero dollars billed,
// which is "can't tell", not "cheap".
// There are TWO reasons a bucket can't be judged, and conflating them told the
// owner his sample was too small when it was actually fine but had no cost on it.
function bmUnjudged(bm) {
  return bm.rate === null ? 'no cost recorded, so this cannot be judged' : 'too few runs to judge';
}
function bucketMetric(list, metric, minSample) {
  const n = list.length;
  if (metric === 'topTier') {
    // Sum the SAME numbers on both sides. s.cost is rounded to 2dp while tierMix
    // carries 4dp, so dividing one by the other let a share exceed 100% — which
    // then drew a negative-height bar on the weekday strip.
    const tierTotal = s => (s.tierMix ? TIERS.reduce((a, t) => a + (s.tierMix[t] || 0), 0) : 0);
    const total = list.reduce((a, s) => a + tierTotal(s), 0);
    const top = list.reduce((a, s) => a + ((s.tierMix && (s.tierMix.flagship + s.tierMix.premium)) || 0), 0);
    const rate = total > 0 ? Math.min(100, Math.round(top / total * 100)) : null;
    return {
      n, rate, judged: n >= minSample && rate !== null, thresh: [66, 33],
      note: total > 0 ? `~${fmtUsd(top)} of ~${fmtUsd(total)} on flagship/premium` : (n ? 'no cost recorded' : ''),
    };
  }
  const rough = list.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
  const rate = n ? Math.round(rough / n * 100) : 0;
  return { n, rate, judged: n >= minSample, thresh: [30, 10], note: n ? `${rough} of ${n} hit trouble` : '' };
}
function bucketColor(bm) {
  if (!bm.n) return { color: 'var(--line)', op: .35 };
  if (!bm.judged) return { color: 'var(--dim)', op: .55 };
  const [hi, lo] = bm.thresh;
  return { color: bm.rate > hi ? 'var(--red)' : bm.rate >= lo ? 'var(--amber)' : 'var(--green)', op: .92 };
}
const BUCKET_METRICS = [['trouble', 'Trouble rate'], ['topTier', 'Top-tier spend']];

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
  renderLiveNowStrip();
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
// A relayed project now arrives as "⇄ <machine> · <folder>". Every caller here
// prints the machine separately, so strip that half when we know it rather than
// render "trifecta-erp · trifecta-erp · BukkakERP". Only the PREFIX is stripped:
// a label that is nothing BUT the machine means the relay never named a project
// (an older build), and the caller needs to be able to see that.
function cleanProjLabel(raw, machine) {
  const s = String(raw || '');
  const win = s.replace(/^[Cc]--Users-[^-]+-/, '');
  let out = (win !== s ? win : s.replace(/^(⇄|Codex)\s*·?\s*/, ''));
  if (machine && out.startsWith(machine + ' · ')) out = out.slice(machine.length + 3);
  return out || 'Unknown';
}
function ringDiscSvg(sessions, metric) {
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
    // Same rule as the Rhythm clock: colour by the SHARE that qualifies, never by
    // "did any of them", which turns one bad (or one pricey) run in seventeen
    // into a red year. See bucketMetric()/bucketColor() above.
    const bm = bucketMetric(wk, metric, RING_MIN_SAMPLE);
    const { color, op } = bucketColor(bm);
    const sw = n > 0 ? 2 + Math.round((n / maxN) * 6) : 2;
    const weekStr = new Date(anchor - (span - 1 - i) * RING_WEEK_MS).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
    const tip = n
      ? `Week of ${weekStr}: ${n} session${n === 1 ? '' : 's'}` + (bm.judged ? ` · ${bm.note} (${bm.rate}%)` : bm.note ? ` (${bm.note}, ${bmUnjudged(bm)})` : ' (too few to judge)')
      : `Week of ${weekStr}: no runs`;
    rings += `<circle cx="0" cy="0" r="${r}" fill="none" stroke="${color}" stroke-width="${sw}" opacity="${op}"><title>${esc(tip)}</title></circle>`;
  });
  return `<svg viewBox="${-R - 4} ${-R - 4} ${(R + 4) * 2} ${(R + 4) * 2}" width="${(R + 4) * 2}" height="${(R + 4) * 2}" class="ring-svg">
    <circle cx="0" cy="0" r="${RING_BASE_R - 5}" fill="var(--panel2)" stroke="var(--line)"/>
    ${rings}
  </svg>`;
}
let ringMetric = 'trouble';
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

  const ringLegend = ringMetric === 'topTier'
    ? `Each ring is a week — colour is the <b>share of that week's dollars</b> spent on flagship/premium models: <span style="color:var(--green)">green</span> under 33%, <span style="color:var(--amber)">amber</span> 33–66%, <span style="color:var(--red)">red</span> over 66%, <span style="color:var(--dim)">grey</span> too few runs or no cost recorded that week to judge.`
    : `Each ring is a week — colour is the <b>share</b> of that week's runs that hit trouble: <span style="color:var(--green)">green</span> under 10%, <span style="color:var(--amber)">amber</span> 10–30%, <span style="color:var(--red)">red</span> over 30%, <span style="color:var(--dim)">grey</span> too few runs that week to judge.`;
  $('rings').innerHTML =
    `<div class="fleet-head"><h2>Projects — rings — ${discs.length} project${discs.length === 1 ? '' : 's'}</h2>
      <div class="seg" id="ringMetricSeg">${BUCKET_METRICS.map(([v, l]) => `<button data-m="${v}" class="${ringMetric === v ? 'on' : ''}">${l}</button>`).join('')}</div>
      ${homeButton('rings')}</div>
    <div class="rings-legend">${ringLegend} Thicker ring = busier week. Only the last ${RING_MAX_WEEKS} weeks are drawn: centre = ${RING_MAX_WEEKS} weeks ago, edge = most recent. Hover any ring for the real numbers; click a disc to see it in Fleet.</div>` +
    (discs.length === 0
      ? `<div class="fp-empty">No sessions yet — once you run something, its project gets a ring disc here.</div>`
      : `<div class="rings-grid">` + discs.map(d => {
        // A relayed session's "project" used to be its MACHINE name, so every
        // remote repo collapsed into one disc. Relays now send the real project,
        // so a disc is a project again — with the machine kept in front of it so
        // two machines' "web" never blur together. A relay too old to name its
        // projects still lands on the machine name, and the caption says so
        // plainly rather than letting one disc read as a single repo.
        const remote = d.sessions.every(s => /^(relay|otel|archive):/.test(s.file || ''));
        const mach = d.sessions.every(s => String(s.file || '').startsWith('relay:')) ? (d.sessions[0].machine || '') : '';
        const proj = cleanProjLabel(d.proj, mach);
        const named = proj !== mach;
        const label = (remote ? '🖥 ' : '') + (mach && named ? machineLabel(mach) + ' · ' : '') + proj
          + (remote && !named ? ' — all remote work (this relay is too old to name projects)' : '');
        const where = (d.sessions.find(s => s.projPath) || {}).projPath || '';
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
        return `<div class="ring-card" data-proj="${esc(d.proj)}" title="${esc((where ? where + ' — ' : '') + 'click to see ' + label + ' in Fleet')}">
          ${ringDiscSvg(d.sessions, ringMetric)}
          <div class="ring-name">${esc(label)}</div>
          <div class="ring-summary">${esc(summary)}</div>
        </div>`;
      }).join('') + `</div>`);
  wireHomeButton($('rings'), 'rings', renderRings);
  $('rings').querySelector('#ringMetricSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => { ringMetric = b.dataset.m; renderRings(); });
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
  renderLiveNowStrip();
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
let rhyMetric = 'trouble';
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
    // Colour from the SHARE that qualifies, never `some(...)`. A boolean OR over an
    // all-time bucket is monotone in sample size: the hours you use most are the
    // ones guaranteed to contain one bad (or one pricey) run eventually, so they'd
    // go red and stay red forever — telling you the exact opposite of the truth.
    const bm = bucketMetric(list, rhyMetric, RHY_MIN_SAMPLE);
    const { color, op } = bucketColor(bm);
    const hourLabel = h === 0 ? '12am' : h < 12 ? `${h}am` : h === 12 ? '12pm' : `${h - 12}pm`;
    const tip = n
      ? `${hourLabel}: ${n} session${n === 1 ? '' : 's'}` + (bm.judged ? ` · ${bm.note} (${bm.rate}%)` : bm.note ? ` (${bm.note}, ${bmUnjudged(bm)})` : ' (too few to judge)')
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
    const h = (n / maxWd) * WD_H;
    const x = i * (WD_W + WD_GAP);
    let overlay = '', tip;
    if (rhyMetric === 'topTier') {
      // Overlay height = share of the day's DOLLARS on flagship/premium; colour
      // follows the same red/amber/green thresholds as the clock and Rings.
      const bm = bucketMetric(list, 'topTier', RHY_MIN_SAMPLE);
      tip = `${RHY_WEEKDAYS[i]}: ${n} session${n === 1 ? '' : 's'}` + (bm.judged ? ` · ${bm.note} (${bm.rate}%)` : bm.note ? ` (${bm.note}, ${bmUnjudged(bm)})` : n ? ' (too few to judge)' : '');
      if (bm.judged) {
        const { color } = bucketColor(bm);
        const oh = h * (bm.rate / 100);
        overlay = `<rect x="${x}" y="${(WD_H - oh).toFixed(1)}" width="${WD_W}" height="${oh.toFixed(1)}" rx="3" fill="${color}" opacity=".8"><title>${esc(tip)}</title></rect>`;
      }
    } else {
      const rough = list.filter(s => s.errors > 0 || s.retrying || s.stalled).length;
      const roughH = n ? (rough / n) * h : 0;
      // The red overlay is a rate too, so it obeys the same small-sample gate as
      // the clock: below the threshold the bar is plain volume and the tooltip says why.
      const judged = n >= RHY_MIN_SAMPLE;
      tip = `${RHY_WEEKDAYS[i]}: ${n} session${n === 1 ? '' : 's'}` + (judged ? `${rough ? `, ${rough} of ${n} hit trouble` : ', all clean'}` : n ? ' (too few to judge)' : '');
      if (rough && judged) overlay = `<rect x="${x}" y="${(WD_H - h).toFixed(1)}" width="${WD_W}" height="${roughH.toFixed(1)}" rx="3" fill="var(--red)" opacity=".8"><title>${esc(tip)}</title></rect>`;
    }
    wdBars += `<g>
      <rect x="${x}" y="${(WD_H - h).toFixed(1)}" width="${WD_W}" height="${h.toFixed(1)}" rx="3" fill="var(--accent2)" opacity=".55"><title>${esc(tip)}</title></rect>
      ${overlay}
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

  const rhyLegend = rhyMetric === 'topTier'
    ? `Spoke length = sessions started that hour, all-time. Colour = <b>share of that hour's dollars</b> spent on flagship/premium models — <span style="color:var(--green)">green</span> under 33%, <span style="color:var(--amber)">amber</span> 33–66%, <span style="color:var(--red)">red</span> over 66%, <span style="color:var(--dim)">grey</span> too few runs or no cost recorded that hour to judge.`
    : `Spoke length = sessions started that hour, all-time. Colour = how they went — <span style="color:var(--green)">green</span> calm, <span style="color:var(--amber)">amber</span> a retry happened, <span style="color:var(--red)">red</span> hit errors, <span style="color:var(--dim)">grey</span> too few runs that hour to judge.`;
  $('rhythm').innerHTML =
    `<div class="fleet-head"><h2>Rhythm — when you run, and how it goes</h2>
      <div class="seg" id="rhyMetricSeg">${BUCKET_METRICS.map(([v, l]) => `<button data-m="${v}" class="${rhyMetric === v ? 'on' : ''}">${l}</button>`).join('')}</div>
      ${homeButton('rhythm')}</div>
    <div class="rings-legend">${rhyLegend}</div>
    <div class="rhy-wrap">
      <div class="rhy-col">${clockSvg}</div>
      <div class="rhy-col">
        <h3 class="rhy-h3">By day of week</h3>
        ${wdSvg}
        ${chrono}
      </div>
    </div>`;
  wireHomeButton($('rhythm'), 'rhythm', renderRhythm);
  $('rhythm').querySelector('#rhyMetricSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => { rhyMetric = b.dataset.m; renderRhythm(); });
}

// ---------- UNSAVED WORK (what your agents made that isn't saved anywhere) ----------
// WHY: every other view asks how a run went. This asks the only thing that outlives
// the run — what did my agents make that isn't safely saved anywhere? A file listed
// here is untracked AND has never appeared in a commit on any branch, so the copy on
// this computer is the only copy in existence.
// LIST AND COPY ONLY. There is no save button, no stage button, no delete — this
// dashboard watches, it never changes the owner's files, and the header says so.
let unsavedData = null;
async function loadUnsaved() {
  $('unsaved').innerHTML = '<div class="fleet-loading">Checking your recent sessions for work that was never saved… (this one takes a moment)</div>';
  try { unsavedData = await (await fetch('/api/unsaved')).json(); }
  catch { unsavedData = { error: 'Could not check for unsaved work just now.' }; }
  renderUnsaved();
}
// fmtBytes rounds to the nearest KB, which prints a small note as "0KB" — say the
// real number instead, because a tiny file is exactly the kind that gets forgotten.
const uwSize = n => (n < 1000 ? `${n || 0} bytes` : fmtBytes(n));
// Show the path the way the owner thinks of it: relative to the project folder.
function uwRel(p, root) {
  const inside = root && p.length > root.length
    && (p[root.length] === '\\' || p[root.length] === '/')
    && p.slice(0, root.length).toLowerCase() === root.toLowerCase();
  return (inside ? p.slice(root.length + 1) : p).replace(/\\/g, '/');
}
function renderUnsaved() {
  const d = unsavedData || {};
  const files = d.files || [];
  // Every reason the list could be incomplete, said out loud. A list of things you
  // might lose is only trustworthy if it admits what it didn't look at.
  const older = (d.totalSessions || 0) - (d.scannedSessions || 0);
  const limits = [
    d.capped ? `${older} older session${older === 1 ? '' : 's'} weren’t checked` : null,
    d.pathsCapped ? 'those sessions wrote more files than one pass checks' : null,
    d.historyCapped ? 'more files were unsaved than one pass could confirm' : null,
  ].filter(Boolean);
  const scanNote = d.error ? esc(d.error)
    : d.gitMissing ? 'git isn’t installed on this computer, so there is no way to tell what has been saved and what hasn’t.'
      : `Looked through ${d.scannedSessions === 1 ? 'the most recent session' : `the ${d.scannedSessions} most recent sessions`} on this computer${limits.length ? ` — ${esc(limits.join('; '))}, so there may be more` : ''}.`;

  const table = `<div class="table-wrap"><table class="ftable uw-table"><thead><tr>
      <th>File</th><th>Project</th><th class="num">Last written</th><th class="num">Size</th><th>Made during</th><th class="tact"></th>
    </tr></thead><tbody>` +
    files.map((f, i) => {
      const name = f.path.split(/[\\/]/).pop();
      const rel = uwRel(f.path, f.repoRoot);
      return `<tr>
      <td class="tsess" title="${esc(f.path)}"><div class="uw-name">${esc(name)}</div>${rel === name ? '' : `<div class="uw-sub">${esc(rel)}</div>`}</td>
      <td class="uw-repo" title="${esc(f.repoRoot || '')}">${esc((f.repoRoot || '').split(/[\\/]/).pop())}</td>
      <td class="num tdate"><span class="ago ${agoClass(f.mtime)}">${fmtAgo(f.mtime)}</span></td>
      <td class="num">${esc(uwSize(f.sizeBytes))}</td>
      <td class="uw-sess" data-file="${esc(f.sessionFile)}" title="open this session">${esc(f.sessionTitle || 'a session')}</td>
      <td class="tact"><button class="mini-btn uw-copy" data-i="${i}">📋 copy path</button></td>
    </tr>`;
    }).join('') + '</tbody></table></div>';

  const empty = `<div class="uw-empty">Nothing is sitting unsaved. Every file your recent agents made is either already saved into a project’s history, or lives in a folder your project was told to ignore.</div>`;

  $('unsaved').innerHTML =
    `<div class="fleet-head"><h2>Unsaved work — ${files.length} file${files.length === 1 ? '' : 's'} only this computer is holding</h2>
      <span class="dim">${scanNote}</span>${homeButton('unsaved')}</div>
     <div class="uw-note">“Not saved anywhere” means your project’s history has never seen this file — it isn’t saved into it, and it isn’t sitting on another branch either. If this computer’s disk went away, so would the file.
       <b>This is a list and nothing more.</b> Agent Mission Control will never save, move, change, or delete these files for you — copy a path and handle it yourself.</div>
     ${d.error || d.gitMissing ? '' : files.length ? table : empty}`;

  wireHomeButton($('unsaved'), 'unsaved', renderUnsaved);
  $('unsaved').querySelectorAll('.uw-copy').forEach(b => b.onclick = () => {
    const f = files[+b.dataset.i]; if (!f) return;
    navigator.clipboard.writeText(f.path);
    const o = b.textContent; b.textContent = '✓ copied'; setTimeout(() => { b.textContent = o; }, 1500);
  });
  $('unsaved').querySelectorAll('.uw-sess[data-file]').forEach(el => el.onclick = () => openSession(el.dataset.file));
}

// ---------- TROUBLE FILES (which files agents keep fighting with) ----------
// WHY: every other view asks how a RUN went. This asks which FILE keeps costing
// time, aggregated across the recent fleet — the file a session touched, not the
// session itself. THE RATE IS THE WHOLE POINT AND THE WHOLE RISK: a file touched
// twice where one session went badly is a coin flip, not a 50% problem file, so
// the server already refuses to compute a rate below a minimum sample — this
// only ever prints the percentage the server sent, never derives its own.
let troubleData = null;
let troubleSort = { col: 'sessions', dir: -1 };
async function loadTrouble() {
  $('trouble').innerHTML = '<div class="fleet-loading">Looking through your recent sessions for files that keep causing trouble…</div>';
  try { troubleData = await (await fetch('/api/trouble-files')).json(); }
  catch { troubleData = { files: [], error: 'Could not check for trouble files just now.' }; }
  renderTrouble();
}
// Same green/amber/red/grey scale as Rings and Rhythm: grey below the sample
// gate (server sends rate: null), then green under 10%, amber 10–30, red over.
function tfRateColor(rate) {
  return rate === null ? 'var(--dim)' : rate > 30 ? 'var(--red)' : rate >= 10 ? 'var(--amber)' : 'var(--green)';
}
function renderTrouble() {
  const d = troubleData || {};
  const files = (d.files || []).slice();
  const cols = [
    { k: 'path', label: 'File', num: false },
    { k: 'sessions', label: 'Sessions', num: true },
    { k: 'badSessions', label: 'Went badly', num: true },
    { k: 'rate', label: 'Rate', num: true },
    { k: 'lastTouched', label: 'Last touched', num: true },
  ];
  const { col, dir } = troubleSort;
  files.sort((a, b) => {
    const av = col === 'rate' ? (a.rate ?? -1) : col === 'path' ? a.path : a[col];
    const bv = col === 'rate' ? (b.rate ?? -1) : col === 'path' ? b.path : b[col];
    if (typeof av === 'number') return (av - bv) * dir;
    return String(av || '').localeCompare(String(bv || '')) * dir;
  });

  const older = (d.totalSessions || 0) - (d.scannedSessions || 0);
  const scanNote = d.error ? esc(d.error)
    : `Looked through ${d.scannedSessions === 1 ? 'the most recent session' : `the ${d.scannedSessions} most recent sessions`} on this computer${d.capped ? ` — ${older} older session${older === 1 ? '' : 's'} weren’t checked, so there may be more` : ''}.`;

  const table = `<div class="table-wrap"><table class="ftable"><thead><tr>` +
    cols.map(c => `<th data-k="${c.k}" class="${c.num ? 'num' : ''} ${col === c.k ? 'sorted' : ''}">${c.label}${col === c.k ? (dir < 0 ? ' ▼' : ' ▲') : ''}</th>`).join('') +
    `</tr></thead><tbody>` +
    files.map((f, i) => {
      const name = f.path.split(/[\\/]/).pop();
      const dirPart = f.path.slice(0, f.path.length - name.length).replace(/[\\/]$/, '');
      return `<tr data-i="${i}" title="${esc(f.path)} — click to see which sessions touched it">
        <td class="tsess"><div class="uw-name">${esc(name)}</div>${dirPart ? `<div class="uw-sub">${esc(dirPart)}</div>` : ''}</td>
        <td class="num">${f.sessions}</td>
        <td class="num ${(f.rate !== null && f.badSessions) ? 'ferr' : ''}">${f.badSessions}</td>
        <td class="num" style="color:${tfRateColor(f.rate)}">${f.rate === null ? 'not enough runs to say yet' : f.rate + '%'}</td>
        <td class="num tdate"><span class="ago ${agoClass(f.lastTouched)}">${fmtAgo(f.lastTouched)}</span></td>
      </tr>`;
    }).join('') + `</tbody></table></div>`;

  const empty = `<div class="uw-empty">No file has been written by more than one of your recent sessions — there's nothing yet to compare.</div>`;

  $('trouble').innerHTML =
    `<div class="fleet-head"><h2>Trouble files — ${files.length} file${files.length === 1 ? '' : 's'} your agents keep touching</h2>
      <span class="dim">${scanNote}</span>${homeButton('trouble')}</div>
     <div class="uw-note">A file's <b>rate</b> is the share of sessions that touched it and went badly (an error, a retry, or a stall) — out of the sessions that touched THAT file, never out of every session ever run. It only shows once a file has been touched at least ${d.minSessions || 5} times; touched less often than that, a file is listed by how much it's used with no rate attached, because too small a sample makes one unlucky run look like a permanently broken file.
       <b>Click a row</b> to see exactly which sessions touched that file.</div>
     ${d.error ? '' : files.length ? table : empty}`;

  wireHomeButton($('trouble'), 'trouble', renderTrouble);
  $('trouble').querySelectorAll('th[data-k]').forEach(th => { th.onclick = () => { const k = th.dataset.k; troubleSort = { col: k, dir: troubleSort.col === k ? -troubleSort.dir : (cols.find(c => c.k === k).num ? -1 : 1) }; renderTrouble(); }; });
  $('trouble').querySelectorAll('tr[data-i]').forEach(tr => { tr.onclick = () => openTroubleDrilldown(files[+tr.dataset.i]); });
}
// Row click -> which sessions touched this file, newest first, each opening it.
function openTroubleDrilldown(f) {
  if (!f) return;
  const ov = document.createElement('div'); ov.className = 'pb-editor-ov'; ov.onclick = e => { if (e.target === ov) ov.remove(); };
  ov.innerHTML = `<div class="handle-modal">
    <div class="hm-head"><b>${esc(f.path.split(/[\\/]/).pop())}</b><button class="mini-btn" id="tfClose">✕</button></div>
    <div class="hm-body"><div class="dim" style="margin-bottom:10px;word-break:break-all">${esc(f.path)}</div>
      <div class="dim" style="margin-bottom:8px">${f.sessions} session${f.sessions === 1 ? '' : 's'} touched this file${f.badSessions ? ` — ${f.badSessions} of them went badly` : ' — all of them went cleanly'}.</div>
      ${(f.sessionRefs || []).map(s => `<div class="arch-item" data-file="${esc(s.file)}">
        <div class="ai-t">${s.bad ? '⚠ ' : ''}${esc(s.title || 'a session')}</div>
        <div class="ai-m dim">${fmtAgo(s.mtime)}${s.bad ? ' · went badly' : ' · went cleanly'}</div>
      </div>`).join('')}
    </div></div>`;
  document.body.appendChild(ov);
  ov.querySelector('#tfClose').onclick = () => ov.remove();
  ov.querySelectorAll('.arch-item[data-file]').forEach(el => el.onclick = () => { ov.remove(); openSession(el.dataset.file); });
}

// ---------- SECRET LEAK SENTINEL (did an agent write a real key into a file?) ----------
// WHY: every other view asks how the work went. This asks whether the work left a
// live credential lying in a file. It is deliberately a SMOKE ALARM, not an audit:
// the server only recognises a handful of shapes whose vendor prefix is
// unmistakable, so a quiet panel is the normal, expected result and the copy has to
// say that plainly rather than imply the whole disk was cleared.
// THE KEY IS NEVER SENT HERE. The server returns a kind, a line number and the
// first four characters; there is nothing in this data to leak on screen, and no
// button here reveals, copies or changes a secret — only the file path.
let leaksData = null;
async function loadLeaks() {
  $('leaks').innerHTML = '<div class="fleet-loading">Reading the files your recent agents wrote, looking for anything shaped like a real key…</div>';
  try { leaksData = await (await fetch('/api/leaks')).json(); }
  catch { leaksData = { findings: [], error: 'Could not check your agents’ files for keys just now.' }; }
  renderLeaks();
}
function renderLeaks() {
  const d = leaksData || {};
  const hits = d.findings || [];
  // Every reason the scan saw less than everything, said out loud. A quiet alarm is
  // only reassuring if it admits exactly how much it actually looked at.
  const older = (d.totalSessions || 0) - (d.scannedSessions || 0);
  const limits = [
    d.capped ? `${older} older session${older === 1 ? '' : 's'} weren’t checked` : null,
    d.filesCapped ? 'those sessions wrote more files than one pass reads' : null,
    d.budgetHit ? 'the scan stopped early once it had read enough' : null,
  ].filter(Boolean);
  const skips = [
    d.skippedNoise ? `${d.skippedNoise} in dependency, build or test-fixture folders` : null,
    d.skippedIgnored ? `${d.skippedIgnored} your project is told to ignore` : null,
    d.skippedUnchecked ? `${d.skippedUnchecked} in projects with more files than one pass can check` : null,
    d.skippedBig ? `${d.skippedBig} too big to be hand-written` : null,
    d.skippedBinary ? `${d.skippedBinary} that ${d.skippedBinary === 1 ? 'isn’t' : 'aren’t'} text` : null,
  ].filter(Boolean);
  const scanNote = d.error ? esc(d.error)
    : `Read ${d.filesRead === 1 ? 'the 1 file' : `all ${d.filesRead || 0} files`} written by ${d.scannedSessions === 1 ? 'your most recent session' : `your ${d.scannedSessions || 0} most recent sessions`}${limits.length ? ` — ${esc(limits.join('; '))}, so there may be more` : ''}.${skips.length ? ` Skipped ${esc(skips.join(', '))}.` : ''}`;

  const table = `<div class="table-wrap"><table class="ftable uw-table"><thead><tr>
      <th>File</th><th>Looks like</th><th class="num">Line</th><th>Starts with</th><th class="num">Last written</th><th>Written during</th><th class="tact"></th>
    </tr></thead><tbody>` +
    hits.map((f, i) => {
      const name = f.path.split(/[\\/]/).pop();
      const rel = uwRel(f.path, f.repoRoot);
      return `<tr>
      <td class="tsess" title="${esc(f.path)}"><div class="uw-name">${esc(name)}</div>${rel === name ? '' : `<div class="uw-sub">${esc(rel)}</div>`}</td>
      <td class="sl-kind">${esc(f.kind)}</td>
      <td class="num">${esc(String(f.line))}</td>
      <td><code class="sl-frag">${esc(f.fragment)}</code></td>
      <td class="num tdate"><span class="ago ${agoClass(f.mtime)}">${fmtAgo(f.mtime)}</span></td>
      <td class="uw-sess" data-file="${esc(f.sessionFile)}" title="open this session">${esc(f.sessionTitle || 'a session')}</td>
      <td class="tact"><button class="mini-btn uw-copy" data-i="${i}">📋 copy path</button></td>
    </tr>`;
    }).join('') + '</tbody></table></div>';

  const empty = `<div class="sl-clear">✓ Nothing that looks like a real key turned up.<div class="sl-clear-sub">None of the files your recent agents wrote contain one of the key shapes listed above. That is the normal, expected result.</div></div>`;

  $('leaks').innerHTML =
    `<div class="fleet-head"><h2>Secrets — ${hits.length ? `${hits.length} thing${hits.length === 1 ? '' : 's'} worth a look` : 'nothing worth a look'}</h2>
      <span class="dim">${scanNote}</span>${homeButton('leaks')}</div>
     <div class="uw-note">This is a <b>smoke alarm, not a security check.</b> It only recognises keys that announce themselves — Amazon Web Services, GitHub, Slack, Stripe live payment keys, Google API keys, private keys, and signed login tokens. Anything else that happens to look random is left alone on purpose, because a warning that is usually wrong is one you stop reading. <b>It will miss things.</b>
       <br>The key itself is never shown, saved, or sent anywhere — you get the file, the line, and the first four characters, so you can find it yourself. <b>Agent Mission Control will never change, remove, or rotate anything for you.</b></div>
     ${d.error ? '' : hits.length ? table : empty}`;

  wireHomeButton($('leaks'), 'leaks', renderLeaks);
  $('leaks').querySelectorAll('.uw-copy').forEach(b => b.onclick = () => {
    const f = hits[+b.dataset.i]; if (!f) return;
    navigator.clipboard.writeText(f.path); // the path, never the key
    const o = b.textContent; b.textContent = '✓ copied'; setTimeout(() => { b.textContent = o; }, 1500);
  });
  $('leaks').querySelectorAll('.uw-sess[data-file]').forEach(el => el.onclick = () => openSession(el.dataset.file));
}

// ---------- THE GRAVEYARD (work this fleet already threw away) ----------
// WHY: every other view asks how a run WENT. This asks what has already been tried
// in a folder and undone, so the next agent doesn't cheerfully rebuild it.
// v1 IS THE NARROW, PROVABLE VERSION. The server recognises four literal git
// commands and nothing else, and it sends that list back as `looksFor` — this panel
// prints the server's own list rather than a hand-written copy, so what's promised
// on screen and what's actually detected cannot drift apart. Fuzzy "the run started
// over and did something different" detection was cut on purpose: every long session
// changes approach, so it fires constantly and means nothing.
// NO RATES, EVER. Each row is one thing that happened once, with a link to it. The
// denominator would be "transcripts that happen to still be on this computer", which
// is not a fair sample of anything, so no share is computed from it.
// NOTHING HERE WRITES ANYTHING. v1 deliberately has no "add this to my guidance
// file" button — the list has to be proved right before anything acts on it.
let graveData = null;
async function loadGraveyard() {
  $('graveyard').innerHTML = '<div class="fleet-loading">Reading the commands your agents actually ran, looking for the moments they threw work away… (this one takes a moment)</div>';
  try { graveData = await (await fetch('/api/graveyard')).json(); }
  catch { graveData = { moments: [], error: 'Could not check for thrown-away work just now.' }; }
  renderGraveyard();
}
const GRAVE_ICON = { reset: '💣', revert: '↩️', checkout: '🗑', restore: '🗑' };
const graveBase = p => String(p || '').split(/[\\/]/).filter(Boolean).pop() || p;
const graveDate = ts => new Date(ts).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
function graveFilesHTML(m) {
  if (!m.files || !m.files.length) return '';
  // One command discarded two files both called sw.js. Bare file names would have
  // printed the same chip twice and looked like a duplicate, so a name that repeats
  // in this row keeps its parent folder — only where it's actually needed.
  const seen = {};
  for (const f of m.files) { const b = graveBase(f); seen[b] = (seen[b] || 0) + 1; }
  const label = f => {
    const segs = String(f).split(/[\\/]/).filter(Boolean);
    const b = segs[segs.length - 1];
    return seen[b] > 1 && segs.length > 1 ? segs.slice(-2).join('/') : b;
  };
  const chips = m.files.map(f => `<span class="gy-file" title="${esc(f)}">${esc(label(f))}</span>`).join('');
  return m.filesNamed
    ? `<div class="gy-files"><span class="dim">files thrown back:</span>${chips}</div>`
    : `<div class="gy-files"><span class="dim">the command named no files, so these are what this run had edited by then${m.filesCapped ? ' (last few)' : ''}:</span>${chips}</div>`;
}
function renderGraveyard() {
  const d = graveData || {};
  const moments = d.moments || [];
  // Everything the scan could not see, said out loud. A graveyard is only useful if
  // it admits which graves it wasn't allowed to walk past.
  const limits = [
    d.capped ? `${(d.candidates || 0) - (d.scannedSessions || 0)} more transcript${(d.candidates || 0) - (d.scannedSessions || 0) === 1 ? '' : 's'} kept here weren’t reached in one pass` : null,
    d.budgetHit ? 'the scan stopped early once it had read enough' : null,
    d.orphans ? `${d.orphans} archived session${d.orphans === 1 ? '' : 's'} arrived with their helper transcripts but not their main one, so there’s no moment to link to` : null,
  ].filter(Boolean);
  const scanNote = d.error ? esc(d.error)
    : `Read the real command text of ${d.scannedSessions === 1 ? 'the 1 transcript' : `all ${d.scannedSessions || 0} transcripts`} stored on this computer (${d.mbRead || 0}MB).${limits.length ? ` Not checked: ${esc(limits.join('; '))}.` : ''}`;

  // The detector's own list, straight from the server, so what this panel promises
  // and what it actually recognises can never drift apart. If the scan failed there
  // is no list to print, and claiming one would be describing a check that didn't run.
  const rules = d.looksFor || [];
  const looks = rules.length
    ? `This looks for <b>${rules.length === 4 ? 'four' : rules.length} exact commands</b> and nothing else:<ul class="gy-shapes">${rules.map(r => `<li><code>${esc(r.cmd)}</code> — ${esc(r.what)}</li>`).join('')}</ul>`
    : '';
  const note = `<div class="uw-note">${looks}
     Anything vaguer — a run that quietly changed approach — is <b>not</b> detected, on purpose: every long session changes approach, so guessing at it would flag nearly everything.
     <br><b>A row is not a verdict.</b> Throwing an edit away is often exactly the right call. This is the list of moments worth re-reading before an agent tries that ground again — click a row to land on the exact moment.
     ${d.summaryOnly ? `<br>Only transcripts stored on this computer can be checked: ${d.summaryOnly} session${d.summaryOnly === 1 ? '' : 's'} in your fleet arrived from another machine already summarised, and a summary keeps the sentence (“Discard local changes”), not the command.` : ''}
     <br><b>Nothing here changes anything.</b> There is no button that writes this into a guidance file — the list has to earn its trust first.</div>`;

  // one block per project folder, folders with the most moments first
  const byRepo = new Map();
  moments.forEach((m, i) => {
    const k = (m.repo || '').toLowerCase();
    if (!byRepo.has(k)) byRepo.set(k, { repo: m.repo, rows: [] });
    byRepo.get(k).rows.push({ m, i });
  });
  const repos = [...byRepo.values()].sort((a, b) => b.rows.length - a.rows.length);
  const body = repos.map(r => `<div class="gy-repo">
      <div class="gy-repo-h"><b>${esc(graveBase(r.repo) || 'folder not recorded')}</b><span class="dim">${esc(r.repo || '')} · ${r.rows.length} moment${r.rows.length === 1 ? '' : 's'}</span></div>
      ${r.rows.map(({ m, i }) => `<div class="gy-row" data-i="${i}" title="${m.seq == null ? 'open this session (the exact moment couldn’t be pinpointed)' : 'open this session at that exact moment'}">
        <span class="gy-when" title="${esc(fmtAgo(m.ts))}">${esc(graveDate(m.ts))}</span>
        <span class="gy-icon" title="${esc(m.cmd)}">${GRAVE_ICON[m.kind] || '🗑'}</span>
        <div class="gy-main">
          <div class="gy-what">${esc(m.what)}<span class="dim"> — in “${esc(m.title || 'a session')}” on ${esc(m.machine ? machineLabel(m.machine) : 'this computer')}</span></div>
          ${graveFilesHTML(m)}
          <code class="gy-cmd" title="${esc(m.whole || m.command)}">${esc(m.command)}</code>
        </div>
        <span class="gy-open">${m.seq == null ? 'open session ↗' : 'open the moment ↗'}</span>
      </div>`).join('')}
    </div>`).join('');

  const empty = `<div class="sl-clear">✓ No agent has thrown work away in anything this computer can read.<div class="sl-clear-sub">None of the ${d.scannedSessions || 0} transcript${d.scannedSessions === 1 ? '' : 's'} here contains one of the four commands above. That is the normal, expected result — and it is a small sample, not a clean bill of health.</div></div>`;

  $('graveyard').innerHTML =
    `<div class="fleet-head"><h2>Graveyard — ${(d.totalMoments || moments.length) ? `${d.totalMoments || moments.length} moment${(d.totalMoments || moments.length) === 1 ? '' : 's'} where work was thrown away${d.totalMoments > moments.length ? ` (showing the newest ${moments.length})` : ''}` : 'nothing thrown away'}</h2>
      <span class="dim">${scanNote}</span>${homeButton('graveyard')}</div>
     ${note}
     ${d.error ? '' : moments.length ? body : empty}`;

  wireHomeButton($('graveyard'), 'graveyard', renderGraveyard);
  $('graveyard').querySelectorAll('.gy-row[data-i]').forEach(el => el.onclick = () => {
    const m = moments[+el.dataset.i];
    if (!m) return;
    if (m.seq == null) openSession(m.file); else openSessionAt(m.file, m.seq);
  });
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
        ${machineActions.map((m, i) => `<div class="hm-machine"><div class="hm-m-head"><b>🖥 ${esc(machineLabel(m.machine))}</b><span class="dim">${m.count} role${m.count !== 1 ? 's' : ''}</span><button class="mini-btn hm-copy" data-t="fail" data-i="${i}" style="margin-left:auto">📋 copy</button></div><pre class="hm-paste">${esc(m.paste)}</pre></div>`).join('')}` : ''}

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
// ---------- RULE REHEARSAL (would this standing order have been broken before?) ----------
// WHAT IT ANSWERS: before a rule is planted into every future session in a repo,
// how often would it have been broken in the sessions already on record?
// WHY IT IS DELIBERATELY NARROW: a rule written in English can mean anything, and a
// scorer that "interprets" it would print a confident number next to a rule it never
// actually checked. That is worse than no number, because the owner plants on the
// strength of it. So this understands exactly the sentence shapes listed in
// RR_SHAPES, shows those shapes on screen so the owner can see what is checkable,
// and answers anything else with "not automatically measurable" and nothing more.
// No LLM, no fuzzy matching, no partial credit, same answer every time.
// A NAMED TOOL MUST BE A REAL TOOL: "never use sudo" looks exactly like "never use
// Bash" to a regex. If the named word is not a tool this fleet has actually called,
// the clause is dropped as unmeasurable rather than scored 0 — a 0 there would read
// as "you never break this", when the truth is "I didn't understand you".
// RATES, NOT BOOLEANS: every answer is "broken in N of the M sessions where the rule
// APPLIED" — the denominator is only the sessions that had a chance to break it —
// and under RR_MIN_APPLICABLE runs there is no percentage at all.
const RR_MIN_APPLICABLE = 3;   // below this, no rate is shown for a clause or overall
const RR_MAX_EXAMPLES = 10;    // linked sessions listed per clause
const RR_MAX_CLAUSES = 6;      // a rule body is a rule, not a program
const RR_MAX_ROLES = 4;        // roles taken from one "a / b / c → tier" line
const RR_TIER_RANK = { cheap: 0, mid: 1, premium: 2, flagship: 3 };
// the shapes, in the owner's words — this array IS the on-screen help, so the two
// can never drift apart
const RR_SHAPES = [
  { kind: 'ban', example: 'never use Bash', re: /\b(?:never|do\s+not|don['’]?t)\s+(?:use|run|call|invoke|touch)\s+(?:the\s+)?[`"']?([A-Za-z][\w-]{1,40})[`"']?/i },
  { kind: 'order', example: 'always run Read before Edit', re: /\balways\s+(?:run|use|call)\s+[`"']?([A-Za-z][\w-]{1,40})[`"']?\s+before\s+(?:any\s+|each\s+|every\s+|you\s+)?[`"']?([A-Za-z][\w-]{1,40})[`"']?/i },
  { kind: 'tier', example: 'review agents must run on claude-sonnet-5 (or “review → claude-sonnet-5”)', re: /^[\s\-*•]*["'`]?([A-Za-z][\w /&+-]{1,40}?)["'`]?\s*(?:agents?|workers?|helpers?|subagents?)?\s*[:→-]*\s*(?:must|should)\s+(?:run\s+on|use)\s+(?:the\s+)?["'`]?([\w.-]+)["'`]?/i },
  { kind: 'tier', example: 'review / research → claude-sonnet-5', re: /^[\s\-*•]*["'`]?([A-Za-z][\w /&+-]{1,40}?)["'`]?\s*(?:agents?|workers?|helpers?|subagents?)?\s*(?:→|->)\s*["'`]?([\w.-]+)["'`]?/i },
];
// a line carrying any of these is a conditional rule, and gets refused outright
const RR_CONDITIONAL = /\b(?:unless|except|if|when|whenever|only|otherwise|prefer|try to|generally|usually|where possible)\b/i;
// same name normalisation mineFleet() aggregates roles by, so "review #3" and
// "review #7" are one role here too
const rrNorm = n => String(n || '').replace(/\s*#\d+$/, '').replace(/[0-9a-f-]{12,}/g, '·').slice(0, 30);
// A role matches an agent on whole words, never a bare substring — "read" must not
// silently claim every agent whose name happens to contain those four letters.
function rrRoleRe(role) {
  return new RegExp('\\b' + String(role).replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\b', 'i');
}
// every tool name this fleet has actually called, lowercase -> how it's really spelled
function rrVocab(data) {
  const v = new Map();
  for (const { d } of data) for (const e of (d.events || [])) if (e.tool && !v.has(e.tool.toLowerCase())) v.set(e.tool.toLowerCase(), e.tool);
  return v;
}
// a tier name, or a model id that resolves to one; null if it is neither
function rrTier(word) {
  const w = String(word || '').trim().toLowerCase().replace(/[.,;:)]+$/, '');
  if (RR_TIER_RANK[w] !== undefined) return w;
  const t = modelTier(w);
  return t === 'unknown' ? null : t;
}
// Parse the body into checkable clauses. Every clause carries `source` — the exact
// words it came from — so the report can show what it read rather than assert.
function rrParse(body, vocab) {
  const clauses = [];
  let unreadable = 0;
  for (const raw of String(body || '').split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    // A conditional rule ("never use X unless…", "…only when…") is not the flat rule
    // the shapes below would score it as, and scoring it flat would overcount every
    // legitimate exception. Refuse the whole line rather than read half of it.
    if (RR_CONDITIONAL.test(line)) { unreadable++; continue; }
    let got = false;
    for (const shape of RR_SHAPES) {
      const m = shape.re.exec(line);
      if (!m) continue;
      if (shape.kind === 'ban') {
        const tool = vocab.get(m[1].toLowerCase());
        if (tool) { clauses.push({ kind: 'ban', tool, source: line, label: `never use ${tool}` }); got = true; }
      } else if (shape.kind === 'order') {
        const before = vocab.get(m[1].toLowerCase()), after = vocab.get(m[2].toLowerCase());
        if (before && after && before !== after) { clauses.push({ kind: 'order', before, after, source: line, label: `always run ${before} before ${after}` }); got = true; }
      } else {
        const tier = rrTier(m[2]);
        if (tier) {
          // "review / research / accuracy-check" is three roles on one line
          const roles = m[1].split(/[\/,]| and /i).map(r => r.trim().replace(/\b(?:agents?|workers?|helpers?|subagents?)\b/ig, '').trim().toLowerCase())
            .filter(r => r.length >= 3).slice(0, RR_MAX_ROLES);
          for (const role of roles) { clauses.push({ kind: 'tier', role, tier, source: line, label: `"${role}" runs no higher than ${TIER_LABEL[tier].toLowerCase()} tier` }); got = true; }
        }
      }
      if (got) break;
    }
    if (!got) unreadable++;
  }
  // the cap is per rule, not per line — one "a / b / c → tier" line can add several.
  // Anything over it is reported as its own sentence: a clause that was dropped for
  // room is not the same thing as a line nobody could read.
  return { clauses: clauses.slice(0, RR_MAX_CLAUSES), unreadable, capped: Math.max(0, clauses.length - RR_MAX_CLAUSES) };
}
// Score one clause against the sessions on record. Returns real counts only.
function rrEval(clause, data) {
  const hits = [], applicable = [];
  let couldNotTell = 0;
  for (const { s, d } of data) {
    const evs = (d.events || []).filter(e => e.kind === 'tool-call' || e.kind === 'spawn');
    if (clause.kind === 'ban') {
      if (!evs.length) continue;                  // ran no tools at all: never had the chance
      applicable.push(s);
      const n = evs.filter(e => e.tool === clause.tool).length;
      if (n) hits.push({ s, why: `used ${clause.tool} ${n} time${n === 1 ? '' : 's'}` });
    } else if (clause.kind === 'order') {
      // per agent, in order: the rule is about what one worker did, not what the
      // session collectively happened to contain somewhere
      const byAgent = new Map();
      for (const e of evs) { if (!byAgent.has(e.agent)) byAgent.set(e.agent, []); byAgent.get(e.agent).push(e.tool); }
      let ranAfter = 0, bad = 0;
      for (const [, tools] of byAgent) {
        let seen = false;
        for (const t of tools) {
          if (t === clause.before) seen = true;
          else if (t === clause.after) { ranAfter++; if (!seen) bad++; }
        }
      }
      if (!ranAfter) continue;                    // never ran the second tool: rule could not bite
      applicable.push(s);
      if (bad) hits.push({ s, why: `ran ${clause.after} ${bad} time${bad === 1 ? '' : 's'} without ${clause.before} first` });
    } else {
      let judged = 0, matched = 0; const worse = [];
      const roleRe = clause.roleRe || (clause.roleRe = rrRoleRe(clause.role));
      for (const a of (d.agents || [])) {
        if (!roleRe.test(rrNorm(a.name || ''))) continue;
        matched++;
        const t = a.model ? modelTier(a.model) : 'unknown';
        if (t === 'unknown') continue;            // no model on record: cannot judge this one
        judged++;
        if (RR_TIER_RANK[t] > RR_TIER_RANK[clause.tier]) worse.push(`${modelShortName(a.model)} (${TIER_LABEL[t].toLowerCase()})`);
      }
      if (!matched) continue;
      if (!judged) { couldNotTell++; continue; }  // the role ran, but nothing recorded what it ran on
      applicable.push(s);
      if (worse.length) hits.push({ s, why: `${[...new Set(worse)].slice(0, 3).join(', ')} — above ${TIER_LABEL[clause.tier].toLowerCase()}` });
    }
  }
  return { clause, hits, files: applicable.map(s => s.file), applicable: applicable.length, couldNotTell, gated: applicable.length < RR_MIN_APPLICABLE };
}
// One clause's line in the report — a count and a denominator, never a bare percent.
function rrClauseHTML(r, i) {
  const c = r.clause;
  const body = r.gated
    ? `<span class="rr-none">not enough runs to say yet</span> <span class="dim">— only ${r.applicable} of the sessions read ever did this${r.couldNotTell ? `; ${r.couldNotTell} more couldn’t be judged` : ''}.</span>`
    : `<b class="${r.hits.length ? 'rr-bad' : 'rr-ok'}">broken in ${r.hits.length} of ${r.applicable}</b> <span class="dim">session${r.applicable === 1 ? '' : 's'} where it applied${r.couldNotTell ? ` (${r.couldNotTell} more couldn’t be judged)` : ''}</span>`;
  const list = (!r.gated && r.hits.length) ? `<div class="rr-hits">${r.hits.slice(0, RR_MAX_EXAMPLES).map(h => `
      <div class="rr-hit" data-file="${esc(h.s.file)}" title="open this session">
        <span class="rr-hit-t">${esc(h.s.title || h.s.session || 'a session')}</span>
        <span class="dim">${esc(h.why)} · ${esc(fmtAgo(h.s.mtime))}</span>
      </div>`).join('')}${r.hits.length > RR_MAX_EXAMPLES ? `<div class="dim">…and ${r.hits.length - RR_MAX_EXAMPLES} more</div>` : ''}</div>` : '';
  return `<div class="rr-clause"><div class="rr-clause-h"><span class="rr-i">${i + 1}</span><b>${esc(c.label)}</b>${body}</div>
    <div class="dim rr-src">read from: “${esc(trunc(c.source, 110))}”</div>${list}</div>`;
}
// The whole report. `data` is the same ~60-session cache Playbook Studio mines.
function rrReportHTML(body, data) {
  // No history read means no rule was checked — say that, never "not measurable",
  // which would blame the wording for a problem that is entirely on this end.
  if (!data.length) return { html: `<div class="dim">No sessions could be read on this computer, so this rule was <b>not checked at all</b>.</div>`, measured: false };
  const parsed = rrParse(body, rrVocab(data));
  // THE INTEGRITY LINE: nothing was checked, so nothing is claimed.
  if (!parsed.clauses.length) return { html: `<div class="rr-unmeasurable">not automatically measurable</div>`, measured: false };
  const results = parsed.clauses.map(c => rrEval(c, data));
  // Headline denominator is the UNION of what each clause could apply to, and the
  // numerator the union of what each clause caught — both built from the very same
  // evaluations the rows below print, so the two can never disagree.
  // ...and ONLY from clauses that individually cleared the gate. Unioning gated
  // clauses produced a confident headline percentage assembled entirely out of
  // samples the rows underneath had just declared too small to judge.
  const brokeIn = new Set(), appliedTo = new Set();
  const judgeable = results.filter(r => !r.gated);
  for (const r of judgeable) { for (const h of r.hits) brokeIn.add(h.s.file); for (const f of r.files) appliedTo.add(f); }
  const applied = appliedTo.size;
  const gated = !judgeable.length || applied < RR_MIN_APPLICABLE;
  const head = gated
    ? `<div class="rr-head rr-none">Not enough runs to say yet.</div><div class="dim">${judgeable.length ? `Only ${applied} of the ${data.length} sessions read ever did the thing this rule is about — too few to put a number on.` : `None of what this rule says could be checked against enough runs to put a number on it.`}</div>`
    : `<div class="rr-head ${brokeIn.size ? 'rr-bad' : 'rr-ok'}">Would have been broken in ${brokeIn.size} of the ${applied} recent session${applied === 1 ? '' : 's'} where it applied.</div>
       <div class="dim">Read your last ${data.length} sessions; ${applied} of them ever did the thing this rule is about. Sessions that never did it can’t break the rule, so they are left out of the count rather than padding it.</div>`;
  const notes = [
    parsed.unreadable ? `The other ${parsed.unreadable} line${parsed.unreadable === 1 ? '' : 's'} of this rule ${parsed.unreadable === 1 ? 'is' : 'are'} not automatically measurable, and ${parsed.unreadable === 1 ? 'was' : 'were'} not checked at all.` : null,
    parsed.capped ? `${parsed.capped} more checkable part${parsed.capped === 1 ? ' was' : 's were'} left out — this rehearsal reads at most ${RR_MAX_CLAUSES} parts of one rule.` : null,
  ].filter(Boolean);
  const partial = notes.length
    ? `<div class="rr-partial">Only the part${parsed.clauses.length === 1 ? '' : 's'} below could be checked. ${esc(notes.join(' '))}</div>`
    : '';
  return { html: head + partial + results.map(rrClauseHTML).join(''), measured: true };
}
// One worked example per shape, straight off RR_SHAPES so the help can never
// promise a sentence the parser doesn't actually understand.
function rrShapesHTML() {
  const byKind = new Map();
  for (const s of RR_SHAPES) if (!byKind.has(s.kind)) byKind.set(s.kind, s.example);
  return `<div class="rr-shapes dim">Sentences it can check: ${[...byKind.values()].map(e => `<code>${esc(e)}</code>`).join(' · ')}. Anything else is reported as not measurable rather than guessed at, and a named tool only counts if your sessions have really called a tool by that name.</div>`;
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
      <div class="rr-box">
        <div class="rr-bar"><button class="mini-btn" id="dcRehearse">🔎 Rehearse against my history</button>
          <span class="dim">Checks this rule against sessions you've already run, before it changes any future one. Reads only — nothing is planted or written.</span></div>
        ${rrShapesHTML()}
        <div id="dcRehearseOut"></div>
      </div>
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
  // ---- rule rehearsal: score this rule against history BEFORE anything is planted ----
  // The first run may have to read ~60 transcripts, so it stays a deliberate click.
  // Once that cache is warm re-scoring costs nothing, so the report then follows the
  // body live and can never sit there stale next to text it was not computed from.
  const rrOut = ov.querySelector('#dcRehearseOut');
  const rrBtn = ov.querySelector('#dcRehearse');
  let rrLive = false, rrT = null;
  const rrDraw = () => {
    const body = ov.querySelector('#dcBody').value.trim();
    if (!body) { rrOut.innerHTML = '<div class="dim">Write the rule first, then rehearse it.</div>'; return; }
    rrOut.innerHTML = rrReportHTML(body, flowsCache || []).html;
    rrOut.querySelectorAll('.rr-hit[data-file]').forEach(el => el.onclick = () => { ov.remove(); openSession(el.dataset.file); });
  };
  rrBtn.onclick = async () => {
    rrBtn.disabled = true;
    try {
      if (!flowsCache) {
        rrOut.innerHTML = '<div class="dim">Reading your recent sessions…</div>';
        if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
        await loadFlows.fetchOnly();
      }
      rrDraw();
      rrLive = true;
    } catch { rrOut.innerHTML = '<div class="dim">Couldn’t read your recent sessions just now, so this rule was <b>not checked at all</b>.</div>'; }
    rrBtn.disabled = false;
  };
  ov.querySelector('#dcBody').addEventListener('input', () => {
    if (!rrLive) return;
    clearTimeout(rrT); rrT = setTimeout(rrDraw, 350);
  });

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

// model tier from an id string (for token-saving analysis).
// Keep in lockstep with modelTier() in server.js. Size qualifiers are tested
// BEFORE family names, or `gpt-5-mini` gets billed as premium and inflates the
// very top-tier share this analysis exists to report honestly.
function modelTier(model) {
  const m = String(model || '').toLowerCase();
  if (/mini|nano|flash|haiku|lite/.test(m)) return 'cheap';
  if (/fable|mythos/.test(m)) return 'flagship';
  if (/sonnet|codex|gpt-4/.test(m)) return 'mid';
  if (/opus|gpt-5|o3|o1/.test(m)) return 'premium';
  if (/gpt-3/.test(m)) return 'cheap';
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

const ROLE_MIN_RUNS = 5;   // below this, show the raw counts instead of a rate
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
      // Attribute per model the agent ACTUALLY used, not just the first one it
      // reported. The server fixed this in costOf()/modelBreakdown() by keeping
      // per-model buckets; reading only a.model here quietly undid it in the UI,
      // so an agent that switched models showed all its spend under model one.
      const buckets = a.usageByModel && Object.keys(a.usageByModel).length ? a.usageByModel : null;
      if (buckets) {
        const totalOut = Object.values(buckets).reduce((n, b) => n + (b.outTokens || 0), 0);
        for (const [id, b] of Object.entries(buckets)) {
          const mm = models.get(id) || { agents: 0, cost: 0, outTok: 0, roles: new Set(), tier: modelTier(id) };
          mm.agents++; mm.outTok += b.outTokens || 0;
          // split the agent's cost across its models in proportion to output
          mm.cost += totalOut ? (a.cost || 0) * ((b.outTokens || 0) / totalOut) : 0;
          if (a.id !== 'main') mm.roles.add(norm(a.name));
          models.set(id, mm);
        }
      } else if (a.model) {
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
  if (k === 'machine-rename') return e.displayName ? { icon: '✏️', text: `Renamed <b>${esc(e.name)}</b> to <b>${esc(e.displayName)}</b>`, kind: 'control' } : { icon: '✏️', text: `Cleared the display name for <b>${esc(e.name)}</b>`, kind: 'control' };
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
    if (item) { const r = await (await fetch('/api/brain/file?id=' + item.id)).json(); if (!r.error) { brainCurrent = r; brainDisk = r.content; brainDirty = false; brainMode = 'view'; renderBrain(); } }
  });
}

// ---------- BRAIN view (memories, hooks, agent configs on this machine) ----------
let brainItems = [], brainCurrent = null, brainDirty = false, brainMode = 'view';
// What is actually ON DISK. The hooks/permissions panes edit brainCurrent.content
// in place, which erased the original and left the save with nothing to diff — so
// a toggled hook looked saved and never was. This keeps the before-picture.
let brainDisk = null;

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
  if (!brainDirty) brainDisk = brainCurrent.content;
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
  if (!brainDirty) brainDisk = brainCurrent.content;
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
// ---------- BLAST RADIUS (what would this permission rule actually have hit?) ----------
// WHAT IT ANSWERS: the owner types an allow/deny pattern into settings.json and has
// no way to know what it really covers. This shows the actual tool calls it would
// have matched, out of this computer's own history, so the rule gets judged by
// examples instead of trusted on faith.
// IT IS AN APPROXIMATION AND IT SAYS SO ON SCREEN: Claude Code's permission matcher
// lives inside Claude Code and moves with it. Re-implementing it exactly here would
// be a compatibility treadmill, and a preview that CLAIMED to be exact would be the
// worse failure — the owner would stop double-checking. So this covers the documented
// common shapes (whole tool, Bash prefix with `:*`, file-path globs, WebFetch
// `domain:`, MCP server prefixes) and returns "couldn't judge this one" for anything
// else, counted and reported, rather than quietly scoring it as a miss.
// READ-ONLY, permanently: this pane has no save button and no write route behind it.
// The Edit tab stays the only way this file ever changes, and only by hand.
let brCalls = null, brPattern = '', brEffect = 'deny';
const BR_MAX_ROWS = 40;      // matched calls listed before "+N more"
const BR_ARG_MAX = 130;      // characters of a command/path shown per row
async function loadToolCalls() {
  try { brCalls = await (await fetch('/api/tool-calls')).json(); }
  catch { brCalls = { calls: [], error: 'Could not read what your agents have actually run just now.' }; }
}
// "Bash", "Bash(git log:*)", "mcp__github" -> {tool, spec}; null if it isn't rule-shaped
function parsePermRule(src) {
  const m = /^([A-Za-z_][\w-]*)\s*(?:\(([\s\S]*)\))?$/.exec(String(src || '').trim());
  return m ? { tool: m[1], spec: m[2] == null ? null : m[2].trim() } : null;
}
function permToolMatches(rule, tool) {
  if (rule.tool === tool) return true;
  // a bare server name covers every tool that server exposes
  if (rule.tool.startsWith('mcp__') && !rule.tool.slice(5).includes('__')) return tool.startsWith(rule.tool + '__');
  return false;
}
// gitignore-ish path glob. `**` crosses folders, `*` and `?` do not. A leading `//`
// anchors at the start of the path; anything else is matched against the tail on a
// folder boundary, and a wildcard-free pattern also covers everything beneath it.
// memoised: the "rules already in this file" list tallies every rule against every
// call, so a settings file with 80 rules would otherwise rebuild thousands of
// identical regexes on each paint
const permGlobCache = new Map();
function permGlobRe(pat) {
  const hit = permGlobCache.get(pat);
  if (hit) return hit;
  const re = permGlobBuild(pat);
  if (permGlobCache.size > 400) permGlobCache.clear();
  permGlobCache.set(pat, re);
  return re;
}
function permGlobBuild(pat) {
  let p = String(pat).replace(/\\/g, '/').trim();
  let anchored = false;
  if (p.startsWith('//')) {
    p = p.slice(1); anchored = true;
    if (/^\/[A-Za-z]:/.test(p)) p = p.slice(1);  // //C:/work -> C:/work: a Windows absolute path has no leading slash
  } else if (p.startsWith('~/')) p = p.slice(1);
  let re = '';
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === '*' && p[i + 1] === '*') { re += '.*'; i++; if (p[i + 1] === '/') i++; }
    else if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else re += c.replace(/[.+^${}()|[\]\\]/g, '\\$&');
  }
  if (!/[*?]/.test(p) && !p.endsWith('/')) re += '(?:/.*)?';
  return new RegExp((anchored ? '^' : '(?:^|/)') + re + '$', 'i');
}
// true = matched, false = didn't, null = this preview will not guess about this call
function permMatch(rule, call) {
  if (!permToolMatches(rule, call.tool)) return false;
  if (rule.spec === null || rule.spec === '*') return true;
  if (/^domain:/i.test(rule.spec)) {
    if (call.argKind !== 'url') return null;
    const want = rule.spec.slice(7).trim().toLowerCase().replace(/^\*\./, '');
    let host;
    try { host = new URL(call.arg).hostname.toLowerCase(); } catch { return null; }
    return !!want && (host === want || host.endsWith('.' + want));
  }
  if (call.argKind === 'command') {
    const c = String(call.arg).trim();
    // `cd x && git push` is two commands, and a Bash(git:*) rule reaches the second
    // one. Testing only the whole string would UNDER-report an allow rule's reach —
    // the one direction a safety preview must never err in. The split is naive about
    // quoting on purpose: erring toward showing more is the safe side of that trade.
    const parts = [c, ...c.split(/&&|\|\||;/g).map(x => x.trim()).filter(x => x && x !== c)];
    if (rule.spec.endsWith(':*')) {
      const p = rule.spec.slice(0, -2).trim().toLowerCase();
      return !p || parts.some(x => x.toLowerCase().startsWith(p));
    }
    if (rule.spec.includes('*')) return null;   // a shape this preview doesn't claim to understand
    if (parts.some(x => x === rule.spec)) return true;
    // A clipped command is a PREFIX of the real one. If the rule doesn't match any
    // part of that prefix it definitely isn't the whole command, so that is a
    // confident no; only when the rule runs past the clip is the answer unknown.
    if (call.clipped) return rule.spec.startsWith(parts[parts.length - 1]) ? null : false;
    return false;
  }
  if (call.argKind === 'path') { try { return permGlobRe(rule.spec).test(String(call.arg).replace(/\\/g, '/')); } catch { return null; } }
  return null;                                   // no argument on record: nothing to match against
}
// counts for one rule over the sample — used both by the big preview and by the
// per-rule tallies next to the rules already in the file
function permTally(src, calls) {
  const rule = parsePermRule(src);
  if (!rule) return null;
  let hit = 0, unsure = 0;
  const rows = [];
  for (const c of calls) {
    const m = permMatch(rule, c);
    if (m === true) { hit++; if (rows.length < BR_MAX_ROWS) rows.push(c); }
    else if (m === null) unsure++;
  }
  return { rule, hit, unsure, rows };
}
const BR_VERB = { allow: 'let through without asking you', deny: 'blocked', ask: 'stopped to ask you about' };
function blastHTML() {
  const d = brCalls;
  if (!d) return '<div class="dim">Reading what your agents have actually run…</div>';
  if (d.error) return `<div class="dim">${esc(d.error)}</div>`;
  const calls = d.calls || [];
  if (!calls.length) return '<div class="dim">No tool calls could be read on this computer, so there is nothing to test a rule against.</div>';
  const src = brPattern.trim();
  if (!src) return `<div class="dim">Type a pattern above — <code>Bash(git log:*)</code>, <code>Read(//C:/work/**)</code>, <code>WebFetch(domain:example.com)</code> — to see which of these ${calls.length} calls it covers.</div>`;
  const t = permTally(src, calls);
  if (!t) return `<div class="br-unknown">That isn’t a shape this preview understands. Permission rules look like <code>Bash</code>, <code>Bash(git log:*)</code>, <code>Read(//C:/work/**)</code>, <code>WebFetch(domain:example.com)</code> or <code>mcp__github</code>.</div>`;
  const verb = BR_VERB[brEffect];
  const head = t.hit
    ? `<div class="br-head br-hit">This would have ${esc(verb)} <b>${t.hit}</b> of the ${calls.length} recent tool calls below.</div>`
    : t.unsure
      ? `<div class="br-head">Nothing in these ${calls.length} recent tool calls could be <i>confirmed</i> as ${esc(verb)} by this rule — but ${t.unsure} of them could not be judged either way, so this is not an all-clear.</div>`
      : `<div class="br-head">Nothing in these ${calls.length} recent tool calls would have been ${esc(verb)} by this rule. That is not a promise about the future — only that it hasn’t come up yet in what can be read here.</div>`;
  const unsure = t.unsure
    ? `<div class="br-unsure">${t.unsure} call${t.unsure === 1 ? '' : 's'} matched the tool name but <b>could not be judged</b> — the argument isn’t on record, it was too long to store whole, or the pattern uses a shape this preview won’t guess at. They are counted neither as covered nor as missed.</div>`
    : '';
  const rows = t.hit ? `<div class="br-rows">${t.rows.map(c => `
      <div class="br-row" data-file="${esc(c.file)}" title="open the session this ran in">
        <span class="br-tool">${esc(c.tool)}</span>
        <code class="br-arg">${esc(c.arg ? trunc(c.arg, BR_ARG_MAX) : '(no argument recorded)')}</code>
        <span class="dim br-when">${esc(fmtAgo(c.ts))} · ${esc(trunc(c.title || 'a session', 40))}</span>
      </div>`).join('')}${t.hit > t.rows.length ? `<div class="dim" style="padding:6px 2px">…and ${t.hit - t.rows.length} more</div>` : ''}</div>` : '';
  return head + unsure + rows;
}
// Every reason this sample is smaller than "everything", said out loud — the same
// contract the unsaved-work and secrets scans hold themselves to.
function brScopeHTML() {
  const d = brCalls || {};
  if (d.error || !(d.calls || []).length) return '';
  const older = (d.totalSessions || 0) - (d.scannedSessions || 0);
  const bits = [
    d.capped && older > 0 ? `${older} older session${older === 1 ? '' : 's'} weren’t read` : null,
    d.budgetHit ? 'the scan stopped early once it had read enough' : null,
  ].filter(Boolean);
  return `<div class="dim">Sample: the ${d.calls.length} most recent tool calls this computer has on record, from ${d.sessionsInSample || 0} session${d.sessionsInSample === 1 ? '' : 's'}${d.newest ? `, ${esc(fmtAgo(d.oldest))} to ${esc(fmtAgo(d.newest))}` : ''}${bits.length ? ` — ${esc(bits.join('; '))}, so a rule may reach further than this shows` : ''}.</div>`;
}
// The pane. Rules already in the file are listed with their own tallies, so the
// owner sees the blast radius of what they've ALREADY allowed, not just of the
// pattern they're typing.
function permsPaneHTML(content) {
  let cfg;
  try { cfg = JSON.parse(content); } catch { return '<div class="dim" style="padding:16px">This file isn’t valid JSON right now — fix it in Edit mode first.</div>'; }
  const perms = (cfg && cfg.permissions) || {};
  const calls = (brCalls && brCalls.calls) || [];
  const listHTML = kind => {
    const arr = Array.isArray(perms[kind]) ? perms[kind] : [];
    if (!arr.length) return `<div class="dim">No <b>${kind}</b> rules in this file.</div>`;
    return `<div class="br-chips">${arr.map(r => {
      const t = calls.length ? permTally(r, calls) : null;
      // a bare "0" would read as "covers nothing" when the honest answer is often
      // "covers nothing I could judge", so unjudged calls get their own mark
      const tally = t ? `<span class="br-count${t.hit ? ' br-count-hit' : ''}">${t.hit}${t.unsure ? ' +?' : ''}</span>` : '';
      const why = t ? `${t.hit} of the sample covered${t.unsure ? `, ${t.unsure} more couldn’t be judged` : ''} — click to see them` : 'click to see which calls this covers';
      return `<button class="br-chip" data-rule="${esc(r)}" data-effect="${esc(kind)}" title="${esc(String(r))}\n${esc(why)}"><code>${esc(trunc(String(r), 60))}</code>${tally}</button>`;
    }).join('')}</div>`;
  };
  return `<div class="hooks-explain">
    <div class="he-sec"><h4>Blast radius — what a permission rule would really have covered</h4>
      <div class="uw-note">This is a <b>preview only, and an approximation.</b> Claude Code does its own permission matching, and that matcher changes as Claude Code changes — this one covers the common shapes (a whole tool, <code>Bash(prefix:*)</code>, file-path patterns, <code>domain:</code>, and MCP server names), reads each half of a chained command like <code>cd x &amp;&amp; git push</code> separately, and openly says "couldn’t judge" for the rest instead of guessing. Where it has to lean, it leans toward showing you <b>more</b> than the real rule would catch, never less. Treat the matches as examples to read, <b>not as a guarantee</b>.
      <br><b>Nothing in this tab can change your settings.</b> It only reads. The one way this file is ever written is by hand in <b>Edit</b>, with the usual confirmation and audit entry.</div>
      ${brScopeHTML()}
    </div>
    <div class="he-sec"><h4>Try a pattern</h4>
      <div class="br-try">
        <input id="brPattern" type="text" spellcheck="false" placeholder="e.g. Bash(git log:*)" value="${esc(brPattern)}">
        <div class="seg" id="brEffectSeg">${['allow', 'ask', 'deny'].map(k => `<button data-e="${k}" class="${brEffect === k ? 'on' : ''}">${k}</button>`).join('')}</div>
      </div>
      <div id="brOut" class="br-out">${blastHTML()}</div>
    </div>
    <div class="he-sec"><h4>Rules already in this file</h4>
      <div class="dim" style="margin-bottom:8px">The number on each is how many of the sample above that rule covers. Click one to see them.</div>
      <div class="br-kind"><b>allow</b> ${listHTML('allow')}</div>
      <div class="br-kind"><b>ask</b> ${listHTML('ask')}</div>
      <div class="br-kind"><b>deny</b> ${listHTML('deny')}</div>
    </div>
  </div>`;
}
// Redraw only the result box, so typing never rebuilds (and scroll-jumps) the pane.
function drawBlast() {
  const out = $('brOut'); if (!out) return;
  out.innerHTML = blastHTML();
  out.querySelectorAll('.br-row[data-file]').forEach(el => el.onclick = () => openSession(el.dataset.file));
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
        // settings.local.json counts too — it carries permissions just the same
        const isPerms = /settings[\w.]*\.json/i.test(brainCurrent.name) || /"permissions"\s*:/.test(brainCurrent.content);
        const viewable = isJSON || isMD;
        let bodyHtml;
        if (brainMode === 'perms' && isPerms) bodyHtml = `<div id="brainViewer">${permsPaneHTML(brainCurrent.content)}</div>`;
        else if (brainMode === 'hooks' && isHooks) bodyHtml = `<div id="brainViewer">${renderHooksExplainer(brainCurrent.content)}</div>`;
        else if (brainMode === 'view' && viewable) bodyHtml = `<div id="brainViewer" class="${isJSON ? 'bv-json' : 'bv-md'}">${isJSON ? `<pre class="hj">${highlightJSON(brainCurrent.content)}</pre>` : renderMD(brainCurrent.content)}</div>`;
        else bodyHtml = `<textarea id="brainEditor" spellcheck="false">${esc(brainCurrent.content)}</textarea>`;
        return `
        <div class="brain-bar">
          <b>${esc(brainCurrent.name)}</b>
          <span class="dim" style="font-size:10.5px">${esc(brainCurrent.path)}</span>
          ${viewable ? `<div class="seg" id="brainModeSeg"><button data-m="view" class="${brainMode === 'view' ? 'on' : ''}">Read</button>${isHooks ? `<button data-m="hooks" class="${brainMode === 'hooks' ? 'on' : ''}">Hooks</button>` : ''}${isPerms ? `<button data-m="perms" class="${brainMode === 'perms' ? 'on' : ''}">Permissions</button>` : ''}<button data-m="edit" class="${brainMode === 'edit' ? 'on' : ''}">Edit</button></div>` : ''}
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
    brainCurrent = r; brainDisk = r.content; brainDirty = false; brainMode = 'view'; renderBrain();
  });
  $('brain').querySelector('#brainModeSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => {
    // hooks mode keeps staged edits (toggles live there); switching to Read discards raw-text edits
    if (brainDirty && b.dataset.m === 'view' && !confirm('Switch to Read and discard unsaved edits?')) return;
    if (brainDirty && b.dataset.m === 'view') brainDirty = false;
    brainMode = b.dataset.m; renderBrain();
  });
  $('brain').querySelectorAll('.he-toggle input').forEach(t => t.onchange = () => toggleHook(t.dataset.ev, Number(t.dataset.idx), t.dataset.on === 'true'));
  $('brain').querySelectorAll('.hook-starter').forEach(b => b.onclick = () => addStarterHook(Number(b.dataset.i)));
  // ---- blast radius: read-only. Typing redraws only its own result box, so the
  // pane never scroll-jumps mid-sentence; nothing here can stage or save a change.
  if (brainMode === 'perms') {
    if (!brCalls) loadToolCalls().then(() => { if (brainMode === 'perms') renderBrain(); });
    const pi = $('brPattern');
    if (pi) { let brT = null; pi.oninput = () => { brPattern = pi.value; clearTimeout(brT); brT = setTimeout(drawBlast, 250); }; }
    $('brEffectSeg')?.querySelectorAll('button').forEach(b => b.onclick = () => { brEffect = b.dataset.e; renderBrain(); });
    $('brain').querySelectorAll('.br-chip').forEach(b => b.onclick = () => { brPattern = b.dataset.rule; brEffect = b.dataset.effect; renderBrain(); });
    drawBlast();
  }
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
      brainCurrent.content = snap.content; brainDisk = snap.content; brainCurrent.mtime = j.mtime; brainDirty = false; brainMode = 'view'; renderBrain();
    });
  };
  const ed = $('brainEditor');
  if (ed) ed.oninput = () => { if (!brainDirty) { brainDirty = true; const b = $('brainSave'); b.disabled = false; b.textContent = '💾 Save'; } };
  // PRE-EXISTING BUG: this was wired only inside `if (ed)`, so in hooks mode — where
  // there is no textarea — toggling a hook staged a change and lit up an enabled
  // "Save" button that did nothing at all. Silently discarding a hook edit the owner
  // believes they saved is the worst possible failure for this particular file.
  const saveBtn = $('brainSave');
  if (saveBtn) {
    saveBtn.onclick = async () => {
      // diff + confirm before writing; loudest warning for hooks/settings (they execute)
      const before = brainDisk != null ? brainDisk : brainCurrent.content;
      const after = ed ? ed.value : brainCurrent.content;
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
      brainCurrent.content = after; brainDisk = after; brainCurrent.mtime = j.mtime; brainDirty = false;
      const b = $('brainSave'); b.disabled = true; b.textContent = '✓ Saved (audited)';
    };
  }
}

// ---------- AUDIT view (immutable record of state-changing actions) ----------
const AUDIT_ICON = { 'brain-write': '✍️', launch: '🚀', 'launch-end': '🏁', kill: '🛑', approval: '✅', enqueue: '📤' };
// ---------- MACHINES view ----------
// Mirrors RHYTHM_MIN_SESSIONS in server.js — only used here as a display
// fallback if an older hub response ever omits `needed`.
const MACHINE_RHYTHM_MIN_SESSIONS = 5;
async function loadMachines() {
  const [machinesData, fleet] = await Promise.all([
    fetch('/api/machines').then(r => r.json()),
    fleetCache ? Promise.resolve(fleetCache) : fetch('/api/fleet').then(r => r.json()),
  ]);
  fleetCache = fleet;
  renderMachineWarnBar(machinesData);
  const byMachine = {};
  for (const s of fleet) {
    const m = s.machine || 'unknown';
    (byMachine[m] = byMachine[m] || { sessions: 0, agents: 0, cost: 0, kinds: {}, lastMs: 0 });
    byMachine[m].sessions++; byMachine[m].agents += s.agents; byMachine[m].cost += s.cost;
    byMachine[m].kinds[s.kind] = (byMachine[m].kinds[s.kind] || 0) + 1;
    byMachine[m].lastMs = Math.max(byMachine[m].lastMs, s.mtime);
  }
  const known = new Set(machinesData.map(m => m.name));
  // A machine we only know about via cached session data (no /api/machines entry
  // yet) hasn't got a rhythm computed for it either — say so rather than guess.
  for (const m of Object.keys(byMachine)) if (!known.has(m)) machinesData.push({ name: m, ips: [], lastSeen: byMachine[m].lastMs, remote: true, quiet: { enoughHistory: false, quiet: false, sessions: 0 } });
  const archives = await (await fetch('/api/archive')).json().catch(() => ({ archives: [] }));
  const archByMachine = {}; for (const a of (archives.archives || [])) archByMachine[a.machine] = a;
  $('machines').innerHTML =
    `<div class="fleet-head"><h2>Machines — ${machinesData.length}</h2></div>` +
    `<div class="machine-grid">` + machinesData.map(m => {
      const st = byMachine[m.name] || { sessions: 0, agents: 0, cost: 0, kinds: {} };
      const q = m.quiet || { enoughHistory: false, quiet: false };
      const fresh = Date.now() - m.lastSeen < 120000;
      const kindDots = Object.entries(st.kinds).map(([k, n]) => `<span class="mkind" style="color:${kindColor(k)}">● ${(AGENT_KIND[k] || AGENT_KIND.claude).label} ${n}</span>`).join('');
      const hubV = machinesData.find(x => !x.remote)?.version;
      const drift = m.version && hubV && m.version !== hubV;
      const arch = archByMachine[safeName(m.name)] || archByMachine[m.name];
      const disp = machineLabel(m.name);
      const renamed = disp !== m.name;
      // "quiet" (rhythm-broken silence) outranks the plain live/idle read —
      // idle is normal for a machine between runs, quiet is not.
      const statusLabel = q.quiet ? 'not checking in' : fresh ? 'live' : 'idle';
      const statusClass = q.quiet ? 'quiet' : fresh ? 'on' : '';
      return `<div class="mcard ${fresh ? 'fresh' : ''}${q.quiet ? ' mcard-quiet' : ''}">
        <h3${renamed ? ` title="real name: ${esc(m.name)}"` : ''}>${m.remote ? '⇄' : '★'} ${esc(disp)}
          ${m.remote ? `<button class="mini-btn mrename" data-machine="${esc(m.name)}" title="rename this machine">✏️</button>` : ''}
          ${m.version ? `<span class="mver ${drift ? 'drift' : ''}" title="${drift ? 'version differs from hub v' + esc(hubV) : 'app version'}">v${esc(m.version)}${drift ? ' ⚠' : ''}</span>` : ''}
          <span class="mstatus ${statusClass}">${statusLabel}</span></h3>
        <div class="mips">${(m.ips || []).map(ip => `<span class="ip">${esc(ip)}</span>`).join('') || '<span class="ip dim">no IPs reported</span>'}</div>
        <div class="mstats"><span><b>${st.sessions}</b> sessions</span><span><b>${st.agents}</b> agents</span><span class="fcost"><b>~${fmtUsd(st.cost)}</b></span></div>
        <div class="mkinds">${kindDots}</div>
        ${machineQuietBlockHTML(m, q)}
        ${arch && arch.files ? `<button class="mini-btn arch-browse" data-machine="${esc(arch.machine)}">📚 ${arch.files} archived transcripts · ${fmtBytes(arch.bytes)} — browse</button>` : ''}
        <div class="fdate">last seen ${new Date(m.lastSeen).toLocaleString()}</div>
      </div>`;
    }).join('') + `</div>`;
  $('machines').querySelectorAll('.arch-browse').forEach(b => b.onclick = () => openArchiveBrowser(b.dataset.machine));
  $('machines').querySelectorAll('.mrename').forEach(b => b.onclick = async e => {
    e.stopPropagation();
    const real = b.dataset.machine;
    const name = prompt('Rename machine (blank = use its real name):', metaMachineNames[real] || '');
    if (name === null) return;
    // Metadata only — this can never rename anything on the relay machine
    // itself (the hub never opens a connection out to one), and sessions stay
    // keyed on `real` the whole time.
    await metaPost('/api/meta/machine', { name: real, displayName: name });
    loadMachines();
  });
}
// The one line that would have saved two days: a plain sentence, not a status
// dot, plus the honest caveat that the hub can only ever watch, never reach
// back out to ask why.
function machineQuietBlockHTML(m, q) {
  if (!m.remote) return '';
  if (!q.enoughHistory) {
    return `<div class="mquiet-note">Not enough check-ins yet (${q.sessions || 0}/${q.needed || MACHINE_RHYTHM_MIN_SESSIONS}) to know this machine's normal rhythm — no verdict until then.</div>`;
  }
  if (!q.quiet) return '';
  return `<div class="mquiet-warn">
    <div><b>⚠️ ${esc(machineLabel(m.name))} has not checked in for ${fmtDurWords(q.silenceMs)}</b> — it usually reports every ${fmtDurWords(q.medianGapMs)}.</div>
    <div class="mquiet-caveat">AMC only ever watches this machine — it never reaches back out to it, so it can't tell you why. Worth checking: is the machine powered on and online? Is the relay still running there? Any VPN or network change on that end?</div>
  </div>`;
}
// ---------- fleet-level "machine went quiet" banner ----------
// Persistent, not a one-off bell ping — visible whenever the owner looks,
// not only if the tab happened to be open the moment the line was crossed.
function renderMachineWarnBar(machinesData) {
  const bar = $('machineWarnBar');
  if (!bar) return;
  const quiet = (machinesData || []).filter(m => m.remote && m.quiet && m.quiet.quiet);
  if (!quiet.length) { bar.style.display = 'none'; bar.innerHTML = ''; return; }
  bar.style.display = '';
  bar.innerHTML = quiet.map(m => `<div class="mwarn-row">⚠️ <b>${esc(machineLabel(m.name))}</b> has not checked in for ${fmtDurWords(m.quiet.silenceMs)} — it usually reports every ${fmtDurWords(m.quiet.medianGapMs)}. AMC can't tell why, only that it's quiet. <button class="mini-btn mwarn-go">check Machines</button></div>`).join('');
  bar.querySelectorAll('.mwarn-go').forEach(b => b.onclick = () => { state.view = 'machines'; setTabs(); });
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
    suns.set(m, { id: 'm:' + m, machine: m, sun: true, x: W / 2 + Math.cos(ang) * 180, y: H / 2 + Math.sin(ang) * 140, vx: 0, vy: 0, r: 16, label: machineLabel(m) });
  });
  const maxCost = Math.max(...fleetCache.map(s => s.cost), 1);
  const stars = fleetCache.map(s => {
    const sun = suns.get(s.machine || 'local');
    const tier = dominantTier(s);
    return {
      id: s.file, sun: false, file: s.file, machine: s.machine,
      x: sun.x + (Math.random() - 0.5) * 120, y: sun.y + (Math.random() - 0.5) * 120, vx: 0, vy: 0,
      // size stays cost (unchanged); colour is now the session's dominant model
      // tier — see dominantTier()/TIER_COLOR_HEX up top — so an expensive corner
      // of the fleet reads as a red cluster at a glance.
      r: 3 + Math.sqrt(s.cost / maxCost) * 14, color: TIER_COLOR_HEX[tier] || TIER_COLOR_HEX.unknown, sunNode: sun,
      active: Date.now() - s.mtime < 6 * 3600e3, title: s.title || s.session.slice(0, 8), kind: s.kind,
      tier, cost: s.cost,
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
      const label = hover.title + (hover.tier ? ` · ${TIER_LABEL[hover.tier]} · ~${fmtUsd(hover.cost)}` : '');
      ctx.fillText(label, hover.x + hover.r + 4 / scale, hover.y + 4 / scale);
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
// same idea as fmtDur but in words, for a plain sentence a non-technical owner
// reads once and understands — "45 hours", "usually every 4 minutes", etc.
function fmtDurWords(ms) {
  if (!ms || ms < 0) return 'a while';
  const m = Math.round(ms / 60000);
  if (m < 1) return 'under a minute';
  if (m < 90) return `${m} minute${m === 1 ? '' : 's'}`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h} hour${h === 1 ? '' : 's'}`;
  const d = Math.round(h / 24);
  return `${d} day${d === 1 ? '' : 's'}`;
}
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
  bar.innerHTML = `<span>💥 ${esc(failureSentence(diag))}</span><button class="mini-btn" id="failCompareBtn" style="margin-left:10px">🪜 compare with last successful run</button>`;
  bar.style.display = '';
  const cmp = $('failCompareBtn');
  if (cmp) cmp.onclick = () => openDivergenceFor(state.file);
}

// ---------- DIVERGENCE LADDER ----------
// WHY: "it broke" isn't enough — the useful question is WHERE it stopped matching
// the run that worked. Candidates are found automatically by matching the VERBATIM
// subagent task text recorded at spawn (agent.task, set in server.js normalize()) —
// that's the one string a script/playbook re-issues identically every time it
// delegates the same piece of work, so two runs of "the same job" line up without
// any manual curation. Reuses flowsCache, the same fleet-wide (cross-machine, relay
// included) cache Playbook Studio already builds — no new fetch, no new cache.
// Individual per-run facts only (this run succeeded, that one failed) — never an
// aggregate rate, so the data-discipline minimum-sample rule doesn't apply here.
const DL_STEP_CAP = 300; // hard cap so one runaway session can't blow up the O(n·m) alignment
// Which subagent in a broken session to blame: prefer one diagnoseFailure-style
// evidence actually points at (errored/stalled), and only among agents that carry
// a verbatim spawn task — that's the only string safe to match against another run.
function divergenceCulprit(sessionData) {
  const withTask = (sessionData.agents || []).filter(a => a.id !== 'main' && a.task);
  if (!withTask.length) return null;
  return withTask.find(a => agentOutcome(a) === 'failed')
    || withTask.find(a => agentOutcome(a) === 'stalled')
    || (withTask.length === 1 ? withTask[0] : null);
}
// A subagent's ordered (toolName, key-argument) trace. diagSig/diagTarget already
// exist above (Playbook Studio's failure diagnosis) — sig for exact-match equality,
// label for what gets printed.
function stepsOfAgent(sessionData, agentId) {
  return (sessionData.events || [])
    .filter(e => e.agent === agentId && (e.kind === 'tool-call' || e.kind === 'spawn') && e.tool)
    .map(e => ({ tool: e.tool, sig: diagSig(e), label: diagTarget(e) }));
}
const stepEq = (x, y) => !!x && !!y && x.sig === y.sig;
// ---------- Needleman-Wunsch: global alignment of two step sequences ----------
// Plain O(n·m) DP, no library. +2 for a matching step, -1 for a mismatch or a gap
// (an insertion/deletion in one run that the other doesn't have). Returns the
// aligned pairs in order — {a,b} indices into each step list, either one null
// where a run has a step the other doesn't — so a run that diverged by ADDING or
// SKIPPING a step still lines back up afterward instead of shifting everything.
function nwAlign(a, b, eq) {
  const n = a.length, m = b.length;
  const MATCH = 2, MISMATCH = -1, GAP = -1;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = 1; i <= n; i++) dp[i][0] = dp[i - 1][0] + GAP;
  for (let j = 1; j <= m; j++) dp[0][j] = dp[0][j - 1] + GAP;
  for (let i = 1; i <= n; i++) {
    for (let j = 1; j <= m; j++) {
      const sub = dp[i - 1][j - 1] + (eq(a[i - 1], b[j - 1]) ? MATCH : MISMATCH);
      dp[i][j] = Math.max(sub, dp[i - 1][j] + GAP, dp[i][j - 1] + GAP);
    }
  }
  const out = [];
  let i = n, j = m;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && dp[i][j] === dp[i - 1][j - 1] + (eq(a[i - 1], b[j - 1]) ? MATCH : MISMATCH)) {
      out.push({ a: i - 1, b: j - 1 }); i--; j--;
    } else if (i > 0 && dp[i][j] === dp[i - 1][j] + GAP) {
      out.push({ a: i - 1, b: null }); i--;
    } else {
      out.push({ a: null, b: j - 1 }); j--;
    }
  }
  return out.reverse();
}
// Best candidate: same verbatim task string, a DIFFERENT session, and that
// subagent actually finished clean. Most recent such match wins ("yesterday's
// working run"). flowsCache already spans every machine that's relayed in, so
// this isn't scoped to sessions on this computer.
// Match on the VERBATIM spawn prompt, never on `task` — `task` is the Task tool's
// 3-6 word description ("review auth", "fix tests"), which dozens of unrelated runs
// share, so matching on it compared entirely different jobs as "the same work".
// A prompt too short to be distinctive is refused outright rather than guessed at.
const DIV_MIN_PROMPT = 60;
function divMatchKey(a) {
  const p = String((a && a.spawnPrompt) || '').trim().replace(/\s+/g, ' ');
  return p.length >= DIV_MIN_PROMPT ? p : null;
}
async function findDivergenceCandidate(file, culprit) {
  if (!flowsCache) await loadFlows.fetchOnly();
  const key = divMatchKey(culprit);
  if (!key) return { tooVague: true };
  let best = null;
  for (const { s, d } of (flowsCache || [])) {
    if (s.file === file) continue;
    for (const a of d.agents) {
      if (a.id === 'main' || divMatchKey(a) !== key || agentOutcome(a) !== 'succeeded') continue;
      if (!best || (s.mtime || 0) > (best.s.mtime || 0)) best = { s, d, a };
    }
  }
  return best;
}
let divState = { sourceFile: null, status: null };
function openDivergenceFor(file) {
  if (!file) return;
  divState = { sourceFile: file, status: 'pending' };
  state.view = 'divergence';
  setTabs();
}
async function resolveDivergence(file) {
  let s = (fleetCache || []).find(x => x.file === file) || (sessionsCache || []).find(x => x.file === file) || { file, title: file };
  let d;
  try { d = await (await fetch('/api/session?file=' + encodeURIComponent(file))).json(); } catch { d = null; }
  if (!d || d.error) { divState = { sourceFile: file, status: 'done:' + file, sourceTitle: s.title || file, error: 'Could not re-read that session just now — try again from the session view.' }; return; }
  const culprit = divergenceCulprit(d);
  if (!culprit) {
    divState = { sourceFile: file, status: 'done:' + file, sourceTitle: s.title || file, error: 'This session\'s trouble isn\'t tied to one subagent with a recorded task string, so there\'s nothing to safely match it against — never comparing unrelated runs.' };
    return;
  }
  const cand = await findDivergenceCandidate(file, culprit);
  if (cand && cand.tooVague) {
    divState = { sourceFile: file, status: 'done:' + file, sourceTitle: s.title || file, culpritTask: culprit.task, error: `This agent's recorded instruction is too short to identify the same job in another run, so comparing could put two unrelated runs side by side. Nothing is shown rather than something misleading.` };
    return;
  }
  if (!cand) {
    divState = { sourceFile: file, status: 'done:' + file, sourceTitle: s.title || file, culpritTask: culprit.task, error: `No other run across your ${(flowsCache || []).length} most recent sessions was given this same instruction and finished clean — nothing to compare yet.` };
    return;
  }
  const aSteps = stepsOfAgent(d, culprit.id).slice(0, DL_STEP_CAP);
  const bSteps = stepsOfAgent(cand.d, cand.a.id).slice(0, DL_STEP_CAP);
  const align = nwAlign(aSteps, bSteps, stepEq);
  let firstDiv = -1;
  for (let i = 0; i < align.length; i++) {
    const p = align[i];
    const same = p.a != null && p.b != null && stepEq(aSteps[p.a], bSteps[p.b]);
    if (!same) { firstDiv = i; break; }
  }
  divState = {
    sourceFile: file, status: 'done:' + file, error: null,
    a: { file, title: s.title || file, agent: culprit, steps: aSteps },
    b: { file: cand.s.file, title: cand.s.title || cand.s.file, agent: cand.a, steps: bSteps, mtime: cand.s.mtime, machine: cand.s.machine },
    align, firstDiv,
  };
}
async function loadDivergence() {
  const pane = $('divergence');
  if (!pane) return;
  if (divState.sourceFile && divState.status !== 'done:' + divState.sourceFile) {
    pane.innerHTML = '<div class="fleet-loading">Looking through the fleet for a run that matches this one…</div>';
    await resolveDivergence(divState.sourceFile);
  }
  renderDivergence();
}
function renderDivergence() {
  const pane = $('divergence');
  if (!pane) return;
  const head = `<div class="fleet-head"><h2>Divergence Ladder</h2><span class="dim">Put a broken run next to the last one that worked, and see the exact step they stopped agreeing on.</span>${homeButton('divergence')}</div>`;
  if (!divState.sourceFile) {
    pane.innerHTML = head + '<div class="uw-empty">Open a failed or stalled session — the warning bar at the top of it gets a "🪜 compare with last successful run" button. Click that; this page fills in.</div>';
    return;
  }
  if (divState.error) {
    pane.innerHTML = head + `<div class="fleet-head" style="margin-top:0"><span class="dim">Session: ${esc(divState.sourceTitle || divState.sourceFile)}</span></div><div class="uw-empty">${esc(divState.error)}</div>`;
    return;
  }
  const { a, b, align, firstDiv } = divState;
  const capNote = (a.steps.length >= DL_STEP_CAP || b.steps.length >= DL_STEP_CAP) ? ' Only the first ' + DL_STEP_CAP + ' tool calls of each run were compared.' : '';
  const rows = align.map((p, i) => {
    const av = p.a != null ? a.steps[p.a] : null;
    const bv = p.b != null ? b.steps[p.b] : null;
    const same = av && bv && stepEq(av, bv);
    const rowCls = i === firstDiv ? 'dl-first-div' : same ? '' : 'dl-diff';
    const cell = st => st ? `<b>${esc(st.tool)}</b> <span class="dim">${esc(st.label)}</span>` : '<span class="dim">— (no matching step here)</span>';
    return `<tr class="${rowCls}"><td class="dl-n">${i + 1}</td><td>${cell(av)}</td><td>${cell(bv)}</td></tr>`
      + (i === firstDiv ? '<tr class="dl-flag-row"><td></td><td colspan="2">⬆ first step where these two runs stopped agreeing</td></tr>' : '');
  }).join('');
  pane.innerHTML = head +
    `<div class="dl-cols">
      <div class="dl-col-head dl-bad" data-file="${esc(a.file)}" title="open this session"><b>💥 ${esc(a.title)}</b><span class="dim">${esc(a.agent.name || 'subagent')} · this run</span></div>
      <div class="dl-col-head dl-good" data-file="${esc(b.file)}" title="open this session"><b>✅ ${esc(b.title)}</b><span class="dim">${esc(b.agent.name || 'subagent')} · ${esc(b.machine || 'local')} · ${fmtAgo(b.mtime)}</span></div>
    </div>
    <div class="table-wrap"><table class="ftable dl-table"><thead><tr><th></th><th>Broken run</th><th>Last successful run</th></tr></thead><tbody>${rows}</tbody></table></div>
    <div class="uw-note">Matched on the identical subagent task both runs were spawned with — “${esc(trunc(a.agent.task, 160))}”. That's what makes this comparison safe without curating a dataset by hand.${capNote}${firstDiv < 0 ? ' These two traces never actually diverged in tool-call sequence — whatever went wrong happened after the compared steps, or outside this subagent.' : ''}</div>`;
  wireHomeButton(pane, 'divergence', renderDivergence);
  pane.querySelectorAll('[data-file]').forEach(el => el.onclick = () => openSession(el.dataset.file));
}

// ---------- DEJA VU FINDER ----------
// WHY: plain history search over your own prompts isn't novel — Ctrl+F does that.
// What earns this a place is REACH — the corpus is every subagent delegation task
// string relayed into this dashboard from ANY machine, including ones offline right
// now — and every hit is labelled with its real OUTCOME, not just that it was said
// before. Index is a small hand-rolled TF-IDF built client-side over flowsCache
// (already the same fleet-wide, ~60-session cache Playbook Studio builds), cached
// until the fleet is refreshed — cheap on purpose, nothing server-side to maintain.
// END STATE decides the outcome, not "did anything ever go wrong on the way".
// Counting any single recovered tool error as 'failed' branded roughly a fifth of
// completed runs as failures — agents hit errors and recover constantly, and that
// is normal work, not a bad run. Recovered errors surface as a neutral note instead.
function agentOutcome(a) {
  if (a.pendingTool && a.pendingTool.since) return 'stalled';
  if (a.lastErrored || a.retrying) return 'failed';
  if (a.done) return 'succeeded';
  return 'unclear';
}
function agentRecoveredNote(a) {
  const n = a.errors || 0;
  return (n > 0 && agentOutcome(a) === 'succeeded') ? `${n} tool error${n === 1 ? '' : 's'}, recovered` : '';
}
const DEJA_OUTCOME = {
  succeeded: { icon: '✅', color: 'var(--green)', label: 'succeeded' },
  failed:    { icon: '💥', color: 'var(--red)',   label: 'failed' },
  stalled:   { icon: '⏳', color: 'var(--amber)', label: 'stalled' },
  unclear:   { icon: '❔', color: 'var(--dim)',   label: 'outcome unclear' },
};
const DEJA_STOP = new Set(['the', 'a', 'an', 'and', 'or', 'of', 'to', 'in', 'on', 'for', 'with', 'is', 'are', 'this', 'that', 'it', 'as', 'be', 'at', 'by', 'from', 'into', 'then', 'than', 'not', 'no']);
function tokenize(s) {
  const m = String(s || '').toLowerCase().match(/[a-z0-9][a-z0-9_.]{1,}/g);
  return m ? m.filter(t => !DEJA_STOP.has(t)) : [];
}
let dejaIndex = null; // { docs:[{s,a,tokens,tf,outcome}], df:Map, N }
function buildDejaIndex() {
  const docs = [], df = new Map();
  for (const { s, d } of (flowsCache || [])) {
    for (const a of d.agents) {
      if (a.id === 'main' || !a.task) continue;
      const tokens = tokenize(a.task);
      if (!tokens.length) continue;
      const tf = new Map();
      for (const t of tokens) tf.set(t, (tf.get(t) || 0) + 1);
      for (const t of tf.keys()) df.set(t, (df.get(t) || 0) + 1);
      docs.push({ s, a, tokens, tf, outcome: agentOutcome(a) });
    }
  }
  dejaIndex = { docs, df, N: docs.length };
}
// score = sum of tf·idf over query terms present in the doc, damped by sqrt(doc
// length) so a long task string doesn't win purely by having more words to match.
function dejaSearch(query, top) {
  if (!dejaIndex || !dejaIndex.N) return [];
  const qTerms = [...new Set(tokenize(query))];
  if (!qTerms.length) return [];
  const { docs, df, N } = dejaIndex;
  const idf = t => Math.log((N + 1) / ((df.get(t) || 0) + 1)) + 1; // smoothed, always > 0
  const scored = [];
  for (const doc of docs) {
    let score = 0;
    for (const t of qTerms) { const c = doc.tf.get(t); if (c) score += c * idf(t); }
    if (score > 0) scored.push({ doc, score: score / Math.sqrt(doc.tokens.length) });
  }
  scored.sort((x, y) => y.score - x.score);
  return scored.slice(0, top).map(x => x.doc);
}
let dejaQuery = '';
async function loadDejaVu() {
  // A failed fetch used to leave the spinner up forever with an unhandled rejection
  // behind it — a pane that never resolves looks identical to one still working.
  try {
    if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
    if (!flowsCache) { $('dejavu').innerHTML = '<div class="fleet-loading">Indexing delegated tasks across your fleet…</div>'; await loadFlows.fetchOnly(); }
    buildDejaIndex();
    renderDejaVu();
  } catch (e) {
    $('dejavu').innerHTML = `<div class="fleet-head"><h2>Déjà Vu</h2></div><div class="uw-note">Couldn’t read your session history just now, so there is nothing to search. ${esc(String(e && e.message || ''))}</div>`;
  }
}
function renderDejaVu() {
  const pane = $('dejavu');
  if (!pane) return;
  if (!pane.querySelector('#dejaBox')) {
    // shell built once — the box only re-renders the results div below it, so
    // typing never loses focus or cursor position (same pattern as spickerSearch)
    pane.innerHTML =
      `<div class="fleet-head"><h2>Deja Vu Finder</h2><button class="mini-btn" id="dejaRefresh">↻ refresh index</button>${homeButton('dejavu')}</div>
       <div class="uw-note">Honestly, this part isn't clever — it's history search. What's useful is <b>reach</b>: <span id="dejaCount">${dejaIndex ? dejaIndex.N : 0}</span> delegated tasks indexed from every machine that has ever relayed into this dashboard, offline ones included, and every match is labelled with what actually happened — not just that it was asked before.</div>
       <input id="dejaBox" type="text" class="deja-box" placeholder="type what you're about to ask an agent to do…">
       <div id="dejaResults" class="deja-results"></div>`;
    wireHomeButton(pane, 'dejavu', renderDejaVu);
    const box = $('dejaBox');
    box.value = dejaQuery;
    box.oninput = () => { dejaQuery = box.value; renderDejaResults(); };
    $('dejaRefresh').onclick = () => { flowsCache = null; pane.innerHTML = ''; loadDejaVu(); };
  }
  const cnt = $('dejaCount'); if (cnt) cnt.textContent = dejaIndex ? dejaIndex.N : 0;
  renderDejaResults();
}
function renderDejaResults() {
  const el = $('dejaResults');
  if (!el) return;
  const q = dejaQuery.trim();
  const results = q ? dejaSearch(q, 5) : [];
  el.innerHTML = !q
    ? '<div class="uw-empty">Start typing — top 5 closest matches from tasks you\'ve delegated before, wherever they ran.</div>'
    : !results.length
      ? '<div class="uw-empty">No close match in the indexed history. This looks new.</div>'
      : results.map(doc => {
          const { s, a, outcome } = doc;
          const b = DEJA_OUTCOME[outcome] || DEJA_OUTCOME.unclear;
          return `<div class="deja-row" data-file="${esc(s.file)}" title="open this session">
            <span class="deja-badge" style="color:${b.color}" title="${b.label}">${b.icon}</span>
            <div class="deja-main">
              <div class="deja-task">${esc(a.task)}</div>
              <div class="dim">${esc(a.name || 'subagent')} in "${esc(s.title || s.session || '')}" · ${esc(s.machine ? machineLabel(s.machine) : 'local')} · ${fmtAgo(s.mtime)} · ${fmtUsd(a.cost || 0)} · <span style="color:${b.color}">${b.label}</span></div>
            </div>
          </div>`;
        }).join('');
  el.querySelectorAll('.deja-row').forEach(row => row.onclick = () => openSession(row.dataset.file));
}

// ---------- HOOK PROPOSALS (the honest version of enforcement) ----------
// WHY: a standing order is words in a guidance file — an agent reads it and may or
// may not comply. A hook is the version the machine itself runs. So this panel
// proposes hooks, but only where this fleet's own numbers justify one, and it prints
// the number beside the proposal so the reasoning can be checked instead of trusted.
// It also prints the ones it REFUSED to propose and why, because a panel that only
// ever agrees with itself is an advertisement.
// DISPLAY AND COPY ONLY, PERMANENTLY. There is no install button here and there
// never will be. A guidance file holds words an agent reads; a settings file holds
// commands the machine RUNS, so a dashboard that could plant one remotely or in bulk
// would be remote code execution with a friendly face. The owner opens the file in
// the Brain tab, reads the line, and saves it themselves.
// A HOOK NOBODY CAN OVERRIDE GETS SWITCHED OFF FOREVER, so every blocking hook here
// carries a written escape hatch, and the escape hatch is part of the proposal.
const HP_MIN_JS_SESSIONS = 5;   // sessions that edited JavaScript before any share is quoted
const HP_MIN_UNDOS = 3;         // undo moments before "ask first" is a pattern and not one bad afternoon
const HP_MIN_NOMODEL = 10;      // spawns with no model recorded before a guard is worth blocking on
const HP_NOMODEL_SHARE = 0.02;  // ...and it has to be at least this much of the fleet, not a rounding error
const HP_MIN_FLEET_COST = 5;    // never quote a spend percentage off trivial money
const HP_MIN_LEAKS = 2;         // a BLOCKING hook needs more than one finding, which may be a fixture
const HP_WRITE = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit']);
const HP_JS = /\.(?:js|mjs|cjs)(?:["'\s]|$)/i;
// What a machine says when JavaScript doesn't parse. Only ever matched against a
// TOOL RESULT — the assistant merely saying the words "syntax error" proves nothing.
const HP_SYNTAX = /SyntaxError|Unexpected token|Unexpected identifier|Unexpected end of input|Invalid or unexpected token/;

// The commands themselves. Node is the interpreter on purpose: this dashboard
// already requires it, it is the same on Windows and everywhere else, and the whole
// check is visible in the one line the owner pastes — nothing to install, nothing
// hidden in a script file somewhere. All three were run for real against sample
// payloads under both bash and cmd.exe before being offered.
// HP_CMD_UNDO mirrors the Graveyard's four commands and HP_CMD_SECRET mirrors
// LEAK_RULES in server.js. Both are deliberately SHORTER than the originals — a
// hook has to fit on one line, and neither reads quoting as carefully as the scan
// does. Each errs toward stopping too often rather than too rarely, which is the
// cheap mistake when there is an escape hatch one comment away.
const HP_CMD_JS = "node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let f='';try{f=(JSON.parse(s).tool_input||{}).file_path||''}catch(e){}if(!/\\.(js|mjs|cjs)$/i.test(f))process.exit(0);try{require('child_process').execFileSync(process.execPath,['--check',f],{stdio:'pipe'})}catch(e){console.error('That file does not parse as JavaScript right now: '+String(e.stderr||e.message));process.exit(2)}})\"";
const HP_CMD_UNDO = "node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let c='';try{c=(JSON.parse(s).tool_input||{}).command||''}catch(e){}if(c.indexOf('amc-ok')>=0)process.exit(0);const hit=c.split(/[;\\n]|&&|\\|\\|/).map(x=>x.trim()).find(x=>/^git\\s/.test(x)&&(/\\breset\\b.*--hard\\b/.test(x)||(/\\brevert\\b/.test(x)&&!/--(abort|continue|quit|skip)\\b/.test(x))||/\\bcheckout\\b.*\\s--(\\s|$)/.test(x)||(/\\brestore\\b/.test(x)&&!/--staged/.test(x))));if(!hit)process.exit(0);console.error('Stopped: that throws work away - '+hit+'. Check the Graveyard tab for what was thrown away here before. If you still mean it, add  # amc-ok  to the command.');process.exit(2)})\"";
const HP_CMD_SECRET = "node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let i={};try{i=JSON.parse(s).tool_input||{}}catch(e){}const t=[i.content,i.new_string].filter(x=>typeof x==='string').join('\\n');if(!t||t.indexOf('amc-ok')>=0)process.exit(0);const R=[[/\\b(?:AKIA|ASIA)[A-Z0-9]{16}\\b/,'an Amazon Web Services key'],[/\\bgh[pousr]_[A-Za-z0-9]{36}\\b/,'a GitHub token'],[/\\bxox[baprs]-[A-Za-z0-9]{10,48}-[A-Za-z0-9]{10,48}/,'a Slack token'],[/\\b[sr]k_live_[A-Za-z0-9]{24,64}\\b/,'a Stripe live payment key'],[/\\bAIza[A-Za-z0-9_-]{35}\\b/,'a Google API key'],[/-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/,'a private key']];for(const r of R)if(r[0].test(t)){console.error('Stopped: this write contains something shaped like '+r[1]+'. Put it in an environment variable and reference that instead. If it is a fake one for a doc or a test, add  amc-ok  on the same line.');process.exit(2)}})\"";

function hpHookJSON(event, matcher, command) {
  return JSON.stringify({ hooks: { [event]: [{ matcher, hooks: [{ type: 'command', command }] }] } }, null, 2);
}
// Where the settings file actually is on THIS computer, taken from the Brain tab's
// own list rather than guessed at from a home-directory pattern.
let hpSettings = null;
let hpProps = null;
async function loadHookProps() {
  const pane = $('hookprops');
  pane.innerHTML = '<div class="fleet-loading">Checking which hooks your own numbers would actually justify…</div>';
  try {
    if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
    if (!flowsCache) await loadFlows.fetchOnly();
  } catch (e) {
    pane.innerHTML = `<div class="fleet-head"><h2>Suggested hooks</h2></div><div class="uw-note">Couldn’t read your fleet history just now, so there is no evidence to base a suggestion on — and a hook suggested without evidence is exactly what this panel refuses to do. ${esc(String(e && e.message || ''))}</div>`;
    return;
  }
  if (!leaksData) { try { leaksData = await (await fetch('/api/leaks')).json(); } catch { leaksData = null; } }
  if (!graveData) { try { graveData = await (await fetch('/api/graveyard')).json(); } catch { graveData = null; } }
  if (!brainItems.length) { try { brainItems = (await (await fetch('/api/brain')).json()).items || []; } catch { brainItems = []; } }
  hpSettings = brainItems.find(i => /settings\.json/i.test(i.name) && /hooks/i.test(i.category || '')) || brainItems.find(i => /settings\.json/i.test(i.name)) || null;
  renderHookProps();
}

// Build every candidate from real numbers, and sort each one into "proposed" or
// "not proposed" by its own gate. A candidate that fails its gate still appears —
// with the number that failed it — because that is the interesting half.
function hookProposals() {
  const props = [], refused = [];
  const flows = flowsCache || [];
  const fleet = fleetCache || [];

  // 1. JavaScript that stopped parsing after an edit -------------------------
  const jsSessions = [];
  for (const { s, d } of flows) {
    const ev = d.events || [];
    let first = -1;
    for (let i = 0; i < ev.length; i++) {
      const e = ev[i];
      if (e.kind === 'tool-call' && HP_WRITE.has(e.tool) && HP_JS.test(String(e.full || e.text || '') + ' ')) { first = i; break; }
    }
    if (first < 0) continue;
    // only AFTER the first JavaScript edit — a syntax error before one was never
    // this hook's to catch
    // ...and the error must actually be about a JavaScript file. A Python or JSON
    // parse error is real, but it is not evidence that a JAVASCRIPT parse hook
    // would have caught anything — that inflates the case for the hook being sold.
    const n = ev.slice(first).filter(e => {
      if (e.kind !== 'tool-result') return false;
      const t = String(e.full || e.text || '');
      return HP_SYNTAX.test(t) && HP_JS.test(t + ' ');
    }).length;
    jsSessions.push({ s, n });
  }
  const jsBad = jsSessions.filter(x => x.n > 0);
  const jsHits = jsBad.reduce((n, x) => n + x.n, 0);
  if (jsSessions.length >= HP_MIN_JS_SESSIONS && jsBad.length) {
    props.push({
      id: 'js-check', event: 'PostToolUse', matcher: 'Edit|Write|MultiEdit', blocking: false,
      title: 'Check a JavaScript file still parses, the moment it’s edited',
      why: `<b>${jsBad.length} of the ${jsSessions.length}</b> recent sessions that edited a JavaScript file hit a JavaScript syntax error afterwards — <b>${jsHits}</b> ${jsHits === 1 ? 'time' : 'times'} in all, counted only from what a tool actually reported, never from an agent saying so.`,
      caught: jsBad.sort((a, b) => b.n - a.n).map(x => ({ file: x.s.file, label: `${trunc(x.s.title || x.s.session || '', 38)} · ${x.n}×` })),
      caughtLabel: 'Sessions it would have spoken up in',
      plain: [
        'Runs after every file edit, and does nothing at all unless the file ends in .js, .mjs or .cjs.',
        'Asks Node the same question you would: does this file still parse?',
        'If it doesn’t, the agent is told immediately — instead of finding out three steps later.',
      ],
      escape: 'None needed: this runs AFTER the edit is already saved, so it can never block your work. The worst it can do is tell the agent something is broken.',
      command: HP_CMD_JS,
    });
  } else {
    refused.push({
      title: 'Check a JavaScript file still parses after it’s edited',
      why: jsSessions.length < HP_MIN_JS_SESSIONS
        ? `Only ${jsSessions.length} of the sessions read edited a JavaScript file — not enough runs to say yet (this one needs ${HP_MIN_JS_SESSIONS}).`
        : `${jsSessions.length} sessions edited JavaScript and none of them hit a syntax error afterwards. Nothing here to catch.`,
    });
  }

  // 2. Ask before throwing work away ----------------------------------------
  const gy = graveData || {};
  const undos = (gy.moments || []).length;
  if (undos >= HP_MIN_UNDOS) {
    props.push({
      id: 'undo-guard', event: 'PreToolUse', matcher: 'Bash', blocking: true,
      title: 'Say something before an agent throws work away',
      why: `<b>${undos}</b> ${undos === 1 ? 'time' : 'times'} in the transcripts stored on this computer, an agent ran one of the four commands that discard work, across ${gy.repos || 1} project folder${(gy.repos || 1) === 1 ? '' : 's'}. Every one of them is listed in the Graveyard, with the moment it happened.`,
      caught: (gy.moments || []).map(m => ({ file: m.file, seq: m.seq, label: `${graveDate(m.ts)} · ${trunc(m.command, 44)}` })),
      caughtLabel: 'The commands it would have stopped',
      plain: [
        'Runs before a shell command, and ignores everything that isn’t one of those four git commands.',
        'When it sees one, the command doesn’t run: the agent is told what it was about to throw away and pointed at the Graveyard.',
        'It is deliberately a little trigger-happy — it doesn’t read quotes as carefully as the Graveyard does, so it may stop a command that merely mentions one of those. Stopping and asking is the cheap mistake here.',
      ],
      escape: 'Add  # amc-ok  anywhere in the command and it runs untouched. That is the whole point: a rule nobody can get past once is a rule that gets switched off for good.',
      note: 'None of those ' + undos + ' moments was necessarily wrong — throwing an edit away is often exactly right. This hook does not forbid it, it makes it deliberate.',
      command: HP_CMD_UNDO,
    });
  } else {
    refused.push({
      title: 'Say something before an agent throws work away',
      why: gy.error ? 'The Graveyard scan could not run just now, so there is no evidence either way.'
        : `Only ${undos} moment${undos === 1 ? '' : 's'} like that in everything this computer can read — below the ${HP_MIN_UNDOS} it would take to call it a habit rather than one afternoon.`,
    });
  }

  // 3. Stop a key being written into a file ---------------------------------
  const lk = leaksData || {};
  const finds = (lk.findings || []).length;
  // Every sibling proposal has a minimum before it is offered; this one had none,
  // so a single finding — quite possibly a test fixture — could talk you into a
  // BLOCKING hook. The one that stops your work needs the most evidence, not the least.
  if (finds >= HP_MIN_LEAKS) {
    props.push({
      id: 'secret-guard', event: 'PreToolUse', matcher: 'Edit|Write|MultiEdit', blocking: true,
      title: 'Stop something key-shaped from being written into a file',
      why: `The key scanner found <b>${finds}</b> thing${finds === 1 ? '' : 's'} worth a look in the ${lk.filesRead || 0} files your recent agents wrote — see the Secrets tab. This is the same short list of shapes, checked before the write instead of after.`,
      caught: (lk.findings || []).map(f => ({ file: f.sessionFile, label: `${f.kind} · ${f.path.split(/[\\/]/).pop()}` })),
      caughtLabel: 'What it would have stopped',
      plain: [
        'Runs before a file is written or edited, and looks only at the text being written.',
        'Recognises the same handful of key shapes the Secrets tab does — the ones whose vendor prefix is unmistakable. It will miss anything subtler, on purpose.',
        'If it matches, the write does not happen and the agent is told to use an environment variable instead.',
      ],
      escape: 'Put  amc-ok  on the same line and the write goes through — documentation and test fixtures are full of correctly-shaped fakes.',
      command: HP_CMD_SECRET,
    });
  } else {
    refused.push({
      title: 'Stop something key-shaped from being written into a file',
      why: finds
        ? `Only ${finds} finding across the ${lk.filesRead || 0} file${lk.filesRead === 1 ? '' : 's'} your agents wrote — and a single hit is as likely to be a test fixture as a real key. A hook that blocks your work needs more evidence than that (this one needs ${HP_MIN_LEAKS}). Look at it in the Secrets tab first.`
        : `Nothing to justify it: the key scanner read ${lk.filesRead || 0} file${lk.filesRead === 1 ? '' : 's'} your agents wrote and none matched a key shape. Proposing a blocking hook off zero findings would be generic advice dressed up as evidence.`,
    });
  }

  // 4. Every subagent must name its model -----------------------------------
  // This is the one the fleet's headline number LOOKS like it justifies and does
  // not, which is exactly why it is worth printing.
  let agents = 0, noModel = 0, cost = 0, top = 0;
  for (const s of fleet) {
    agents += s.agents || 0;
    noModel += s.agentsNoModel || 0;
    cost += s.cost || 0;
    const mix = s.tierMix || {};
    top += (mix.flagship || 0) + (mix.premium || 0);
  }
  const share = agents ? noModel / agents : 0;
  const topPct = cost >= HP_MIN_FLEET_COST ? Math.round(top / cost * 100) : null;
  if (noModel >= HP_MIN_NOMODEL && share >= HP_NOMODEL_SHARE) {
    props.push({
      id: 'model-guard', event: 'PreToolUse', matcher: 'Task', blocking: true,
      title: 'Refuse a subagent that never says which model to run',
      why: `<b>${noModel} of your ${agents}</b> recorded agents ran with no model in the transcript at all${topPct === null ? '' : `, while ${topPct}% of ~${fmtUsd(cost)} went to the top two tiers`}. An unnamed model inherits the orchestrator's, and inheriting is where the money leaks.`,
      caught: [], caughtLabel: '',
      plain: [
        'Runs before a subagent is started and reads the call’s settings.',
        'If no model is named, the spawn does not happen and the agent is told to pick one.',
      ],
      escape: 'Put  amc-ok  in the subagent’s prompt when inheriting really is what you want.',
      command: "node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let i={};try{i=JSON.parse(s).tool_input||{}}catch(e){}const t=JSON.stringify(i);if(/\\\"model\\\"/.test(t)||t.indexOf('amc-ok')>=0)process.exit(0);console.error('Stopped: this subagent names no model, so it inherits the orchestrator one. Name a model, or put  amc-ok  in the prompt.');process.exit(2)})\"",
    });
  } else {
    refused.push({
      title: 'Refuse a subagent that never says which model to run',
      why: `Your transcripts record a model for <b>${agents - noModel} of ${agents}</b> agents, so this hook would have stopped ${noModel} ${noModel === 1 ? 'spawn' : 'spawns'} in your whole recorded history.`
        + (topPct === null ? '' : ` Your top-tier spend is real — <b>${topPct}% of ~${fmtUsd(cost)}</b> — but it is not coming from models nobody set. It is coming from models that were named, and were expensive.`)
        + ' A hook cannot fix that; it is a judgement about which work deserves which tier, which is what the tiering standing order is for.',
    });
  }

  return { props, refused, sessionsRead: flows.length, fleetSessions: fleet.length };
}

const HP_CAUGHT_MAX = 6;   // examples shown before "+N more" — evidence, not a roster
function hpCaughtHTML(p) {
  if (!p.caught || !p.caught.length) return '';
  const shown = p.caught.slice(0, HP_CAUGHT_MAX);
  const over = p.caught.length - shown.length;
  return `<div class="hp-caught"><span class="dim">${esc(p.caughtLabel)}:</span>` +
    shown.map(c => `<span class="hp-hit" data-file="${esc(c.file)}"${c.seq == null ? '' : ` data-seq="${c.seq}"`} title="open this session">${esc(c.label)}</span>`).join('') +
    (over > 0 ? `<span class="dim">+${over} more</span>` : '') +
    '</div>';
}
function renderHookProps() {
  const pane = $('hookprops');
  if (!pane) return;
  hpProps = hookProposals();
  const { props, refused } = hpProps;
  const where = hpSettings ? hpSettings.path : 'settings.json in your .claude folder';

  const cards = props.map((p, i) => `<div class="hp-card">
      <div class="hp-h"><span class="hp-ev" title="${esc(HOOK_EVENT_HELP[p.event] || 'lifecycle event')}">${esc(p.event)}</span><b>${esc(p.title)}</b>${p.blocking ? '<span class="hp-block" title="this one stops the action">stops the action</span>' : '<span class="hp-warn-tag" title="this one only reports back">reports back only</span>'}</div>
      <div class="hp-why"><b>Why this one, on your numbers:</b> ${p.why}</div>
      ${p.note ? `<div class="hp-note">${esc(p.note)}</div>` : ''}
      ${hpCaughtHTML(p)}
      <ul class="hp-plain">${p.plain.map(l => `<li>${esc(l)}</li>`).join('')}</ul>
      <div class="hp-escape"><b>Way past it:</b> ${esc(p.escape)}</div>
      <div class="hp-where"><b>Where it goes:</b> <code>${esc(where)}</code> — inside the <code>hooks</code> section, under <code>${esc(p.event)}</code>. If that section already exists, paste only the <code>"${esc(p.event)}"</code> part into it; the Brain tab shows you what is in there now.</div>
      <div class="hp-jsonwrap"><pre class="hp-json">${esc(hpHookJSON(p.event, p.matcher, p.command))}</pre>
        <div class="hp-acts"><button class="mini-btn hp-copy" data-i="${i}">📋 copy this</button><button class="mini-btn hp-brain">🧠 open the file in Brain</button></div></div>
    </div>`).join('');

  const refusedHTML = refused.map(r => `<div class="hp-refused"><b>${esc(r.title)}</b><span>${r.why}</span></div>`).join('');

  pane.innerHTML =
    `<div class="fleet-head"><h2>Hook ideas — ${props.length ? `${props.length} backed by your own numbers` : 'none your numbers back yet'}</h2>
      <span class="dim">Weighed against ${hpProps.sessionsRead} session${hpProps.sessionsRead === 1 ? '' : 's'} read in full, out of ${hpProps.fleetSessions} in your fleet.</span>${homeButton('hookprops')}</div>
     <div class="uw-note">A standing order is <b>words an agent reads</b> and may or may not follow. A hook is the version <b>your computer runs</b>, every time, whether the agent likes it or not. That is the whole difference, and it is why this page hands you text and nothing else.
       <br><b>Agent Mission Control will never write a hook for you</b> — not with a button here, not in bulk, not from another machine. Guidance files hold words; settings files hold commands that execute. A dashboard that could plant those would be a way to run code on your computer with a friendly face on it. So: you copy the line, you open the file in the Brain tab, you read it, you save it.
       <br>Every proposal below shows the number that earned it. The ones that <b>didn’t</b> earn it are listed at the bottom with the number that failed them — those are the honest half.
       <br><span class="dim">One more thing worth knowing: the evidence spans every machine that relays into this dashboard, but the settings file is <b>this computer’s</b>. A hook only guards the machine it is saved on.</span></div>
     ${cards || '<div class="uw-empty">Nothing in your fleet’s numbers justifies a hook right now. That is a good result, not an empty page — see below for what was checked and what the numbers said.</div>'}
     <div class="hp-sec">Checked, and <b>not</b> proposed</div>
     ${refusedHTML}`;

  wireHomeButton(pane, 'hookprops', renderHookProps);
  pane.querySelectorAll('.hp-copy').forEach(b => b.onclick = () => {
    const p = props[+b.dataset.i]; if (!p) return;
    navigator.clipboard.writeText(hpHookJSON(p.event, p.matcher, p.command));
    const o = b.textContent; b.textContent = '✓ copied'; setTimeout(() => { b.textContent = o; }, 1500);
  });
  // navigation only — this opens the file for the owner to read, it does not touch it
  pane.querySelectorAll('.hp-brain').forEach(b => b.onclick = () => { state.view = 'brain'; setTabs(); });
  pane.querySelectorAll('.hp-hit[data-file]').forEach(el => el.onclick = () => {
    const seq = el.dataset.seq;
    if (seq == null) openSession(el.dataset.file); else openSessionAt(el.dataset.file, Number(seq));
  });
}

// ---------- overview nav bar (category dropdowns) ----------
// The 17 overview views used to be one flat row of tabs and overflowed the header.
// Markup + dispatch are both driven off NAV_MENUS/VIEW_META (declared up top next
// to OVERVIEW) so a view only needs adding in one place. #navBar is rebuilt once at
// boot; setTabs() just toggles classes/labels on the existing elements each switch.
// ---------- ECONOMICS view (where the money actually goes) ----------
// This view exists because these exact numbers were measured by hand in scratch
// scripts, written into a standing order, and immediately began to go stale.
// The findings it encodes: most spend is context handling rather than output;
// cost per turn is U-shaped in agent lifetime (briefing cost amortizes while
// re-reading compounds); one-shot subagents pay a full briefing and die before
// earning it back. Every judgment below is gated on sample size — a rate with
// too few runs behind it renders as raw counts, never as a confident verdict.
const ECON_BUCKET_LABELS = ['1–2', '3–5', '6–10', '11–20', '21–40', '41–80', '81–160', '161+'];  // must match ECON_BUCKETS in server.js
const ECON_MIN_AGENTS = 5;      // per-bucket floor before a cost/turn is shown as a rate
const ECON_MIN_SPEND = 1;       // dollars, before percentages of spend mean anything

async function loadEconomics() {
  if (!fleetCache) fleetCache = await (await fetch('/api/fleet')).json();
  renderEconomics();
}

function renderEconomics() {
  const data = fleetCache || [];
  // sum the per-session aggregates the server already computed
  const life = ECON_BUCKET_LABELS.map(() => ({ n: 0, turns: 0, cost: 0 }));
  const split = { fresh: 0, read: 0, write: 0, out: 0 };
  let oneShotCost = 0, subs = 0, codexSessions = 0, unpricedAgents = 0;
  for (const s of data) {
    const e = s.econ;
    if (s.kind === 'codex') codexSessions++;
    if (!e) continue;
    // The dollar lanes take every session — a dollar is a dollar. The lifetime
    // curve takes CLAUDE sessions only: there a turn is one assistant reply, a
    // comparable unit. Codex counts token events (thousands per agent) and bills
    // at different rates, so mixing it in once crowned a 4,300-'turn' Codex agent
    // the cheapest work on the fleet. It was neither cheap nor 4,300 turns.
    if (e.split) { split.fresh += e.split.fresh; split.read += e.split.read; split.write += e.split.write; split.out += e.split.out; }
    if (s.kind !== 'claude') continue;
    for (let i = 0; i < life.length; i++) {
      const b = (e.life || [])[i];
      if (b) { life[i].n += b.n; life[i].turns += b.turns; life[i].cost += b.cost; }
    }
    oneShotCost += e.oneShotCost || 0;
    subs += e.subs || 0;
    unpricedAgents += e.unpricedAgents || 0;
  }
  const total = split.fresh + split.read + split.write + split.out;
  const ctx = split.read + split.write;
  const pct = n => total >= ECON_MIN_SPEND ? Math.round(n / total * 100) + '%' : '—';

  // the four billing lanes, plain-language first
  const lanes = [
    { k: 'read', label: 'Re-reading context', hint: 'everything already said, re-read on every turn (cache reads, 0.1× rate)', v: split.read, cls: 'ec-read' },
    { k: 'write', label: 'Storing context', hint: 'briefings and new material written into the cache (1.25× rate)', v: split.write, cls: 'ec-write' },
    { k: 'out', label: 'Actual output', hint: 'the words and code the models produced — the thing being paid for', v: split.out, cls: 'ec-out' },
    { k: 'fresh', label: 'Fresh input', hint: 'brand-new prompt text billed at the full input rate', v: split.fresh, cls: 'ec-fresh' },
  ].sort((a, b) => b.v - a.v);
  const maxLane = Math.max(...lanes.map(l => l.v), 0.01);

  // U-curve: cost per turn per lifetime bucket, gated per bucket
  const rows = life.map((b, i) => ({
    label: ECON_BUCKET_LABELS[i], n: b.n, turns: b.turns, cost: b.cost,
    perTurn: b.turns ? b.cost / b.turns : null, gated: b.n < ECON_MIN_AGENTS,
  }));
  const shown = rows.filter(r => !r.gated && r.perTurn !== null);
  const maxPer = Math.max(...shown.map(r => r.perTurn), 0.001);
  const best = shown.length >= 2 ? shown.reduce((a, b) => (a.perTurn <= b.perTurn ? a : b)) : null;

  // plain-language findings, each earned by its own gate
  const findings = [];
  if (total >= ECON_MIN_SPEND) {
    findings.push(`<b>${pct(ctx)} of spend is moving context around</b> — re-reading and storing what was already said — versus ${pct(split.out)} on actual output. Shorter briefs and shorter agent lifetimes attack the whole ${fmtUsd(ctx)}; a cheaper model only rescales it.`);
  }
  if (best && shown.length >= 3) {
    const worst = shown.reduce((a, b) => (a.perTurn >= b.perTurn ? a : b));
    if (worst.perTurn > best.perTurn * 1.5) {
      findings.push(`<b>The cheapest work happens in agents that live ${best.label} turns</b> (~${fmtUsd(best.perTurn)}/turn). Agents in the ${worst.label} bucket cost ${(worst.perTurn / best.perTurn).toFixed(1)}× as much per turn.`);
    }
  }
  if (rows[0].n >= ECON_MIN_AGENTS && rows[0].perTurn && best && rows[0] !== best && rows[0].perTurn > (best.perTurn || 0) * 2) {
    findings.push(`<b>One-shot agents are the worst value here</b>: ${rows[0].n} agents that lived 1–2 turns spent ${fmtUsd(oneShotCost)}, paying a full briefing each and quitting before it paid off. A single grep or read is cheaper done inline.`);
  }
  const tail = rows[rows.length - 1];
  if (tail.n >= ECON_MIN_AGENTS && tail.cost > total * 0.4) {
    findings.push(`<b>Most subagent money is in the long tail</b>: agents living ${tail.label} turns spent ${fmtUsd(tail.cost)}. Splitting long-runners costs a briefing or two and typically saves several times that in re-reads.`);
  }
  if (!findings.length) findings.push('Not enough runs to say anything with confidence yet — this page fills in as the fleet works.');

  $('economics').innerHTML = `
    <div class="fleet-head"><h2>Economics — where the money actually goes</h2>
      <div style="display:flex;gap:8px">${homeButton('economics')}<button id="econRefresh" class="mini-btn">↻ refresh</button></div></div>
    <div class="dim" style="margin-bottom:12px">Computed live from every parsed session on every machine, using the same corrected accounting as the cost figures elsewhere. All dollar figures are estimates.</div>

    <div class="usage-tiles" style="margin-bottom:14px">
      <div class="utile"><div class="ul">Attributed spend</div><div class="uv accent">~${fmtUsd(total)}</div></div>
      <div class="utile"><div class="ul">Context handling</div><div class="uv">${pct(ctx)}</div></div>
      <div class="utile"><div class="ul">Actual output</div><div class="uv">${pct(split.out)}</div></div>
      <div class="utile"><div class="ul">Claude subagents measured</div><div class="uv">${subs.toLocaleString()}</div></div>
      <div class="utile"><div class="ul">One-shot spend</div><div class="uv">${oneShotCost >= 0.01 ? '~' + fmtUsd(oneShotCost) : '—'}</div></div>
    </div>

    <div class="flows-panel" style="margin-bottom:14px">
      <h3>Where each dollar goes <span class="qi" title="Every model bill has four lanes. Two of them are just carrying the conversation itself back and forth — that is usually where almost all the money is.">ⓘ</span></h3>
      ${lanes.map(l => `
        <div class="ec-lane" title="${esc(l.hint)}">
          <span class="ec-label">${l.label}</span>
          <span class="ec-track"><span class="ec-bar ${l.cls}" style="width:${Math.max(1.5, l.v / maxLane * 100)}%"></span></span>
          <span class="ec-val">~${fmtUsd(l.v)}</span><span class="ec-pct dim">${pct(l.v)}</span>
        </div>`).join('')}
    </div>

    <div class="flows-panel" style="margin-bottom:14px">
      <h3>Cost per turn, by how long an agent lives <span class="dim" style="font-weight:400;font-size:11.5px">Claude agents only — a turn is one reply; other engines count work differently</span> <span class="qi" title="Short-lived agents pay a full briefing and quit before it pays off. Long-lived ones re-read an ever-growing history every turn. The cheap zone is in the middle.">ⓘ</span></h3>
      <div class="econ-grid">
        <div class="eg-head"><span>Agent lifetime</span><span>Agents</span><span>Total spent</span><span>Cost per turn</span><span></span></div>
        ${rows.map(r => r.gated
          ? `<div class="eg-row eg-gated" title="only ${r.n} agent${r.n === 1 ? '' : 's'} in this range so far — not enough to call a rate"><span>${r.label} turns</span><span>${r.n || '—'}</span><span>${r.cost ? '~' + fmtUsd(r.cost) : '—'}</span><span class="dim">${r.n ? 'too few to say' : 'none yet'}</span><span></span></div>`
          : `<div class="eg-row${best && r.label === best.label ? ' eg-best' : ''}"><span>${r.label} turns${best && r.label === best.label ? ' ★' : ''}</span><span>${r.n}</span><span>~${fmtUsd(r.cost)}</span><span>~${fmtUsd(r.perTurn)}</span><span class="ec-track"><span class="ec-bar ec-curve" style="width:${Math.max(2, r.perTurn / maxPer * 100)}%"></span></span></div>`).join('')}
      </div>
      ${best ? `<div class="dim" style="margin-top:8px">★ the sweet spot — enough turns to absorb the briefing, not so many that re-reading takes over.</div>` : ''}
    </div>

    <div class="flows-panel">
      <h3>What this means</h3>
      ${findings.map(f => `<div class="econ-finding">${f}</div>`).join('')}
      ${codexSessions ? `<div class="dim" style="margin-top:10px">Honesty note: ${codexSessions} Codex session${codexSessions === 1 ? ' is' : 's are'} costed from a partial read of very large files, so their share is undercounted here.</div>` : ''}
      ${unpricedAgents ? `<div class="dim" style="margin-top:4px">${unpricedAgents.toLocaleString()} agent${unpricedAgents === 1 ? '' : 's'} ran on models this tool has no price for — they are left out of the cost-per-turn table rather than shown as free.</div>` : ''}
    </div>`;

  $('econRefresh').onclick = async () => { fleetCache = await (await fetch('/api/fleet')).json(); renderEconomics(); };
  wireHomeButton($('economics'), 'economics', renderEconomics);
}

const VIEW_LOADERS = {
  fleet: loadFleet, table: loadTable, fingerprints: loadFingerprints, calendar: loadCalendar,
  rings: loadRings, rhythm: loadRhythm, projects: loadProjects, usage: loadUsage, flows: loadFlows,
  playbooks: loadPlaybooks, brain: loadBrain, audit: loadAudit, constellation: loadConstellation,
  machines: loadMachines, unsaved: loadUnsaved, trouble: loadTrouble, leaks: loadLeaks,
  divergence: loadDivergence, graveyard: loadGraveyard, hookprops: loadHookProps, dejavu: loadDejaVu,
  economics: loadEconomics,
};
let openNavMenu = null;
function closeNavMenus() {
  if (!openNavMenu) return;
  openNavMenu = null;
  document.querySelectorAll('.navgrp-btn').forEach(b => b.setAttribute('aria-expanded', 'false'));
  document.querySelectorAll('.navgrp-panel').forEach(p => p.classList.remove('open'));
}
function toggleNavMenu(key) {
  const opening = openNavMenu !== key;
  closeNavMenus();
  if (!opening) return;
  openNavMenu = key;
  $('navBtn-' + key)?.setAttribute('aria-expanded', 'true');
  $('navPanel-' + key)?.classList.add('open');
}
function updateNavHome() {
  const id = getHomeView() || 'fleet';
  const b = $('navHome');
  if (b) { b.textContent = '⌂ ' + VIEW_META[id].label; b.dataset.view = id; }
}
function renderNavBar() {
  const bar = $('navBar');
  if (!bar) return;
  bar.innerHTML = `<button id="navHome" class="nav-home" title="Your home view — one click, always here"></button>` +
    NAV_MENUS.map(m => `
      <div class="navgrp" id="navgrp-${m.key}">
        <button class="navgrp-btn" id="navBtn-${m.key}" type="button" aria-haspopup="true" aria-expanded="false">${esc(m.label)} <span class="navgrp-car">▾</span></button>
        <div class="navgrp-panel" id="navPanel-${m.key}" role="menu">
          ${m.views.map(v => `<button class="navitem" id="navitem-${v}" data-view="${v}" type="button" role="menuitem">${VIEW_META[v].icon ? esc(VIEW_META[v].icon) + ' ' : ''}${esc(VIEW_META[v].label)}</button>`).join('')}
        </div>
      </div>`).join('');
  updateNavHome();
  bar.addEventListener('click', e => {
    const item = e.target.closest('.navitem');
    if (item) { closeNavMenus(); if (!BAKED) { state.view = item.dataset.view; setTabs(); } return; }
    const grpBtn = e.target.closest('.navgrp-btn');
    if (grpBtn) { e.stopPropagation(); toggleNavMenu(grpBtn.id.slice('navBtn-'.length)); return; }
    if (e.target.closest('#navHome')) { if (!BAKED) { state.view = getHomeView() || 'fleet'; setTabs(); } }
  });
}
document.addEventListener('click', closeNavMenus);

// ---------- render ----------
// (OVERVIEW/NAV_MENUS/VIEW_META are declared near `state` above)
function setTabs() {
  updateNavHome();
  $('navHome')?.classList.toggle('on', state.view === ($('navHome').dataset.view || 'fleet'));
  for (const m of NAV_MENUS) {
    $('navgrp-' + m.key)?.classList.toggle('has-active', m.views.includes(state.view));
    for (const v of m.views) $('navitem-' + v)?.classList.toggle('on', state.view === v);
  }
  for (const [btn, v] of [['viewBoard', 'board'], ['viewStory', 'story'], ['viewLanes', 'lanes'], ['viewWaterfall', 'waterfall'], ['viewCost', 'costflow'], ['viewTimeline', 'timeline']]) {
    const el = $(btn); if (el) el.classList.toggle('on', state.view === v);
  }
  const overview = OVERVIEW.includes(state.view);
  document.querySelector('main').classList.toggle('no-feed', overview);
  $('empty').style.display = 'none'; // only board/timeline turn it back on
  for (const v of OVERVIEW) $(VIEW_META[v].pane).style.display = (state.view === v) ? '' : 'none';
  if (overview) for (const p of SESSION_PANES) $(p).style.display = 'none';
  $('feed').style.display = overview ? 'none' : '';
  document.querySelector('footer').style.display = overview ? 'none' : '';
  $('statbar').style.display = overview ? 'none' : '';
  $('liveNowStrip').style.display = overview ? '' : 'none';
  renderLiveNowStrip(); // paint from whatever's cached now; the view's own load()/poll keep it fresh
  renderStickbar(); // hides itself on the overview tabs
  renderFailbar(); // ditto
  stopConstellation();
  VIEW_LOADERS[state.view]?.();
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
  const cacheW = a.reduce((n, x) => n + (x.cacheWriteTokens || 0), 0);
  const outT = a.reduce((n, x) => n + (x.outTokens || 0), 0);
  const cost = a.reduce((n, x) => n + (x.cost || 0), 0);
  const toolCalls = evs.filter(e => e.kind === 'tool-call' || e.kind === 'spawn').length;
  const errs = evs.filter(e => e.error).length;
  const first = evs.find(e => e.ts), last = [...evs].reverse().find(e => e.ts);
  const dur = first && last ? new Date(last.ts) - new Date(first.ts) : 0;
  $('statbar').innerHTML =
    `<span>agents <b>${a.length}</b></span><span>events <b>${evs.length}</b></span>` +
    `<span>tool calls <b>${toolCalls}</b></span><span>duration <b>${fmtDur(dur)}</b></span>` +
    `<span>tokens in <b>${fmtTok(inT)}</b> · cache read <b>${fmtTok(cacheT)}</b> · cache write <b>${fmtTok(cacheW)}</b> · out <b>${fmtTok(outT)}</b></span>` +
    `<span>est. cost <b>~${fmtUsd(cost)}</b></span>` +
    // A transcript too large to read whole is shown from its most recent part only.
    // Say so on the same line as the numbers it affects — a silent partial read is
    // exactly the kind of confident-looking wrong answer this tool must not give.
    (state.data.truncated
      ? `<span class="trunc-warn" title="This session's transcript is ${fmtBytes(state.data.truncated.totalBytes)}, too large to read in full. Everything above is counted from the most recent ${fmtBytes(state.data.truncated.readBytes)} only, so the real totals are higher.">⚠ partial — newest ${fmtBytes(state.data.truncated.readBytes)} of ${fmtBytes(state.data.truncated.totalBytes)}</span>`
      : '') +
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
renderNavBar(); // builds #navHome + the four category dropdowns from NAV_MENUS
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
  $('navBar').style.display = 'none'; // hides home button + all four menus in one shot — they're the only children NAV_MENUS drives
  $('exportBtn').style.display = 'none';
  $('liveBtn').style.display = 'none';
  $('liveDot').className = 'dot'; $('liveLabel').textContent = 'replay';
  setTabs(); render();
} else {
  loadSessions();
  loadMeta().then(() => setTabs());
  startNotifications();
  loadAppVersion();
  fetch('/api/machines').then(r => r.json()).then(renderMachineWarnBar).catch(() => { /* offline at boot */ });
}
