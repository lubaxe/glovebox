'use strict';

// ---------- helpers ----------

const $ = (sel, root = document) => root.querySelector(sel);

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'style') node.style.cssText = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', `#i-${name}`);
  svg.append(use);
  return svg;
}

async function api(path) {
  const res = await fetch(path);
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
  return res.json();
}

// send posts JSON and returns the reply; the car's own error text comes back
// as {"error": ...}.
async function send(method, path, body = {}) {
  const res = await fetch(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const text = await res.text();
  let data = {};
  try { data = JSON.parse(text); } catch { /* plain-text error */ }
  if (!res.ok) throw new Error(data.error || text.trim() || `HTTP ${res.status}`);
  return data;
}

// MEDIA_LAYOUT changes when the server presents recordings differently
// (2: index first), so browsers don't mix cached pieces of the old layout.
const MEDIA_LAYOUT = 2;

function mediaURL(file, download) {
  const p = file.path.split('/').map(encodeURIComponent).join('/');
  return `/media/${encodeURIComponent(file.snapshot)}/${p}?layout=${MEDIA_LAYOUT}${download ? '&download=1' : ''}`;
}

// A clip's keyframes only: 2 frames a second at full resolution, about a
// fifth of the bytes.
const keyframesURL = (file) => mediaURL(file) + '&keyframes=1';

const telemetryURL = (file) => '/api/telemetry/' + encodeURIComponent(file.snapshot) + '/' + file.path.split('/').map(encodeURIComponent).join('/');

// Stops a video's download and frees its decoder.
function unload(v) {
  v.pause();
  if (!v.getAttribute('src')) return;
  v.removeAttribute('src');
  v.load();
}

// The car records local time without a zone, e.g. "2026-10-15T00:39:00".
function carTime(s) {
  const [d, t] = s.split('T');
  const [y, mo, da] = d.split('-').map(Number);
  const [h, mi, se] = t.split(':').map(Number);
  return new Date(y, mo - 1, da, h, mi, se);
}
function fmtDay(d) {
  const today = new Date();
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return 'Today';
  if (d.toDateString() === yesterday.toDateString()) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
}
const fmtTime = (d) => d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
const fmtClock = (d) => d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
function gb(bytes) {
  if (bytes < 2 ** 30) return `${Math.max(1, Math.round(bytes / 2 ** 20))} MB`;
  return `${(bytes / 2 ** 30).toFixed(bytes < 10 * 2 ** 30 ? 1 : 0)} GB`;
}
// "in 40 s", "in 3 min": how long until date.
function inTime(date) {
  const s = Math.max(0, Math.round((date - Date.now()) / 1000));
  if (s < 60) return `in ${s} s`;
  return `in ${Math.round(s / 60)} min`;
}

// One line on what keep-awake is doing while it's active.
function keepAwakeLine(k) {
  const parts = [];
  if (k.since) parts.push(`started ${ago(new Date(k.since))}`);
  if (k.held_until) parts.push(`as asked, until ${new Date(k.held_until).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}`);
  else parts.push('for the backup');
  if (k.nudging) parts.push('nudging now');
  else if (k.next_wake) parts.push(new Date(k.next_wake) > Date.now() ? `next nudge ${inTime(new Date(k.next_wake))}` : 'nudging now');
  return parts.join(' · ');
}

function ago(date) {
  const s = Math.round((Date.now() - date) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} days ago`;
}
function uptime(sec) {
  const h = Math.floor(sec / 3600);
  return h >= 24 ? `${Math.floor(h / 24)} d ${h % 24} h` : `${h} h ${Math.floor((sec % 3600) / 60)} min`;
}

const REASONS = {
  sentry_aware_object_detection: 'Object detected',
  sentry_aware_accel: 'Car bumped',
  sentry_locked_handle_pulls: 'Door handle pulled',
  user_interaction_dashcam_icon_tapped: 'Saved from dashcam icon',
  user_interaction_dashcam_panel_save: 'Saved from dashcam panel',
  user_interaction_dashcam_launcher_action_tapped: 'Saved from launcher',
  user_interaction_honk: 'Saved by honking',
  vehicle_auto_emergency_braking: 'Emergency braking',
};
function reasonText(reason) {
  if (!reason) return '';
  if (REASONS[reason]) return REASONS[reason];
  const s = reason.replace(/^(sentry_aware_|user_interaction_)/, '').replace(/_/g, ' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

const CAMERA_ORDER = ['front', 'back', 'left_repeater', 'right_repeater', 'left_pillar', 'right_pillar'];
const CAMERA_NAMES = {
  front: 'Front', back: 'Rear', left_repeater: 'Left repeater', right_repeater: 'Right repeater',
  left_pillar: 'Left pillar', right_pillar: 'Right pillar',
};
function sortCameras(set) {
  const known = CAMERA_ORDER.filter((c) => set.has(c));
  const rest = [...set].filter((c) => !CAMERA_ORDER.includes(c)).sort();
  return known.concat(rest);
}

const KIND = {
  sentry: { name: 'Sentry', icon: 'shield', empty: 'No Sentry events yet', hint: 'When Sentry Mode records an event, it shows up here.' },
  saved: { name: 'Saved', icon: 'bookmark', empty: 'No saved clips yet', hint: 'Tap the dashcam icon in the car to save the last ten minutes.' },
};

// ---------- page frame ----------

let current = ''; // the view being shown, so status updates can refresh it

function page(name, title, ...nodes) {
  player.stop();
  current = name;
  document.title = title === 'Overview' ? 'Glovebox' : `${title} · Glovebox`;
  $('#title').textContent = title;
  for (const a of document.querySelectorAll('#nav a')) a.classList.toggle('active', a.dataset.nav === name.split(':')[0]);
  document.body.classList.toggle('viewer-active', nodes.some(node => node?.classList?.contains('cinema-viewer')));
  $('.content').classList.toggle('viewer-open', document.body.classList.contains('viewer-active'));
  $('#view').replaceChildren(...nodes);
  window.scrollTo(0, 0);
  $('.content').scrollTop = 0; // Mobile scrolls the content, independently of its fixed navigation.
}

function loading() { return el('p', { class: 'muted' }, 'Loading…'); }

function empty(iconName, title, text) {
  return el('div', { class: 'empty' }, icon(iconName), el('h3', {}, title), el('div', {}, text));
}

// ---------- status ----------

const status = { system: null, drive: null, archive: null, car: null, encryption: null };

// driveLocked: the drive is encrypted and waits for the car or the user's key.
const driveLocked = () => status.encryption?.encrypted && status.encryption.state === 'waiting';

async function refreshStatus() {
  const [st, dr, ar, car, enc] = await Promise.allSettled([api('/api/status'), api('/api/drive'), api('/api/archive'), api('/api/car'), api('/api/encryption')]);
  const wasLocked = driveLocked();
  status.system = st.status === 'fulfilled' ? st.value : null;
  status.drive = dr.status === 'fulfilled' ? dr.value : null;
  status.archive = ar.status === 'fulfilled' ? ar.value : null;
  status.car = car.status === 'fulfilled' ? car.value : null;
  status.encryption = enc.status === 'fulfilled' ? enc.value : null;
  renderChips();
  renderFoot();
  if (wasLocked !== driveLocked() && current !== 'settings' && current !== 'car') route();
  else if (current === 'locked') renderLocked();
  if (current === 'overview') renderOverviewStats();
  if (current === 'car' && status.car) renderCarStatus(status.car);
}

function chip(text, state, title, minor) {
  return el('span', { class: `chip ${state || ''} ${minor ? 'minor' : ''}`, title }, el('span', { class: 'dot' }), text);
}

function usbState() {
  if (driveLocked()) return { text: 'Drive locked', state: 'warn', detail: 'Encrypted: waiting for the car, or your key.' };
  const d = status.drive;
  if (!d) return { text: 'Drive off', state: 'bad', detail: 'The drive is disabled or failed to start.' };
  if (d.gadget.host_state === 'configured') return { text: 'Car connected', state: 'ok', detail: 'The car is using the drive.' };
  if (d.gadget.enabled) return { text: 'Waiting for car', state: 'warn', detail: 'The drive is ready; the car isn\'t using it.' };
  return { text: 'Drive off', state: 'bad', detail: 'The drive isn\'t presented over USB.' };
}

function nasState() {
  const a = status.archive;
  if (!a) return { text: 'Backup off', state: '', detail: 'NAS backup isn\'t set up.' };
  if (a.state === 'archiving') return { text: `Backing up ${a.done}/${a.total}`, state: 'warn', detail: a.current || '' };
  if (a.state === 'syncing') return { text: 'Syncing drives', state: 'warn', detail: `Copying music, light shows and Boombox sounds from ${a.target}` };
  if (a.state === 'home') return { text: 'Backed up', state: a.last?.error ? 'bad' : 'ok', detail: a.last?.error || `Everything is on ${a.target}.` };
  return { text: 'Away from home', state: '', detail: `${a.target} isn't reachable.` };
}

function renderChips() {
  const u = usbState();
  const n = nasState();
  const chips = [chip(u.text, u.state, u.detail)];
  if (status.system?.warnings?.length) chips.push(chip('Needs attention', 'bad', status.system.warnings.join('\n')));
  const nas = chip(n.text, n.state, `${n.detail}\nClick to look for new clips now.`);
  nas.classList.add('clickable');
  nas.setAttribute('role', 'button');
  nas.tabIndex = 0;
  const run = async () => {
    await fetch('/api/archive/run', { method: 'POST' });
    setTimeout(refreshStatus, 1500);
  };
  nas.addEventListener('click', run);
  nas.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); run(); }
  });
  chips.push(nas);
  const k = status.car?.keep_awake;
  if (k?.active) {
    chips.push(el('a', { class: 'chip live', href: '#/car', title: keepAwakeLine(k) }, el('span', { class: 'dot' }), 'Keeping car awake'));
  }
  const s = status.system?.system;
  if (s?.wifi_signal_dbm != null) chips.push(chip(`Wi-Fi ${s.wifi_signal_dbm} dBm`, s.wifi_signal_dbm < -75 ? 'warn' : '', 'Pi\'s Wi-Fi signal', true));
  $('#chips').replaceChildren(...chips);
}

function renderFoot() {
  const s = status.system;
  if (!s) return;
  $('#foot').replaceChildren(
    el('div', {}, s.system.hostname),
    el('div', {}, `version ${s.version}`),
    ...(s.warnings || []).map(warning => el('div', { class: 'error', role: 'alert' }, warning)),
    updateInfo?.available && updateInfo.latest && updateInfo.state === 'idle'
      ? el('a', { class: 'update-link', href: '#/settings/updates' }, `${updateInfo.latest.version} is available`) : '');
}

// ---------- overview ----------

function stat(cls, iconName, label, value, sub, extra, href) {
  return el(href ? 'a' : 'div', { class: `stat ${cls || ''}`, href },
    el('div', { class: 'stat-head' }, el('span', { class: 'stat-icon' }, icon(iconName)), label),
    el('div', { class: 'stat-value' }, value),
    extra || null,
    el('div', { class: 'stat-sub' }, sub));
}

function meter(fraction, cls) {
  return el('div', { class: `meter ${cls || ''}` }, el('span', { style: `width:${Math.max(2, Math.min(100, fraction * 100))}%` }));
}

function renderOverviewStats() {
  const box = $('#stats');
  if (!box) return;
  const cards = [];

  const u = usbState();
  cards.push(stat(u.state, 'usb', 'Dashcam drive', u.text,
    status.drive ? `${gb(status.drive.cam_size_bytes)} drive · ${status.drive.snapshots} snapshots` : u.detail));

  const a = status.archive;
  if (a) {
    const n = nasState();
    let sub = `${a.archived_files.toLocaleString()} files on ${a.target}`;
    let extra = null;
    if (a.state === 'archiving') {
      extra = meter(a.total ? a.done / a.total : 0, 'warn');
      sub = a.catching_up ? 'Catching up on the drive · keeping the car awake' : 'Copying new clips';
    } else if (a.last && !a.last.error && a.last.uploaded) {
      sub = `Last backup ${ago(new Date(a.last.started))}: ${a.last.uploaded} files, ${gb(a.last.bytes)}`;
    }
    cards.push(stat(n.state, 'cloud', 'NAS backup', n.text, sub, extra));
  } else {
    cards.push(stat('warn', 'cloud', 'NAS backup', 'Not set up', 'Tell Glovebox where to copy clips on the Settings page.', null, '#/settings'));
  }

  if (status.drive) {
    const d = status.drive;
    const s = d.store;
    if (s) {
      // minutes of driving deleted for space before they were backed up, in the last day
      const lost = s.dropped && Date.now() - new Date(s.dropped_at) < 864e5;
      cards.push(stat(lost ? 'warn' : '', 'disk', 'Kept drives',
        `${fmtHours(s.minutes / 60)} · ${gb(d.backingfiles_free_bytes)} free`,
        lost ? `${s.dropped} min deleted before backup` : null, null, '#/settings/storage'));
    } else {
      cards.push(stat('', 'disk', 'Snapshot space', `${gb(d.backingfiles_free_bytes)} free`,
        'Oldest cleared when low.'));
    }
    const extras = d.extras || [];
    if (extras.length) {
      const failed = extras.find((x) => x.last_error);
      const synced = extras.map((x) => x.last_sync).filter(Boolean).sort().pop();
      let sub = synced ? `Last synced ${ago(new Date(synced))}` : 'Synced when the car gets home';
      if (failed) sub = `${failed.label.toLowerCase()}: ${failed.last_error}`;
      cards.push(stat(failed ? 'warn' : '', 'music', 'Extra drives',
        extras.map((x) => `${x.label[0]}${x.label.slice(1).toLowerCase()} ${gb(x.size_bytes)}`).join(' · '), sub, null, '#/settings'));
    }
  }

  const c = status.car;
  if (c) {
    if (c.paired) {
      const k = c.keep_awake || {};
      if (k.active) {
        cards.push(stat(k.last_error ? 'warn' : 'live', 'bolt', 'Keep-awake', 'Keeping the car awake',
          k.last_error ? `Last try failed: ${k.last_error}` : keepAwakeLine(k), null, '#/car'));
      } else {
        let sub = 'Ready to keep the car awake while backing up';
        if (k.last_error) sub = `Last try failed: ${k.last_error}`;
        else if (k.last_wake) sub = `Last kept awake ${ago(new Date(k.last_wake))}`;
        cards.push(stat(k.last_error ? 'warn' : 'ok', 'bolt', 'Keep-awake', 'Idle', sub, null, '#/car'));
      }
    } else {
      cards.push(stat('warn', 'bolt', 'Keep-awake', 'Not paired', 'Pair with your car so backups aren\'t cut short.', null, '#/car'));
    }
  }

  const s = status.system?.system;
  if (s) {
    const bits = [];
    if (s.cpu_temp_c != null) bits.push(`${Math.round(s.cpu_temp_c)} °C`);
    if (s.wifi_signal_dbm != null) bits.push(`Wi-Fi ${s.wifi_signal_dbm} dBm`);
    bits.push(`up ${uptime(s.uptime_seconds)}`);
    cards.push(stat(s.cpu_temp_c > 75 ? 'bad' : '', 'chip', 'Glovebox', s.model?.replace(/ Rev .*/, '') || s.hostname, bits.join(' · ')));
  }
  box.replaceChildren(...cards);
}

async function showOverview() {
  page('overview', 'Overview', el('div', { id: 'stats', class: 'stats' }), el('div', { id: 'latest' }));
  renderOverviewStats();
  const [sentry, saved, recent] = await Promise.allSettled([
    api('/api/events?kind=sentry'), api('/api/events?kind=saved'), api('/api/recent')]);
  if (current !== 'overview') return;
  const events = [...(sentry.value || []), ...(saved.value || [])].sort((x, y) => (x.time < y.time ? 1 : -1));
  const minutes = recent.value || [];
  const latest = $('#latest');
  if (!events.length && !minutes.length) {
    latest.append(el('div', { class: 'section-head' }, el('h2', {}, 'Footage')),
      empty('film', 'No footage yet', 'Recordings show up here.'));
    return;
  }
  if (events.length) {
    latest.append(el('div', { class: 'section-head' }, el('h2', {}, 'Latest events'), el('a', { href: '#/sentry' }, 'All events →')),
      el('div', { class: 'grid' }, events.slice(0, 8).map((ev) => eventCard(ev, true))));
  }
  if (minutes.length) {
    const ds = drives(minutes);
    const locked = minutes.filter((c) => c.encrypted).length;
    latest.append(
      el('div', { class: 'section-head' },
        el('h2', {}, 'Latest drives'),
        el('a', { href: '#/recent' }, `All ${ds.length} →`)),
      locked ? el('p', { class: 'muted', style: 'margin:-6px 0 12px' }, pill('warn', 'Encrypted by the car'),
        tip('Open a drive to download it for dashcam.tesla.com.')) : '', // append() writes null as "null"
      el('div', { class: 'grid' }, ds.slice(0, 4).map(driveCard)));
    if (!events.length) {
      latest.append(el('p', { class: 'muted', style: 'margin-top:18px;font-size:14px' }, 'No Sentry or saved events yet.'));
    }
  }
}

// drives groups recent minutes into runs at most 3 minutes apart with the
// same encryption, newest drive first; each drive's clips are oldest first.
function drives(clips) {
  const asc = [...clips].sort((a, b) => (a.start < b.start ? -1 : 1));
  const out = [];
  let cur = null;
  for (const c of asc) {
    const t = carTime(c.start);
    if (cur && cur.encrypted === !!c.encrypted && t - cur.last <= 3 * 60 * 1000) {
      cur.clips.push(c);
      cur.last = t;
    } else {
      cur = { encrypted: !!c.encrypted, clips: [c], first: t, last: t };
      out.push(cur);
    }
  }
  return out.reverse();
}

const driveHref = (d) => `#/drive/${encodeURIComponent(d.clips[0].start)}${d.encrypted ? '/enc' : ''}`;
const driveRange = (d) => `${fmtTime(d.first)} – ${fmtTime(new Date(d.last.getTime() + 60000))}`;
const clipBytes = (clips) => clips.reduce((n, c) => n + Object.values(c.cameras).reduce((m, f) => m + f.size, 0), 0);

function driveCard(d) {
  const mins = `${d.clips.length} min`;
  if (d.encrypted) {
    return el('a', { class: 'card locked', href: driveHref(d) },
      el('div', { class: 'thumb' },
        el('span', { class: 'lock-mark' }, icon('lock')),
        el('span', { class: 'when' }, mins),
        el('span', { class: 'badge' }, icon('lock'), 'Encrypted')),
      el('div', { class: 'card-body' },
        el('div', { class: 'card-title' }, driveRange(d)),
        el('div', { class: 'card-sub' }, `${fmtDay(d.first)} · ${gb(clipBytes(d.clips))}`)));
  }
  // a frame from the front camera, a little way into the drive
  const mid = d.clips[Math.floor(d.clips.length / 2)];
  const front = mid.cameras.front || Object.values(mid.cameras)[0];
  const frame = el('video', { muted: true, playsinline: true, preload: 'metadata', src: `${mediaURL(front)}#t=5` });
  return el('a', { class: 'card', href: driveHref(d) },
    el('div', { class: 'thumb' }, frame,
      el('span', { class: 'play' }, icon('play')),
      el('span', { class: 'when' }, mins)),
    el('div', { class: 'card-body' },
      el('div', { class: 'card-title' }, driveRange(d)),
      el('div', { class: 'card-sub' }, `${fmtDay(d.first)} · ${new Set(d.clips.flatMap((c) => Object.keys(c.cameras))).size} cameras`)));
}

// ---------- event lists ----------

function eventHref(ev) {
  return `#/event/${ev.kind}/${encodeURIComponent(ev.id)}${ev.encrypted ? '/enc' : ''}`;
}

function eventCard(ev, showKind) {
  const t = carTime(ev.time);
  const info = ev.info || {};
  const kindBadge = el('span', { class: `badge ${ev.kind}` }, icon(KIND[ev.kind].icon), KIND[ev.kind].name);
  const sub = [reasonText(info.reason), info.city].filter(Boolean);
  if (ev.encrypted) {
    return el('a', { class: 'card locked', href: eventHref(ev) },
      el('div', { class: 'thumb' },
        el('span', { class: 'lock-mark' }, icon('lock')),
        el('span', { class: 'when' }, fmtTime(t)),
        el('span', { class: 'badge' }, icon('lock'), 'Encrypted')),
      el('div', { class: 'card-body' },
        el('div', { class: 'card-title' }, showKind ? `${KIND[ev.kind].name} · ${fmtDay(t)}` : 'Encrypted event'),
        el('div', { class: 'card-sub' }, `${ev.clips} min · view at dashcam.tesla.com`)));
  }
  return el('a', { class: 'card', href: eventHref(ev) },
    el('div', { class: 'thumb', style: ev.thumb ? `background-image:url("${mediaURL(ev.thumb)}")` : '' },
      el('span', { class: 'play' }, icon('play')),
      el('span', { class: 'when' }, fmtTime(t)),
      showKind ? kindBadge : null),
    el('div', { class: 'card-body' },
      el('div', { class: 'card-title' }, sub[0] || (showKind ? KIND[ev.kind].name : 'Event')),
      el('div', { class: 'card-sub' },
        info.city ? [icon('pin'), info.city, ' · '] : null,
        showKind ? `${fmtDay(t)} · ` : '',
        `${ev.clips} min · ${ev.cameras.length} cameras`)));
}

async function showEvents(kind) {
  page(kind, KIND[kind].name, loading());
  let events;
  try {
    events = await api(`/api/events?kind=${kind}`);
  } catch (e) {
    page(kind, KIND[kind].name, empty('shield', 'Couldn\'t load events', e.message));
    return;
  }
  if (current !== kind) return;
  if (!events.length) {
    const box = empty(KIND[kind].icon, KIND[kind].empty, KIND[kind].hint);
    page(kind, KIND[kind].name, box);
    const recent = await api('/api/recent').catch(() => []);
    if (recent.length && current === kind) {
      box.append(el('div', { style: 'margin-top:16px' },
        el('a', { class: 'btn', href: '#/recent' }, icon('film'), `See the ${recent.length} minutes of recent footage`)));
    }
    return;
  }
  const nodes = [];
  let day = '';
  let grid;
  for (const ev of events) {
    const d = fmtDay(carTime(ev.time));
    if (d !== day) {
      day = d;
      grid = el('div', { class: 'grid' });
      nodes.push(el('h3', { class: 'day' }, d), grid);
    }
    grid.append(eventCard(ev, false));
  }
  page(kind, KIND[kind].name, ...nodes);
}

async function showRecent() {
  page('recent', 'Recent', loading());
  let clips;
  try {
    clips = await api('/api/recent');
  } catch (e) {
    page('recent', 'Recent', empty('film', 'Couldn\'t load recent clips', e.message));
    return;
  }
  if (current !== 'recent') return;
  if (!clips.length) {
    page('recent', 'Recent', empty('film', 'No drives yet', 'Driving shows up here.'));
    return;
  }
  const nodes = [];
  let day = '';
  let grid;
  for (const d of drives(clips)) {
    const name = fmtDay(d.first);
    if (name !== day) {
      day = name;
      grid = el('div', { class: 'grid' });
      nodes.push(el('h3', { class: 'day' }, name), grid);
    }
    grid.append(driveCard(d));
  }
  page('recent', 'Recent', ...nodes);
}

// ---------- encrypted ----------

function encryptedNotice(zipURL, bytes) {
  const go = el('a', { class: 'btn btn-primary', href: zipURL, download: '' }, icon('download'), `Download all (${gb(bytes)}) and open dashcam.tesla.com`);
  go.addEventListener('click', () => window.open('https://dashcam.tesla.com', '_blank', 'noopener'));
  return el('div', { class: 'notice' },
    el('span', { class: 'stat-icon' }, icon('lock')),
    el('div', {},
      el('h3', {}, 'Encrypted by your car'),
      el('p', {}, 'Play these at dashcam.tesla.com: download, unzip, drop the files there. ',
        tip('The car\'s "Encrypt Dashcam Recordings" is on, so only Tesla can play them. Its site only takes files from your computer.')),
      el('div', { class: 'actions' },
        go,
        el('a', { class: 'btn', href: 'https://dashcam.tesla.com', target: '_blank', rel: 'noopener' }, 'Just open the site', icon('external'))),
      el('p', { class: 'muted', style: 'margin:12px 0 0;font-size:13px' },
        'About 3–5 MB/s over Wi-Fi. ', tip('Already backed up? The EncryptedClips folder on your NAS is faster.'))));
}

function minuteRows(clips) {
  return el('div', { class: 'minute-files' }, clips.map((c) => {
    const cams = sortCameras(new Set(Object.keys(c.cameras)));
    return el('div', { class: 'minute-row' },
      el('span', { class: 't' }, fmtTime(carTime(c.start))),
      el('div', { class: 'file-chips' }, cams.map((cam) =>
        el('a', { href: mediaURL(c.cameras[cam], true) }, icon('download'), CAMERA_NAMES[cam] || cam))));
  }));
}

function showLocked(name, title, sub, clips, backTo, zipURL) {
  page(name, title,
    el('div', { class: 'player-head' },
      el('a', { class: 'icon-btn', href: backTo, title: 'Back' }, icon('back')),
      el('div', {}, el('h2', {}, sub, ' ', el('span', { class: 'badge' }, icon('lock'), 'Encrypted')))),
    encryptedNotice(zipURL, clipBytes(clips)),
    el('div', { class: 'section-head' }, el('h2', {}, 'Files'), el('span', { class: 'muted' }, `${clips.length} min`)),
    minuteRows(clips));
}

// ---------- player ----------

const player = {
  clips: [],
  i: 0,
  cams: [],
  videos: {},
  master: null,
  playing: false,
  speed: 1,
  scrubbing: false,

  open(name, pageTitle, clips, start, title, sub, backTo, zipURL) {
    const view = $('#player-tpl').content.firstElementChild.cloneNode(true);
    page(name, pageTitle, view);
    this.clips = clips;
    $('#back').href = backTo;
    $('#player-title').textContent = title;
    $('#player-sub').replaceChildren(...sub);
    this.zipURL = zipURL;
    this.speedUnit = 'mph';
    try { this.speedUnit = localStorage.getItem('glovebox-speed-unit') === 'kmh' ? 'kmh' : 'mph'; } catch { /* Optional preference. */ }
    // Low bandwidth plays every camera's keyframes only: all six come to
    // about 7.5 Mbit/s, which the Pi's Wi-Fi carries, against about 43 for
    // the full clips.
    this.lowBandwidth = false;
    try { this.lowBandwidth = localStorage.getItem('glovebox-low-bandwidth') === '1'; } catch { /* Optional preference. */ }

    const set = new Set();
    for (const c of clips) for (const cam of Object.keys(c.cameras)) set.add(cam);
    this.cams = sortCameras(set);
    // Two videos per camera: the one showing, and the spare that buffers the
    // next minute behind it, so a drive plays through without a gap.
    this.videos = {};
    this.spares = {};
    this.prepared = null;
    $('#cams').style.setProperty('--camera-columns', Math.min(3, this.cams.length));
    $('#cams').style.setProperty('--camera-rows', Math.ceil(this.cams.length / 3));
    $('#cams').style.setProperty('--camera-mobile-columns', Math.min(2, this.cams.length));
    $('#cams').style.setProperty('--camera-mobile-rows', Math.ceil(this.cams.length / 2));
    const video = (cam) => {
      const v = el('video', { muted: true, playsinline: true, preload: 'auto' });
      v.muted = true;
      v.addEventListener('timeupdate', () => this.onTime(v));
      v.addEventListener('ended', () => this.onEnded(v));
      v.addEventListener('loadedmetadata', () => { v.playbackRate = this.speed; this.fitDuration(v); });
      v.addEventListener('loadeddata', () => {
        if (this.videos[cam] !== v) return;
        this.showFrame(v);
        this.drawThumbnails(true);
      });
      v.addEventListener('seeked', () => this.videos[cam] === v && this.drawThumbnails(true));
      v.addEventListener('error', () => this.recover(cam, v));
      return v;
    };
    $('#cams').replaceChildren(...this.cams.map((cam) => {
      this.videos[cam] = video(cam);
      this.spares[cam] = video(cam);
      this.spares[cam].classList.add('spare');
      return el('div', { class: 'cam', 'data-cam': cam, onclick: () => this.focus(cam) },
        this.videos[cam], this.spares[cam], el('span', { class: 'label' }, CAMERA_NAMES[cam] || cam));
    }));
    this.buildCameraRail();
    $('#viewer-info').addEventListener('click', () => {
      const details = $('#viewer-details');
      details.hidden = !details.hidden;
      $('#viewer-info').setAttribute('aria-expanded', String(!details.hidden));
      this.renderTelemetry(this.master?.currentTime || 0);
    });
    $('#hud-unit').addEventListener('click', () => {
      this.speedUnit = this.speedUnit === 'mph' ? 'kmh' : 'mph';
      try { localStorage.setItem('glovebox-speed-unit', this.speedUnit); } catch { /* Optional preference. */ }
      this.renderTelemetry(this.master?.currentTime || 0);
    });
    const fullscreen = $('#viewer-fullscreen');
    fullscreen.hidden = !document.fullscreenEnabled;
    fullscreen.addEventListener('click', async () => {
      try { if (document.fullscreenElement) await document.exitFullscreen(); else await view.requestFullscreen(); }
      catch { fullscreen.title = 'Fullscreen is unavailable in this browser'; }
    });
    view.addEventListener('fullscreenchange', () => { fullscreen.setAttribute('aria-label', document.fullscreenElement ? 'Exit fullscreen' : 'Enter fullscreen'); });
    $('#clip-strip').replaceChildren(...clips.map((c, k) =>
      el('button', { type: 'button', onclick: () => this.load(k, this.playing) }, fmtTime(carTime(c.start)))));

    $('#play').addEventListener('click', () => (this.playing ? this.pause() : this.play()));
    $('#prev').addEventListener('click', () => this.i > 0 && this.load(this.i - 1, this.playing));
    $('#next').addEventListener('click', () => this.i + 1 < this.clips.length && this.load(this.i + 1, this.playing));
    const quality = $('#quality');
    quality.value = this.lowBandwidth ? 'low' : 'full';
    quality.addEventListener('change', () => this.setLowBandwidth(quality.value === 'low'));
    $('#speed').value = String(this.speed);
    $('#speed').addEventListener('change', (e) => {
      this.speed = Number(e.target.value);
      for (const v of [...Object.values(this.videos), ...Object.values(this.spares)]) v.playbackRate = this.speed;
    });
    // The timeline is real time: a drive can include clips shorter than a
    // minute (the pieces a Saved clip was cut from).
    const first = carTime(clips[0].start).getTime();
    this.offsets = clips.map((c) => (carTime(c.start).getTime() - first) / 1000);
    this.duration = this.offsets[this.offsets.length - 1] + 60;
    const scrub = $('#scrub');
    scrub.max = this.duration;
    scrub.value = this.offsets[start];
    scrub.addEventListener('input', () => { this.scrubbing = true; });
    scrub.addEventListener('change', () => { this.scrubbing = false; this.seek(Number(scrub.value)); });
    this.load(start, false);
  },

  load(i, autoplay) {
    this.stopFrameUpdates();
    // The next minute is already buffering in the spares: show those instead
    // of starting over, so the drive continues without a blank frame.
    const swap = this.prepared === i;
    this.prepared = null;
    this.i = i;
    this.clockSecond = null;
    this.lastSample = undefined;
    const clip = this.clips[i];
    this.master = null;
    for (const cam of this.cams) {
      const old = this.videos[cam];
      const box = old.parentElement;
      const file = clip.cameras[cam];
      if (swap) {
        const next = this.spares[cam];
        next.classList.remove('spare');
        old.classList.add('spare');
        this.videos[cam] = next;
        this.spares[cam] = old;
        this.handOver(old, next);
      } else {
        old.pause();
        if (file) old.src = this.source(file);
        else unload(old);
        unload(this.spares[cam]);
      }
      box.classList.toggle('missing', !file);
    }
    this.pickMaster();
    // A spare that loaded before it was swapped in hasn't shown its frame.
    if (swap) for (const v of Object.values(this.videos)) this.showFrame(v);
    for (const [k, b] of [...$('#clip-strip').children].entries()) b.classList.toggle('current', k === i);
    $('#downloads').replaceChildren(...this.cams.filter((c) => clip.cameras[c]).map((c) =>
      el('a', { href: mediaURL(clip.cameras[c], true) }, icon('download'), CAMERA_NAMES[c] || c)),
      this.zipURL ? el('a', { href: this.zipURL, download: '' }, icon('download'), `Everything (${gb(clipBytes(this.clips))} zip)`) : '');
    this.updateClock(0);
    this.updateCameraDownload();
    this.drawThumbnails(true);
    this.startFrameUpdates();
    this.loadTelemetry(clip);
    $('#prev').disabled = i === 0;
    $('#next').disabled = i + 1 >= this.clips.length;
    if (autoplay) this.play();
  },

  // handOver keeps the minute that ended on screen, on its last frame,
  // until the next one has drawn its first: the new video shows black until
  // then, which blinked at every minute.
  handOver(old, next) {
    this.finishHandOver(old);
    old.classList.add('leaving');
    const done = () => this.finishHandOver(old);
    old.handOverTimer = setTimeout(done, 1500); // a camera that's slow to load
    if (next.requestVideoFrameCallback) next.requestVideoFrameCallback(() => done());
    else next.addEventListener('timeupdate', done, { once: true });
  },

  finishHandOver(old) {
    if (!old.classList.contains('leaving')) return;
    clearTimeout(old.handOverTimer);
    old.classList.remove('leaving');
    unload(old);
  },

  setPlayIcon(name) {
    const b = $('#play');
    if (b) { b.replaceChildren(icon(name)); b.setAttribute('aria-label', name === 'pause' ? 'Pause' : 'Play'); }
  },

  // active is what streams: the camera on screen, or every camera in the
  // grid. All six at once need about 43 Mbit/s, more than the Pi's Wi-Fi
  // gives; Safari then never starts playing. The others keep the frame they
  // loaded, for the thumbnails.
  //
  // In low bandwidth every camera streams, the one on screen first.
  active() {
    const loaded = this.cams.map((c) => this.videos[c]).filter((v) => v?.getAttribute('src'));
    if (this.selectedCamera === 'grid') return loaded;
    const main = this.videos[this.selectedCamera];
    if (this.lowBandwidth) return main?.getAttribute('src') ? [main, ...loaded.filter((v) => v !== main)] : loaded;
    return main?.getAttribute('src') ? [main] : loaded.slice(0, 1);
  },

  // pickMaster makes the first active video the one the others, the clock
  // and the HUD follow, and lets only the active ones buffer ahead.
  pickMaster() {
    const active = this.active();
    this.master = active[0] || null;
    for (const v of Object.values(this.videos)) v.preload = active.includes(v) ? 'auto' : 'metadata';
  },

  play() {
    this.playing = true;
    this.setPlayIcon('pause');
    const active = this.active();
    for (const v of Object.values(this.videos)) if (!active.includes(v)) v.pause();
    for (const v of active) {
      v.playbackRate = this.speed;
      v.play().catch((e) => {
        // A browser that won't play the main camera (not yet loaded, or a
        // limit on videos playing at once) shouldn't look like it's playing.
        if (v === this.master && e.name !== 'AbortError') {
          this.playing = false;
          this.setPlayIcon('play');
          $('#viewer-time').textContent = `Can't play here: ${e.message || e.name}`;
        }
      });
    }
  },

  pause() {
    this.playing = false;
    this.setPlayIcon('play');
    for (const v of Object.values(this.videos)) v.pause();
  },

  stop() {
    this.stopFrameUpdates();
    this.telemetryController?.abort();
    this.telemetrySamples = [];
    clearTimeout(this.telemetryHoldTimer);
    this.telemetryHold = false;
    this.playing = false;
    for (const v of Object.values(this.spares || {})) this.finishHandOver(v);
    for (const v of [...Object.values(this.videos), ...Object.values(this.spares || {})]) unload(v);
    this.videos = {};
    this.spares = {};
    this.prepared = null;
    this.clips = [];
  },

  // showFrame decodes the frame a camera that isn't streaming has loaded:
  // Chromium doesn't until it's seeked or played, so its thumbnail stays
  // blank. Seeking to where it is uses what's already buffered, and
  // 'seeked' draws it.
  showFrame(v) {
    if (v.readyState >= 2 && v.paused && !this.active().includes(v)) v.currentTime = v.currentTime;
  },

  // Keeps the other cameras with the master. Small drift is taken up by
  // running them a little faster or slower; seeking stalls a camera while it
  // refills its decoder, so it's only for big jumps or while paused.
  onTime(v) {
    if (v !== this.master) return;
    this.fitDuration(v);
    for (const other of this.active()) {
      if (other === v || other.readyState < 2) continue;
      const drift = other.currentTime - v.currentTime;
      if (v.paused || Math.abs(drift) > 1) {
        if (Math.abs(drift) > 0.1) other.currentTime = v.currentTime;
        other.playbackRate = this.speed;
      } else {
        other.playbackRate = this.speed * (drift > 0.04 ? 0.95 : drift < -0.04 ? 1.05 : 1);
      }
    }
    if (v.duration - v.currentTime < 20) this.prepareNext();
    if (!this.scrubbing) $('#scrub').value = this.offsets[this.i] + v.currentTime;
    // Frame callbacks keep the clock and HUD in step with the picture, but
    // some browsers stop sending them (a hidden or throttled video): then
    // the regular time updates keep the clock moving.
    if (!this.frameVideo || performance.now() - (this.lastFrameAt || 0) > 500) this.updateClock(v.currentTime);
  },

  onEnded(v) {
    if (v !== this.master) return;
    if (this.i + 1 < this.clips.length) this.load(this.i + 1, true);
    else this.pause();
  },

  // Starts the next minute buffering in the spares 20 seconds before this
  // one ends. Only the master buffers it all: on the Pi's Wi-Fi, the other
  // cameras' next minute would take bandwidth from this one, so they fetch
  // just the index and start loading when they're swapped in. Keyframes
  // are small enough for every camera to buffer.
  prepareNext() {
    const k = this.i + 1;
    if (this.prepared === k || k >= this.clips.length) return;
    this.prepared = k;
    const clip = this.clips[k];
    for (const cam of this.cams) {
      const v = this.spares[cam], file = clip.cameras[cam];
      this.finishHandOver(v);
      v.preload = this.lowBandwidth || this.videos[cam] === this.master ? 'auto' : 'metadata';
      if (file) v.src = this.source(file);
      else unload(v);
    }
    this.prefetchTelemetry(clip);
  },

  // Drops a prepared minute whose cameras no longer match what's watched.
  discardPrepared() {
    this.prepared = null;
    for (const v of Object.values(this.spares)) { this.finishHandOver(v); unload(v); }
  },

  // A decoder can fail when the browser runs short of them (several cameras
  // plus the next minute buffering). One reload, at the same moment,
  // recovers; a file that really is damaged keeps its error.
  recover(cam, v) {
    const src = v.getAttribute('src');
    if (this.videos[cam] !== v || v.error?.code !== MediaError.MEDIA_ERR_DECODE || !src || v.dataset.retried === src) return;
    const at = v.currentTime;
    v.dataset.retried = src;
    v.src = src;
    v.addEventListener('loadedmetadata', () => {
      v.currentTime = this.master && this.master !== v ? this.master.currentTime : at;
      if (this.playing && this.active().includes(v)) v.play().catch(() => {});
    }, { once: true });
  },

  // The timeline first assumes a drive's last minute is a whole one, but
  // it usually ends early, where the car was parked: once the master has
  // loaded it, the timeline ends where it does, instead of playback
  // stopping short of the end.
  fitDuration(v) {
    const last = this.clips.length - 1;
    if (v !== this.master || this.i !== last || !Number.isFinite(v.duration) || !v.duration) return;
    const duration = this.offsets[last] + v.duration;
    if (Math.abs(duration - this.duration) < 0.05) return;
    this.duration = duration;
    $('#scrub').max = duration;
  },

  // Position in the recording, in seconds of real time from its first clip.
  position() {
    return (this.offsets?.[this.i] || 0) + (this.master?.currentTime || 0);
  },

  seek(seconds) {
    seconds = Math.max(0, Math.min(seconds, this.duration - 0.1));
    let k = 0;
    while (k + 1 < this.clips.length && this.offsets[k + 1] <= seconds) k++;
    const offset = seconds - this.offsets[k];
    // only the cameras streaming; the others catch up when they're shown
    const apply = () => {
      for (const v of this.active()) v.currentTime = Math.min(offset, (v.duration || 60) - 0.1);
    };
    if (k !== this.i) {
      const playing = this.playing;
      this.load(k, false);
      const ready = () => { apply(); if (playing) this.play(); };
      // A prepared minute already has its metadata.
      if (this.master?.readyState >= 1) ready();
      else this.master?.addEventListener('loadedmetadata', ready, { once: true });
    } else {
      apply();
    }
  },

  updateClock(offset) {
    const clip = this.clips[this.i];
    if (clip) {
      const date = new Date(carTime(clip.start).getTime() + offset * 1000);
      const second = Math.floor(date.getTime() / 1000);
      if (this.clockSecond !== second) {
        this.clockSecond = second;
        $('#clock').textContent = fmtClock(date);
        $('#viewer-time').textContent = date.toLocaleString(undefined, { month: 'short', day: 'numeric', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
        $('#viewer-time').dateTime = date.toISOString();
      }
      this.renderTelemetry(offset);
      this.drawThumbnails();
    }
  },

  startFrameUpdates() {
    const video = this.master;
    if (!video?.requestVideoFrameCallback) return;
    this.frameVideo = video;
    const draw = (_now, frame) => {
      if (this.frameVideo !== video || !this.clips.length) return;
      this.lastFrameAt = performance.now();
      this.updateClock(frame.mediaTime);
      this.frameRequest = video.requestVideoFrameCallback(draw);
    };
    this.frameRequest = video.requestVideoFrameCallback(draw);
  },

  stopFrameUpdates() {
    if (this.frameVideo && this.frameRequest != null) this.frameVideo.cancelVideoFrameCallback(this.frameRequest);
    this.frameVideo = null;
    this.frameRequest = null;
  },

  buildCameraRail() {
    this.thumbnailCanvases = {};
    const choice = (id, label) => {
      const canvas = el('canvas', { width: '240', height: '135', 'aria-hidden': 'true' });
      this.thumbnailCanvases[id] = canvas;
      return el('button', { type: 'button', class: 'camera-choice', 'data-camera': id,
        'aria-label': label, 'aria-pressed': 'false', onclick: () => this.focus(id) }, canvas, el('span', {}, label));
    };
    $('#camera-rail').replaceChildren(choice('grid', 'Grid'), ...this.cams.map(cam => choice(cam, CAMERA_NAMES[cam] || cam)));
    this.focus(this.cams.includes('front') ? 'front' : this.cams[0]);
    this.lastThumbnailDraw = 0;
  },

  drawThumbnails(force = false) {
    if (!force && performance.now() - this.lastThumbnailDraw < 250) return;
    this.lastThumbnailDraw = performance.now();
    const mosaic = this.thumbnailCanvases?.grid;
    const grid = mosaic?.getContext('2d');
    const columns = Math.min(3, this.cams.length), rows = Math.ceil(this.cams.length / columns);
    this.cams.forEach((cam, index) => {
      const canvas = this.thumbnailCanvases?.[cam], video = this.videos[cam];
      if (!canvas) return;
      const context = canvas.getContext('2d');
      const tile = [index % columns * mosaic.width / columns, Math.floor(index / columns) * mosaic.height / rows, mosaic.width / columns, mosaic.height / rows];
      if (!video?.getAttribute('src')) { // not recorded this minute
        context.clearRect(0, 0, canvas.width, canvas.height);
        grid?.clearRect(...tile);
        return;
      }
      // still loading: keep the last picture rather than going blank
      if (video.readyState < 2) return;
      try {
        context.drawImage(video, 0, 0, canvas.width, canvas.height);
        grid?.drawImage(video, ...tile);
      } catch { /* A camera still seeking will draw on its next frame. */ }
    });
  },

  cacheTelemetry(url, result) {
    this.telemetryCache ||= new Map();
    this.telemetryCache.set(url, result);
    while (this.telemetryCache.size > 6) this.telemetryCache.delete(this.telemetryCache.keys().next().value);
  },

  // Reads the next minute's front-camera telemetry ahead, so the HUD carries
  // on at the switch. The cache holds the pending promise; a failure leaves
  // the minute to load as usual.
  prefetchTelemetry(clip) {
    const file = clip.cameras.front;
    if (!file) return;
    const url = telemetryURL(file);
    if (this.telemetryCache?.has(url)) return;
    this.cacheTelemetry(url, fetch(url)
      .then((r) => (r.ok ? r.json() : null))
      .then((result) => { if (result) this.cacheTelemetry(url, result); else this.telemetryCache.delete(url); return result; })
      .catch(() => { this.telemetryCache.delete(url); return null; }));
  },

  async loadTelemetry(clip) {
    this.telemetryController?.abort();
    const controller = new AbortController();
    this.telemetryController = controller;
    this.telemetryCache ||= new Map();
    clearTimeout(this.telemetryHoldTimer);
    // Read ahead already: no gap at all.
    const ahead = clip.cameras.front && this.telemetryCache.get(telemetryURL(clip.cameras.front));
    if (ahead && !(ahead instanceof Promise) && ahead.samples?.length) {
      this.telemetryHold = false;
      this.telemetrySamples = ahead.samples;
      this.telemetryState = '';
      this.lastSample = undefined;
      this.renderTelemetry(0);
      return;
    }
    // Otherwise the HUD keeps showing what it did for a moment, rather than
    // blanking for what's usually a fraction of a second.
    this.telemetryHold = true;
    this.telemetryHoldTimer = setTimeout(() => {
      if (this.telemetryController !== controller || !this.telemetryHold) return;
      this.telemetryHold = false;
      this.telemetrySamples = [];
      this.telemetryState = 'Reading recorded telemetry…';
      this.renderTelemetry(this.master?.currentTime || 0);
    }, 1000);
    for (const camera of ['front', ...this.cams.filter(c => c !== 'front')]) {
      const file = clip.cameras[camera];
      if (!file) continue;
      const url = telemetryURL(file);
      try {
        let result = await this.telemetryCache.get(url);
        if (!result) {
          const response = await fetch(url, { signal: controller.signal });
          if (!response.ok) continue;
          result = await response.json();
          this.cacheTelemetry(url, result);
        }
        if (controller.signal.aborted || this.clips[this.i] !== clip) return;
        if (result.samples?.length) {
          this.telemetryHold = false;
          this.telemetrySamples = result.samples;
          this.telemetryState = '';
          this.renderTelemetry(this.master?.currentTime || 0);
          return;
        }
      } catch (error) { if (controller.signal.aborted) return; }
    }
    if (!controller.signal.aborted && this.clips[this.i] === clip) {
      this.telemetryHold = false;
      this.telemetrySamples = [];
      this.telemetryState = 'No telemetry recorded in this clip';
      this.renderTelemetry(this.master?.currentTime || 0);
    }
  },

  renderTelemetry(offset) {
    if (!$('#viewer-telemetry') || this.telemetryHold) return;
    const sample = window.GloveboxTelemetry.at(this.telemetrySamples || [], offset);
    // Called for every video frame; the HUD only changes with the sample.
    const shown = [sample, this.speedUnit, this.telemetryState];
    if ($('#viewer-details').hidden && this.lastSample !== undefined && shown.every((v, k) => v === this.lastSample[k])) return;
    this.lastSample = shown;
    $('#hud-status').textContent = sample ? '' : this.telemetryState || 'No telemetry for this moment';
    $('#viewer-telemetry').classList.toggle('unavailable', !sample);
    $('#hud-speed').textContent = sample ? window.GloveboxTelemetry.speed(sample.speed_mps, this.speedUnit) : '—';
    $('#hud-gear').textContent = sample ? ['P', 'D', 'R', 'N'][sample.gear] || '?' : '—';
    $('#hud-unit').textContent = this.speedUnit === 'kmh' ? 'KM/H' : 'MPH';
    $('#hud-unit').setAttribute('aria-label', 'Change speed units, currently ' + $('#hud-unit').textContent);
    $('#hud-assist').textContent = sample ? ['Manual', 'FSD', 'Autosteer', 'TACC'][sample.autopilot] || 'Unknown' : '—';
    $('#hud-wheel').style.transform = 'rotate(' + (sample?.steering_deg || 0) + 'deg)';
    $('#hud-left').classList.toggle('active', !!sample?.left);
    $('#hud-right').classList.toggle('active', !!sample?.right);
    $('#hud-brake').classList.toggle('active', !!sample?.brake);
    // The car records the pedal as a percentage already, 0 to 100.
    const pedal = Math.max(0, Math.min(100, sample?.accelerator || 0));
    $('#hud-throttle').textContent = sample ? Math.round(pedal) + '%' : '—';
    $('#hud-throttle').style.setProperty('--pedal', pedal + '%');
    $('#hud-steering').classList.toggle('assisted', !!sample && sample.autopilot > 0);
    $('#hud-throttle').title = sample ? 'Accelerator pedal: ' + pedal.toFixed(1) + '% (not motor power)' : 'No recorded accelerator data';
    if (!$('#viewer-details').hidden) {
      const rows = sample ? [
        ['Speed', window.GloveboxTelemetry.speed(sample.speed_mps, this.speedUnit) + (this.speedUnit === 'kmh' ? ' km/h' : ' mph')],
        ['Gear', ['Park', 'Drive', 'Reverse', 'Neutral'][sample.gear] || 'Unknown'],
        ['Steering wheel', sample.steering_deg.toFixed(1) + '°'],
        ['Accelerator pedal', pedal.toFixed(1) + '%'],
        ['Brake', sample.brake ? 'Applied' : 'Released'],
        ['Turn signals', [sample.left ? 'Left' : '', sample.right ? 'Right' : ''].filter(Boolean).join(' + ') || 'Off'],
        ['Driving assistance', ['None', 'Self-driving', 'Autosteer', 'Traffic-aware cruise'][sample.autopilot] || 'Unknown'],
        ['Latitude / longitude', sample.latitude.toFixed(6) + ', ' + sample.longitude.toFixed(6)],
        ['Heading', sample.heading.toFixed(1) + '°'],
        ['Acceleration X / Y / Z', [sample.accel_x, sample.accel_y, sample.accel_z].map(n => n.toFixed(2)).join(' / ') + ' m/s²'],
        ['Frame / schema', sample.frame + ' / v' + sample.version],
      ] : [['Telemetry', this.telemetryState || 'No recorded sample at this moment']];
      $('#telemetry-details').replaceChildren(...rows.flatMap(([key, value]) => [el('dt', {}, key), el('dd', {}, value)]));
    }
  },

  updateCameraDownload() {
    const camera = this.selectedCamera === 'grid' ? (this.cams.includes('front') ? 'front' : this.cams[0]) : this.selectedCamera;
    const file = this.clips[this.i]?.cameras[camera];
    const link = $('#viewer-download');
    if (!link) return;
    if (file) { link.href = mediaURL(file, true); link.setAttribute('download', ''); }
    else { link.removeAttribute('href'); link.removeAttribute('download'); }
    link.setAttribute('aria-disabled', String(!file));
    link.setAttribute('aria-label', 'Download ' + (CAMERA_NAMES[camera] || camera) + ' camera');
    link.title = file ? 'Download ' + (CAMERA_NAMES[camera] || camera) + ' camera' : 'Camera was not recorded in this minute';
  },

  source(file) {
    return this.lowBandwidth ? keyframesURL(file) : mediaURL(file);
  },

  // Switches between the full clips and keyframes, carrying on from the
  // same moment.
  setLowBandwidth(on) {
    if (on === this.lowBandwidth) return;
    this.lowBandwidth = on;
    try { localStorage.setItem('glovebox-low-bandwidth', on ? '1' : '0'); } catch { /* Optional preference. */ }
    if (!this.master) return;
    const at = this.master.currentTime, playing = this.playing;
    this.discardPrepared();
    this.load(this.i, false);
    for (const v of this.active()) v.addEventListener('loadedmetadata', () => { v.currentTime = at; }, { once: true });
    if (playing) this.play();
  },

  focus(camera) {
    if (camera !== 'grid' && !this.cams.includes(camera)) return;
    this.selectedCamera = camera;
    $('#cams').classList.toggle('viewer-grid', camera === 'grid');
    for (const box of $('#cams').children) box.classList.toggle('main', box.dataset.cam === camera);
    for (const button of $('#camera-rail').children) button.setAttribute('aria-pressed', String(button.dataset.camera === camera));
    this.updateCameraDownload();
    // Switch what streams, picking up where the old view was.
    if (!this.master) return; // nothing loaded yet
    const at = this.master.currentTime;
    this.stopFrameUpdates();
    this.discardPrepared();
    this.pickMaster();
    for (const v of this.active()) if (Math.abs(v.currentTime - at) > 0.25) v.currentTime = at;
    if (this.playing) this.play();
    else for (const v of Object.values(this.videos)) v.pause();
    this.startFrameUpdates();
  },

};

document.addEventListener('keydown', (e) => {
  if (!player.clips.length || e.target.closest('input, select, textarea, button, a, [contenteditable=true]')) return;
  if (e.key === 'Escape' && !document.fullscreenElement) {
    if (!$('#viewer-details').hidden) { $('#viewer-details').hidden = true; $('#viewer-info').setAttribute('aria-expanded', 'false'); }
    else location.hash = $('#back').getAttribute('href');
    return;
  }
  if (e.key === ' ') { e.preventDefault(); player.playing ? player.pause() : player.play(); }
  if (e.key === 'ArrowRight') player.seek(player.position() + 5);
  if (e.key === 'ArrowLeft') player.seek(player.position() - 5);
});

async function showEvent(kind, id, encrypted) {
  const name = `${kind}:event`;
  page(name, KIND[kind].name, loading());
  let ev;
  try {
    ev = await api(`/api/events/${kind}/${encodeURIComponent(id)}${encrypted ? '?encrypted=1' : ''}`);
  } catch (e) {
    page(name, KIND[kind].name, empty('shield', 'Couldn\'t load the event', e.message));
    return;
  }
  if (current !== name) return;
  const t = carTime(ev.time);
  const title = `${fmtDay(t)}, ${fmtTime(t)}`;
  if (ev.encrypted) {
    showLocked(name, KIND[kind].name, `${KIND[kind].name} · ${title}`, ev.clips, `#/${kind}`,
      `/api/events/${kind}/${encodeURIComponent(id)}/zip?encrypted=1`);
    return;
  }
  if (!ev.clips.length) {
    page(name, KIND[kind].name, empty('film', 'Nothing to play', 'This event has no complete clips yet.'));
    return;
  }
  const info = ev.info || {};
  const sub = [[reasonText(info.reason), info.city].filter(Boolean).join(' · ')];
  if (info.est_lat && info.est_lon) {
    const lat = encodeURIComponent(info.est_lat);
    const lon = encodeURIComponent(info.est_lon);
    sub.push(' · ', el('a', { href: `https://www.openstreetmap.org/?mlat=${lat}&mlon=${lon}#map=17/${lat}/${lon}`, target: '_blank', rel: 'noopener' }, 'map'));
  }
  // Sentry events start playing near the event time; the clips before it are context.
  let start = 0;
  if (kind === 'sentry') {
    const k = ev.clips.findIndex((c) => carTime(c.start) > t);
    start = Math.max(0, (k === -1 ? ev.clips.length : k) - 1);
  }
  player.open(name, KIND[kind].name, ev.clips, start, title, sub, `#/${kind}`, `/api/events/${kind}/${encodeURIComponent(id)}/zip`);
}

function recentZip(d) {
  const q = new URLSearchParams({ from: d.clips[0].start, to: d.clips[d.clips.length - 1].start });
  if (d.encrypted) q.set('encrypted', '1');
  return `/api/recent/zip?${q}`;
}

async function showDrive(start, encrypted) {
  const name = 'recent:drive';
  page(name, 'Recent', loading());
  let all;
  try {
    all = await api('/api/recent');
  } catch (e) {
    page(name, 'Recent', empty('film', 'Couldn\'t load recent clips', e.message));
    return;
  }
  if (current !== name) return;
  const d = drives(all).find((x) => x.encrypted === encrypted && x.clips.some((c) => c.start === start));
  if (!d) {
    page(name, 'Recent', empty('film', 'That drive is gone', 'It was cleared to make room.'));
    return;
  }
  const title = `${fmtDay(d.first)}, ${driveRange(d)}`;
  if (encrypted) {
    showLocked(name, 'Recent', title, d.clips, '#/recent', recentZip(d));
    return;
  }
  const i = Math.max(0, d.clips.findIndex((c) => c.start === start));
  player.open(name, 'Recent', d.clips, i, title, [`${d.clips.length} min of driving`], '#/recent', recentZip(d));
}

// ---------- car ----------

function padArt() {
  // Model 3 front console: two inclined charging bays beneath the screen.
  const svg = new DOMParser().parseFromString(`
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 460 270" class="pad-art" role="img" aria-labelledby="pad-title pad-desc">
  <title id="pad-title">Place your key card on a wireless charging pad</title>
  <desc id="pad-desc">Model 3 center console viewed from the seats. A key card rests flat against the left of two inclined wireless charging bays beneath the touchscreen.</desc>
  <defs>
    <linearGradient id="pad-shell" x1="0" y1="0" x2="0" y2="1">
      <stop stop-color="currentColor" stop-opacity=".09"/>
      <stop offset="1" stop-color="currentColor" stop-opacity=".03"/>
    </linearGradient>
    <linearGradient id="pad-card" x1="0" y1="0" x2="1" y2="1">
      <stop stop-color="#c8dcff"/><stop offset="1" stop-color="#8eafe9"/>
    </linearGradient>
  </defs>
  <!-- Touchscreen and slim dashboard establish the familiar console silhouette. -->
  <path d="M50 56h360" stroke="currentColor" stroke-opacity=".1"/>
  <path d="M116 24h228l-4 42a9 9 0 0 1-9 8H129a9 9 0 0 1-9-8Z" fill="currentColor" fill-opacity=".04" stroke="currentColor" stroke-opacity=".24" stroke-width="1.5"/>
  <path d="M128 62h204" stroke="currentColor" stroke-opacity=".08"/>
  <!-- Inclined dual charging tray, with a raised lower retaining lip. -->
  <path d="M127 82h206l-16 112a15 15 0 0 1-15 13H158a15 15 0 0 1-15-13Z" fill="url(#pad-shell)" stroke="currentColor" stroke-opacity=".28" stroke-width="1.5"/>
  <path d="M139 90h85v101h-66a6 6 0 0 1-6-5Z" fill="currentColor" fill-opacity=".045" stroke="currentColor" stroke-opacity=".1"/>
  <path d="M236 90h85l-13 96a6 6 0 0 1-6 5h-66Z" fill="currentColor" fill-opacity=".045" stroke="currentColor" stroke-opacity=".1"/>
  <path d="M230 94v94M154 196h152" stroke="currentColor" stroke-opacity=".2" stroke-width="1.5" stroke-linecap="round"/>
  <!-- Subtle blue reader rings indicate the placement area, not a separate object. -->
  <g fill="none" stroke="var(--accent, #9abaff)">
    <ellipse cx="183" cy="143" rx="32" ry="39" opacity=".13"/>
    <ellipse cx="183" cy="143" rx="25" ry="32" opacity=".22"/>
    <ellipse cx="183" cy="143" rx="18" ry="25" opacity=".3"/>
  </g>
  <!-- Portrait keycard lies flush with the left charging surface. -->
  <path d="m164 120 37-4a4 4 0 0 1 4 4l2 58a4 4 0 0 1-4 4l-37 3a4 4 0 0 1-4-4l-2-57a4 4 0 0 1 4-4Z" fill="#101319" fill-opacity=".18" transform="translate(2 3)"/>
  <path d="m164 117 37-4a4 4 0 0 1 4 4l2 58a4 4 0 0 1-4 4l-37 3a4 4 0 0 1-4-4l-2-57a4 4 0 0 1 4-4Z" fill="url(#pad-card)" stroke="#d9e6ff" stroke-opacity=".75"/>
  <rect x="171" y="128" width="12" height="9" rx="2" fill="#294d82" fill-opacity=".45" transform="rotate(-6 177 132)"/>
  <path d="m172 164 20-2m-20 7 12-1" stroke="#294d82" stroke-opacity=".55" stroke-width="2" stroke-linecap="round"/>
  <!-- Storage cover continues forward toward the seats. -->
  <path d="m148 207-9 34h182l-9-34m-169 23h174" fill="none" stroke="currentColor" stroke-opacity=".16" stroke-width="1.5"/>
  <path d="M66 137h67l26 10" fill="none" stroke="var(--accent, #9abaff)" stroke-opacity=".65" stroke-width="1.3"/>
  <circle cx="159" cy="147" r="2.5" fill="var(--accent, #9abaff)"/>
  <text x="28" y="123" fill="currentColor" font-size="12" font-weight="600">Key card</text>
  <text x="28" y="154" fill="currentColor" fill-opacity=".55" font-size="10">Lay flat</text>
  <path d="M288 144h39l18-13h23" fill="none" stroke="currentColor" stroke-opacity=".3" stroke-width="1.3"/>
  <circle cx="288" cy="144" r="2" fill="currentColor" fill-opacity=".4"/>
  <text x="346" y="113" fill="currentColor" fill-opacity=".75" font-size="11">Wireless</text>
  <text x="346" y="127" fill="currentColor" fill-opacity=".75" font-size="11">charging pads</text>
</svg>`, 'image/svg+xml').documentElement;
  return document.importNode(svg, true);
}

function result(state, text) {
  return el('div', { class: `result ${state}` }, state === 'busy' ? el('span', { class: 'spinner' }) : icon(state === 'ok' ? 'check' : 'lock'), el('span', {}, text));
}

async function runStep(button, out, busyText, fn) {
  button.disabled = true;
  out.replaceChildren(result('busy', busyText));
  try {
    const okText = await fn();
    out.replaceChildren(result('ok', okText));
    return true;
  } catch (e) {
    out.replaceChildren(result('bad', e.message));
    return false;
  } finally {
    button.disabled = false;
  }
}

async function showCar() {
  page('car', 'Car', loading());
  let car;
  try {
    car = await api('/api/car');
  } catch (e) {
    page('car', 'Car', empty('car', 'Keep-awake is off', e.message));
    return;
  }
  if (current !== 'car') return;
  status.car = car;
  const hasVIN = !!car.vin;
  // paired before 0.5, with a key that can't keep the car awake
  const oldKey = car.paired && car.role !== 'driver';

  // 1. VIN
  const vin = el('input', { type: 'text', class: 'vin-input', value: car.vin, placeholder: '17 characters', maxlength: 17, autocomplete: 'off', spellcheck: 'false', 'aria-label': 'VIN' });
  const vinOut = el('div');
  const vinBtn = el('button', { type: 'button', class: hasVIN ? '' : 'btn-primary' }, hasVIN ? 'Update' : 'Save');
  vinBtn.addEventListener('click', async () => {
    if (await runStep(vinBtn, vinOut, 'Saving…', async () => { await send('PUT', '/api/car/vin', { vin: vin.value }); return 'Saved.'; })) setTimeout(showCar, 600);
  });
  vin.addEventListener('keydown', (e) => { if (e.key === 'Enter') vinBtn.click(); });

  // 2. pairing request
  const pairOut = el('div');
  const pairBtn = el('button', { type: 'button', class: car.paired && !oldKey ? '' : 'btn-primary', disabled: !hasVIN }, icon('card'), 'Send pairing request');
  pairBtn.addEventListener('click', () => runStep(pairBtn, pairOut, 'Talking to the car over Bluetooth…', async () => {
    await send('POST', '/api/car/pair');
    return 'Sent. Key card on the phone pad, then confirm on screen.';
  }));

  // 3. check
  const checkOut = el('div');
  const checkBtn = el('button', { type: 'button', class: car.paired ? '' : 'btn-primary', disabled: !hasVIN }, icon('check'), car.paired ? 'Check again' : 'Check pairing');
  checkBtn.addEventListener('click', async () => {
    if (await runStep(checkBtn, checkOut, 'Asking the car…', async () => { await send('POST', '/api/car/check'); return 'Paired. The car accepted Glovebox\'s key.'; })) setTimeout(showCar, 900);
  });

  const stepState = (done, cur, enabled) => `step ${done ? 'done' : ''} ${cur ? 'current' : ''} ${enabled ? '' : 'disabled'}`;
  const steps = el('div', { class: 'steps' },
    el('div', { class: stepState(hasVIN, !hasVIN, true) },
      el('span', { class: 'step-num' }, hasVIN ? icon('check') : '1'),
      el('div', {},
        el('h3', {}, 'VIN', tip('Controls → Software, or the Tesla app. Used to find the car over Bluetooth.')),
        el('div', { class: 'row' }, vin, vinBtn), vinOut)),
    el('div', { class: stepState(car.paired, hasVIN && !car.paired, hasVIN) },
      el('span', { class: 'step-num' }, car.paired ? icon('check') : '2'),
      el('div', {},
        el('h3', {}, 'Pair', tip('In the car, Pi plugged in, key card in hand.')),
        el('ol', {},
          el('li', {}, 'Press ', el('b', {}, 'Send pairing request')),
          el('li', {}, 'Within 30 s, lay a ', el('b', {}, 'key card on the phone pad')),
          el('li', {}, 'Accept the new key on screen')),
        padArt(),
        el('div', { class: 'row' }, pairBtn), pairOut)),
    el('div', { class: stepState(car.paired, false, hasVIN) },
      el('span', { class: 'step-num' }, car.paired ? icon('check') : '3'),
      el('div', {},
        el('h3', {}, car.paired ? 'Paired' : 'Check'),
        car.paired ? el('p', { class: 'muted' }, car.paired_at ? new Date(car.paired_at).toLocaleDateString() : '') : null,
        oldKey ? el('p', {}, pill('warn', 'Pair again'),
          tip('This older Charging Manager key can\'t keep the car awake. Pair again, then remove the old key in Controls → Locks → Keys.')) : null,
        el('div', { class: 'row' }, checkBtn), checkOut)));

  // side panel: what the key can do, and the car's state
  const stateOut = el('div');
  carStatusBox = el('div');
  renderCarStatus(car);
  const stateBtn = el('button', { type: 'button', class: 'btn-quiet', disabled: !hasVIN }, icon('refresh'), 'Ask the car');
  stateBtn.addEventListener('click', () => runStep(stateBtn, stateOut, 'Asking the car…', async () => {
    const res = await fetch('/api/car/state');
    const s = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(s.error || `HTTP ${res.status}`);
    return `The car is in Bluetooth range and ${s.state.toLowerCase()}.`;
  }));
  const forget = el('button', { type: 'button', class: 'btn-quiet', disabled: !hasVIN }, 'Forget this car');
  forget.addEventListener('click', async () => {
    if (!confirm('Forget this car? Remove the key in the car too (Controls → Locks → Keys).')) return;
    await send('POST', '/api/car/forget');
    showCar();
  });

  const why = el('div', { class: 'panel', style: 'margin-top:0' },
      el('h3', { style: 'margin:0 0 4px;font-size:16px' }, car.paired ? 'What the key does' : 'Why pair?',
        tip('A sleeping car cuts USB power. Glovebox keeps it awake while backing up, at most an hour.')),
      el('ul', { class: 'can-list' },
        el('li', { class: 'yes' }, icon('check'), 'Wakes the car'),
        el('li', { class: 'yes' }, icon('check'), 'Closes the charge port', tip('Nothing moves with a cable plugged in.')),
        el('li', { class: 'yes' }, icon('check'), 'Re-sets the same charge current')),
      el('p', { class: 'muted', style: 'margin:12px 0 0;font-size:13px' }, 'Turn on PIN to Drive. ',
        tip('The key has the Driver role, needed to keep the car awake. Someone with the Pi could drive off. Controls → Safety.')));
  const statusPanel = el('div', { class: 'panel', style: 'margin-top:0' },
    el('h3', { style: 'margin:0;font-size:16px' }, 'Status'),
    carStatusBox,
    el('div', { class: 'row', style: 'margin-top:12px' }, stateBtn, forget), stateOut);

  if (car.paired && !oldKey) {
    // set up: what's going on first; the steps only to change something
    page('car', 'Car', el('div', { class: 'car-layout' },
      el('div', { class: 'side' }, statusPanel,
        el('details', { class: 'panel fold' }, el('summary', {}, 'Change VIN or pair again'), steps)),
      el('div', { class: 'side' }, why)));
    return;
  }
  page('car', 'Car', el('div', { class: 'car-layout' }, steps, el('div', { class: 'side' }, why, statusPanel)));
}

// The Car page's status list and keep-awake button, refreshed with the
// status every 10 s.
let carStatusBox = null;

function renderCarStatus(car) {
  if (!carStatusBox) return;
  const k = car.keep_awake || {};
  let holdBtn = null;
  if (car.paired && car.keep_awake) {
    holdBtn = k.held_until
      ? el('button', { type: 'button' }, 'Stop keeping it awake')
      : el('button', { type: 'button', class: 'btn-primary' }, icon('bolt'), 'Keep awake for 30 min');
    holdBtn.addEventListener('click', async () => {
      holdBtn.disabled = true;
      try {
        await send('POST', '/api/car/keep-awake', { minutes: k.held_until ? 0 : 30 });
        status.car = await api('/api/car');
        renderCarStatus(status.car);
        renderChips();
      } catch (e) {
        holdBtn.disabled = false;
        alert(e.message);
      }
    });
  }
  carStatusBox.replaceChildren(
    el('dl', { class: 'kv' },
      el('dt', {}, 'VIN'), el('dd', {}, car.vin || '—'),
      el('dt', {}, 'Key'), el('dd', {}, car.paired ? 'Paired' : 'Not paired'),
      el('dt', {}, 'Keep-awake'), el('dd', { class: k.active ? 'live-text' : '' }, !car.paired ? 'Waiting for pairing'
        : k.active ? `Keeping the car awake: ${keepAwakeLine(k)}` : 'Idle'),
      el('dt', {}, 'Last nudge'), el('dd', {}, k.last_wake ? `${ago(new Date(k.last_wake))}${k.nudge ? `: ${k.nudge}` : ''}` : '—'),
      k.last_error ? [el('dt', {}, 'Last error'), el('dd', {}, k.last_error)] : null),
    holdBtn ? el('div', { class: 'row', style: 'margin-top:12px' }, holdBtn) : ''); // replaceChildren writes null as "null"
}

// ---------- encrypted drive ----------

// keyInput is a box for the user's private key, filled by typing, pasting
// or choosing their key file.
function keyInput() {
  const area = el('textarea', { rows: 3, placeholder: 'AGE-SECRET-KEY-1…', spellcheck: 'false', autocomplete: 'off' });
  const file = el('input', { type: 'file', accept: '.txt,text/plain' });
  file.addEventListener('change', async () => { if (file.files[0]) area.value = await file.files[0].text(); });
  return { area, node: el('div', {}, area, el('label', { class: 'field-hint', style: 'display:block;margin-top:6px' }, 'Or choose your key file: ', file)) };
}

let lockedBox = null; // the locked page's status, refreshed with the status

// showLockedDrive stands in for the footage pages while the encrypted drive
// waits for the car.
function showLockedDrive() {
  const k = keyInput();
  const out = el('div');
  const unlock = el('button', { class: 'btn btn-primary', type: 'button' }, 'Unlock');
  unlock.addEventListener('click', async () => {
    unlock.disabled = true;
    out.replaceChildren(updateResult('busy', 'Unlocking…'));
    try {
      await send('POST', '/api/encryption/unlock', { identity: k.area.value });
      k.area.value = '';
      out.replaceChildren(updateResult('ok', 'Unlocked. Starting the drive…'));
      setTimeout(refreshStatus, 1500);
    } catch (e) {
      out.replaceChildren(updateResult('bad', e.message));
      unlock.disabled = false;
    }
  });
  const box = el('div');
  page('locked', 'Drive locked', el('div', { class: 'setup-page' },
    el('div', { class: 'panel' },
      el('h2', { style: 'margin-top:0' }, 'The drive is locked'),
      el('p', {}, 'Waiting for the car over Bluetooth. ', tip('Until it\'s open, the car can\'t record.')),
      box),
    el('div', { class: 'panel' },
      el('h3', { style: 'margin-top:0' }, 'Unlock with your key'),
      el('p', { class: 'muted' }, 'Used once, not stored.'),
      k.node, el('div', { class: 'row', style: 'margin-top:12px' }, unlock), out)));
  lockedBox = box;
  renderLocked();
}

function renderLocked() {
  if (!lockedBox) return;
  const e = status.encryption || {};
  lockedBox.replaceChildren(el('dl', { class: 'kv' },
    el('dt', {}, 'Car check'), el('dd', {}, e.attempts ? `Tried ${e.attempts} time${e.attempts > 1 ? 's' : ''}, still trying` : 'Checking…'),
    e.last_error ? [el('dt', {}, 'Last try'), el('dd', {}, `${e.last_try ? `${ago(new Date(e.last_try))}: ` : ''}${e.last_error}`)] : null));
}

const UNLOCKED_BY = {
  car: 'when Glovebox saw the car', audit: 'without waiting for the car (audit mode)', key: 'with your key',
  restart: 'earlier while the car was awake', create: 'when it was encrypted',
};

// mountEncryption is the Settings panel for the encrypted drive.
function mountEncryption(box) {
  let st = null;
  let error = '';
  let pair = null; // a key pair made here, until it's in use
  let replacing = false;
  let mode = ''; // 'new' or 'own' while choosing a key

  async function load(method, path, body) {
    try {
      st = method ? await send(method, path, body) : await api('/api/encryption');
      error = '';
    } catch (e) {
      error = e.message;
      if (!st) { // no drive here
        box.replaceChildren(el('h3', {}, 'Drive encryption'), el('p', { class: 'muted' }, `Not available: ${error}`));
        return;
      }
    }
    if (box.isConnected) render();
  }

  function keyChooser() {
    const parts = [];
    if (mode === 'new' && pair) {
      const blob = new Blob([window.GloveboxAgeKey.keyFile(pair)], { type: 'text/plain' });
      const link = el('a', { class: 'btn', href: URL.createObjectURL(blob), download: 'glovebox-key.txt' }, icon('download'), 'Download your key file');
      const saved = el('input', { type: 'checkbox' });
      const use = el('button', { class: 'btn btn-primary', type: 'button', disabled: true }, 'Use this key');
      saved.addEventListener('change', () => { use.disabled = !saved.checked; });
      use.addEventListener('click', async () => {
        use.disabled = true;
        await load('PUT', '/api/encryption/recipient', { recipient: pair.publicKey });
        if (!error) { pair = null; mode = ''; replacing = false; render(); }
      });
      parts.push(
        el('p', {}, 'Save this file somewhere safe, off the Pi. ',
          tip('Made in this browser; only the public half goes to Glovebox. If the Pi dies, it\'s the only way to open the footage.')),
        el('div', { class: 'row' }, link),
        el('label', { class: 'check' }, saved, el('span', {}, 'I\'ve saved it')),
        el('div', { class: 'row' }, use));
    } else if (mode === 'own') {
      const input = el('input', { type: 'text', placeholder: 'age1…', spellcheck: 'false', autocomplete: 'off' });
      const save = el('button', { class: 'btn btn-primary', type: 'button' }, 'Use this key');
      save.addEventListener('click', async () => {
        save.disabled = true;
        await load('PUT', '/api/encryption/recipient', { recipient: input.value });
        if (!error) { mode = ''; replacing = false; render(); } else save.disabled = false;
      });
      parts.push(el('label', { class: 'field' }, el('span', { class: 'field-label' }, 'Your age public key'), input,
        el('span', { class: 'field-hint' }, 'The public key line from age-keygen.')),
      el('div', { class: 'row', style: 'margin-top:10px' }, save));
    } else {
      const make = el('button', { class: 'btn btn-primary', type: 'button' }, 'Create a key');
      make.addEventListener('click', () => { pair = window.GloveboxAgeKey.generate(); mode = 'new'; render(); });
      const own = el('button', { class: 'btn', type: 'button' }, 'Use my own age key');
      own.addEventListener('click', () => { mode = 'own'; render(); });
      parts.push(el('div', { class: 'row' }, make, own));
    }
    if (st.recipient) {
      const cancel = el('button', { class: 'btn', type: 'button' }, 'Keep the current key');
      cancel.addEventListener('click', () => { replacing = false; mode = ''; pair = null; render(); });
      parts.push(el('div', { class: 'row', style: 'margin-top:10px' }, cancel));
    }
    return parts;
  }

  function keySection(canReplace) {
    if (!st.recipient || replacing) return keyChooser();
    const parts = [el('dl', { class: 'kv' }, el('dt', {}, 'Public key'),
      el('dd', { style: 'word-break:break-all;font-family:var(--mono);font-size:13px' }, st.recipient))];
    if (canReplace) {
      const replace = el('button', { class: 'btn', type: 'button' }, 'Replace key');
      replace.addEventListener('click', () => { replacing = true; render(); });
      parts.push(el('div', { class: 'row', style: 'margin-top:10px' }, replace));
    }
    return parts;
  }

  function encryptSection() {
    const r = st.ready || {};
    const item = (ok, text) => updateResult(ok ? 'ok' : 'bad', text);
    const word = el('input', { type: 'text', placeholder: 'ERASE', autocomplete: 'off' });
    const go = el('button', { class: 'btn btn-primary', type: 'button', disabled: !(r.key && r.car && r.archived) }, 'Encrypt the drive');
    go.addEventListener('click', async () => {
      if (!confirm('Erase everything on the card and start again encrypted?')) return;
      go.disabled = true;
      await load('POST', '/api/encryption/encrypt', { confirm: word.value.trim() });
      if (!error) waitForRestart();
      else go.disabled = false;
    });
    return [
      el('h4', {}, 'Encrypt the drive'),
      el('div', { class: 'check-steps' },
        item(r.key, 'Key set'),
        item(r.car, 'Car paired'),
        item(r.archived, 'Backed up')),
      el('p', {}, el('b', {}, 'Erases the card.'), el('span', { class: 'muted' }, ' The car records again a minute later.')),
      el('label', { class: 'field', style: 'max-width:220px' }, el('span', { class: 'field-label' }, 'Type ERASE to confirm'), word),
      el('div', { class: 'row', style: 'margin-top:10px' }, go),
    ];
  }

  async function waitForRestart() {
    box.replaceChildren(el('h3', {}, 'Drive encryption'), updateResult('busy', 'Restarting to encrypt…'));
    const until = Date.now() + 5 * 60 * 1000;
    while (box.isConnected && Date.now() < until) {
      await new Promise((res) => setTimeout(res, 4000));
      try {
        const now = await api('/api/encryption');
        if (now.encrypted || now.encrypt_error) { st = now; error = ''; render(); refreshStatus(); return; }
      } catch { /* restarting */ }
    }
    if (box.isConnected) { error = 'Not back yet. Reload in a minute.'; render(); }
  }

  function render() {
    const parts = [el('h3', {}, 'Drive encryption')];
    if (!st.encrypted) {
      parts.push(el('p', { class: 'muted' }, 'Encrypt the card with your own key. ',
        tip('Clips still play here and in the car. The drive opens when Glovebox sees your car over Bluetooth. Turn off the car\'s own Encrypt Dashcam Recordings (Controls → Safety).')));
      if (st.encrypt_requested) parts.push(updateResult('busy', 'Encrypting at the next restart…'));
      if (st.encrypt_error) parts.push(updateResult('bad', `Encrypting failed: ${st.encrypt_error}`));
      parts.push(el('h4', {}, 'Your key'), ...keySection(true));
      if (st.recipient && !replacing) parts.push(...encryptSection());
    } else {
      const unlocked = st.state === 'unlocked';
      parts.push(el('dl', { class: 'kv' },
        el('dt', {}, 'Drive'), el('dd', {}, unlocked
          ? `Encrypted, unlocked ${UNLOCKED_BY[st.unlocked_by] || ''}${st.unlocked_at ? ` (${ago(new Date(st.unlocked_at))})` : ''}`
          : 'Encrypted, locked: waiting for the car'),
        el('dt', {}, 'Car check'), el('dd', {}, st.car_check === 'audit' ? 'Audit only' : 'On',
          tip(st.car_check === 'audit' ? 'Checked and logged; the drive opens either way.' : 'The drive opens only once the car is seen.')),
        st.check_seconds ? [el('dt', {}, 'Last check took'), el('dd', {}, `${st.check_seconds.toFixed(1)} s`)] : null),
      el('h4', {}, 'Your key'), ...keySection(unlocked));
      if (!unlocked) parts.push(el('p', { class: 'muted' }, 'Unlock to change the key.'));
    }
    if (error) parts.push(updateResult('bad', error));
    box.replaceChildren(...parts);
  }

  load();
}

// ---------- storage ----------

const fmtHours = (h) => (h < 1 ? `${Math.max(0, Math.round(h * 60))} min` : `${h.toFixed(1)} h`);

// tip is a small "?" that shows a short explanation when tapped.
function tip(text) {
  return el('details', { class: 'tip' }, el('summary', { 'aria-label': 'More info' }, '?'), el('div', { class: 'tip-body' }, text));
}

// pill is a short status tag.
function pill(state, text) {
  return el('span', { class: `pill pill-${state}` }, text);
}

// usageBar draws a storage bar like a phone's: one colored segment per use.
function usageBar(parts, total, thin) {
  const shown = parts.filter((p) => p.bytes > 0);
  return el('div', { class: `usage-bar${thin ? ' thin' : ''}`, role: 'img',
    'aria-label': shown.map((p) => `${p.label} ${gb(p.bytes)}`).join(', ') },
  shown.map((p) => el('span', { class: `usage-seg u-${p.key}`, title: `${p.label}: ${gb(p.bytes)}`,
    style: `width:${(100 * p.bytes / total).toFixed(2)}%` })));
}

// usageRows lists the parts under the bar: dot, name, size, and a tip.
function usageRows(parts) {
  return el('ul', { class: 'rows' }, parts.filter((p) => p.bytes > 0 || p.always).map((p) => el('li', {},
    el('span', { class: `usage-dot u-${p.key}` }),
    el('span', { class: 'row-name' }, p.label, p.tip ? tip(p.tip) : null, p.sub ? el('small', {}, p.sub) : null),
    el('span', { class: 'row-value' }, p.value || gb(p.bytes)))));
}

// mountStorage is Settings → Storage: how the card is used, and how much
// driving the Pi keeps until it's backed up. That's the size of the car's
// drive: the car records over all of its drive, so only the rest of the
// card can keep drives.
function mountStorage(box) {
  let st = null;
  let error = '';
  let hours = null; // the slider, while the user moves it
  let confirming = false;

  async function load(method, path, body) {
    try {
      st = method ? await send(method, path, body) : await api('/api/storage');
      error = '';
    } catch (e) {
      error = e.message;
      if (!st) {
        box.replaceChildren(el('h3', {}, 'Storage'), el('p', { class: 'muted' }, `Not available: ${error}`));
        return;
      }
    }
    if (box.isConnected) render();
  }

  const extrasBytes = () => st.extras.reduce((n, x) => n + x.size, 0);
  const room = (car) => Math.max(0, st.total - car - extrasBytes() - st.min_free);
  const hoursFor = (car) => room(car) / st.rate;
  const carFor = (h) => {
    const car = Math.floor((st.total - extrasBytes() - st.min_free - h * st.rate) / 2 ** 30) * 2 ** 30;
    return Math.min(st.max_car_size, Math.max(st.min_car_size, car));
  };
  const extraName = (x) => `${x.label[0]}${x.label.slice(1).toLowerCase()}`;

  function parts() {
    const c = st.car;
    const k = st.kept;
    const out = [];
    if (c.known) {
      out.push(
        { key: 'recent', label: 'Car recordings', bytes: c.recent + c.other, tip: 'The car\'s own last hour.' },
        { key: 'sentry', label: 'Sentry', bytes: c.sentry },
        { key: 'saved', label: 'Saved', bytes: c.saved },
        { key: 'carfree', label: 'Car free space', bytes: Math.max(0, c.size - c.recent - c.other - c.sentry - c.saved),
          tip: 'Empty to the car, but it records over all of it, so Glovebox can\'t use it.' });
    } else {
      out.push({ key: 'recent', label: 'Car drive', bytes: c.size });
    }
    for (const x of st.extras) out.push({ key: 'extras', label: extraName(x), bytes: x.size });
    // kept clips only cost space once the car has recorded over its own copy
    const onCard = Math.min(k.bytes, st.outside);
    const pending = k.minutes - k.archived_minutes;
    out.push({ key: 'kept', label: 'Kept drives', bytes: onCard, always: true,
      value: k.minutes ? fmtHours(k.minutes / 60) : 'None',
      sub: pending ? `${fmtHours(pending / 60)} not backed up` : (k.minutes ? 'All backed up' : ''),
      tip: 'Driving kept after the car moves on, until it\'s backed up. Parked time is skipped.' });
    const taken = out.reduce((n, p) => n + p.bytes, 0);
    out.push({ key: 'work', label: 'System', bytes: Math.max(0, st.total - taken - st.free), tip: 'Snapshot and working space.' },
      { key: 'free', label: 'Free', bytes: st.free, always: true });
    return out;
  }

  function plan(car) {
    const c = st.car;
    const events = c.sentry + c.saved;
    return [
      { key: 'recent', label: 'Car recordings', bytes: Math.min(st.rate, car) },
      { key: 'sentry', label: 'Sentry and saved', bytes: Math.min(events, Math.max(0, car - st.rate)) },
      { key: 'carfree', label: 'Car free space', bytes: Math.max(0, car - st.rate - events) },
      ...st.extras.map((x) => ({ key: 'extras', label: extraName(x), bytes: x.size })),
      { key: 'kept', label: 'Kept drives', bytes: room(car) },
      { key: 'work', label: 'System', bytes: st.min_free },
    ];
  }

  function keepSection() {
    if (st.recreate) {
      const cancel = el('button', { class: 'btn', type: 'button' }, 'Cancel');
      cancel.addEventListener('click', () => load('DELETE', '/api/storage/resize'));
      return [el('div', { class: 'row-line' }, pill('busy', `Car drive → ${gb(st.recreate)} on restart`), cancel)];
    }
    if (st.max_car_size < st.min_car_size) return [el('p', { class: 'muted' }, 'Card too small to keep extra driving.')];
    const lo = hoursFor(st.max_car_size);
    const hi = hoursFor(st.min_car_size);
    if (hours === null) hours = Math.min(hi, Math.max(lo, hoursFor(st.car.size)));
    // The slider stays put while it moves (replacing it would end a touch
    // drag); only what depends on it is redrawn.
    const slider = el('input', { type: 'range', min: lo.toFixed(2), max: hi.toFixed(2), step: '0.05', value: String(hours),
      'aria-label': 'Hours of driving to keep' });
    const value = el('span', { class: 'keep-value' });
    const preview = el('div');
    const note = el('div', { class: 'muted keep-note' });
    const actions = el('div');
    const update = () => {
      const car = carFor(hours);
      value.textContent = fmtHours(hoursFor(car));
      preview.replaceChildren(usageBar(plan(car), st.total, true));
      const changed = Math.abs(car - st.car.size) >= 2 ** 30;
      note.textContent = `Car drive ${gb(car)}${changed ? ` · now ${gb(st.car.size)}` : ' · current'}`;
      actions.replaceChildren(...(changed ? applyControls(car) : []));
    };
    slider.addEventListener('input', () => {
      hours = Number(slider.value);
      confirming = false;
      update();
    });
    update();
    return [
      el('div', { class: 'keep-head' },
        el('h4', {}, 'Keep driving', tip(`More kept driving makes the car drive smaller. The car still keeps its last hour and its events. Your car records about ${gb(st.rate)} an hour.`)),
        value),
      slider,
      el('div', { class: 'keep-scale muted' }, el('span', {}, fmtHours(lo)), el('span', {}, fmtHours(hi))),
      preview, note, actions,
    ];
  }

  function applyControls(car) {
    const ready = st.archived || !st.backups;
    if (!confirming) {
      const apply = el('button', { class: 'btn btn-primary', type: 'button', disabled: !ready }, 'Apply');
      apply.addEventListener('click', () => { confirming = true; render(); });
      return [el('div', { class: 'row-line' }, apply, ready ? null : pill('warn', 'Waiting for backup'))];
    }
    const word = el('input', { type: 'text', placeholder: 'Type ERASE', autocomplete: 'off', 'aria-label': 'Type ERASE to confirm' });
    const go = el('button', { class: 'btn btn-danger', type: 'button', disabled: true }, 'Erase and resize');
    word.addEventListener('input', () => { go.disabled = word.value.trim() !== 'ERASE'; });
    const cancel = el('button', { class: 'btn', type: 'button' }, 'Cancel');
    cancel.addEventListener('click', () => { confirming = false; render(); });
    go.addEventListener('click', async () => {
      go.disabled = true;
      await load('POST', '/api/storage/resize', { size: car, confirm: word.value.trim() });
      if (!error) waitForRestart(car);
    });
    return [el('div', { class: 'confirm-box' },
      el('div', {}, el('b', {}, 'Erases the car drive.'), el('span', { class: 'muted' }, st.backups ? ' Kept drives stay.' : ' Nothing is backed up.')),
      el('div', { class: 'row-line' }, word, go, cancel))];
  }

  async function waitForRestart(car) {
    box.replaceChildren(el('h3', {}, 'Storage'), pill('busy', 'Restarting with the new car drive…'));
    const until = Date.now() + 5 * 60 * 1000;
    while (box.isConnected && Date.now() < until) {
      await new Promise((res) => setTimeout(res, 4000));
      try {
        const now = await api('/api/storage');
        if (!now.recreate && Math.abs(now.car.size - car) < 2 ** 30) {
          st = now; hours = null; confirming = false; error = '';
          render(); refreshStatus();
          return;
        }
      } catch { /* restarting */ }
    }
    if (box.isConnected) { error = 'Not back yet. Reload in a minute.'; render(); }
  }

  function render() {
    const p = parts();
    box.replaceChildren(
      el('div', { class: 'storage-head' }, el('h3', {}, 'Storage'),
        el('span', { class: 'muted' }, el('b', {}, gb(st.total - st.free)), ` of ${gb(st.total)} used`)),
      usageBar(p, st.total),
      usageRows(p),
      el('hr', { class: 'sep' }),
      ...keepSection(),
      error ? pill('bad', error) : '');
  }

  load();
}

// ---------- setup ----------

function showSetup(initialTab = 'backups') {
  const sections = [
    ['backups', 'Backups', 'Choose where clips go and test the connection.'],
    ['drives', 'Extra drives', 'Make room for music, light shows, and Boombox sounds.'],
    ['storage', 'Storage', 'See what uses the card and choose how much driving to keep.'],
    ['security', 'Security', 'Manage drive encryption and your recovery key.'],
    ['updates', 'Updates', 'Review new releases and choose when to install.'],
  ];
  const panels = Object.fromEntries(sections.map(([id]) => [id,
    el('section', { id: 'settings-panel-' + id, role: 'tabpanel', 'aria-labelledby': 'settings-tab-' + id, tabindex: '0', class: 'settings-panel' })]));
  const save = el('div', { class: 'settings-save' });
  const tabs = el('div', { class: 'settings-tabs', role: 'tablist', 'aria-label': 'Settings sections' });
  let selected = sections.some(([id]) => id === initialTab) ? initialTab : 'backups';
  function select(id, focus = false) {
    selected = id;
    for (const button of tabs.children) {
      const active = button.dataset.section === id;
      button.setAttribute('aria-selected', String(active));
      button.tabIndex = active ? 0 : -1;
      if (active && focus) button.focus();
    }
    for (const [key, panel] of Object.entries(panels)) panel.hidden = key !== id;
    save.hidden = !['backups', 'drives'].includes(id);
    // Updating the address without routing keeps all unsaved form controls alive.
    history.replaceState(null, '', '#/settings/' + id);
  }
  sections.forEach(([id, label, description], i) => {
    const button = el('button', { type: 'button', role: 'tab', id: 'settings-tab-' + id,
      'data-section': id, 'aria-controls': panels[id].id, title: description }, label);
    button.addEventListener('click', () => select(id));
    button.addEventListener('keydown', event => {
      let next;
      if (event.key === 'ArrowRight') next = (i + 1) % sections.length;
      else if (event.key === 'ArrowLeft') next = (i + sections.length - 1) % sections.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = sections.length - 1;
      else return;
      event.preventDefault();
      select(sections[next][0], true);
    });
    tabs.append(button);
  });
  page('settings', 'Settings', el('div', { class: 'setup-page settings-page' },
    el('div', { class: 'settings-top' }, tabs, el('a', { class: 'btn', href: '#/welcome' }, 'Guided setup')),
    ...Object.values(panels), save));
  select(selected);
  window.GloveboxSetup.mount(panels.backups, {
    onSaved: refreshStatus,
    onError: error => { panels.drives.replaceChildren(updateResult('bad', "Couldn’t load drive settings: " + error.message)); },
    onSections: groups => {
      panels.backups.replaceChildren(...groups.backups);
      panels.drives.replaceChildren(...groups.drives);
      groups.save.querySelector('p').prepend('Saves changes from both Backups and Extra drives. ');
      save.replaceChildren(groups.save);
    },
  });
  panels.drives.append(loading());
  const sto = el('div', { class: 'panel update' });
  panels.storage.append(sto);
  mountStorage(sto);
  const enc = el('div', { class: 'panel update' });
  const upd = el('div', { class: 'panel update' });
  panels.security.append(enc);
  panels.updates.append(upd);
  mountEncryption(enc);
  mountUpdate(upd);
}

// ---------- software update ----------

let updateInfo = null; // the last /api/update answer, shared by all views
let updateCheckPending = false;
let updateCheckAttempted = 0;
let dismissedUpdateVersion = '';
try { dismissedUpdateVersion = localStorage.getItem('glovebox-dismissed-update') || ''; } catch { /* Storage can be disabled. */ }

function renderUpdateNotice() {
  const notice = $('#update-notice');
  const available = updateInfo?.available && updateInfo.latest && updateInfo.state === 'idle'
    && updateInfo.latest.version !== dismissedUpdateVersion;
  notice.hidden = !available;
  notice.replaceChildren(...(available ? [
    el('div', {}, el('strong', {}, 'Glovebox ' + updateInfo.latest.version + ' is available.'),
      el('span', {}, ' Review what’s new and update when you’re ready.')),
    el('a', { class: 'btn update-review', href: '#/settings/updates' }, 'Review update →'),
    el('button', { type: 'button', class: 'update-dismiss', 'aria-label': 'Dismiss update notification',
      title: 'Dismiss this release notification', onclick: () => {
        dismissedUpdateVersion = updateInfo.latest.version;
        try { localStorage.setItem('glovebox-dismissed-update', dismissedUpdateVersion); } catch { /* Keep it dismissed for this visit. */ }
        renderUpdateNotice();
      } }, '×'),
  ] : []));
}

async function checkForUpdates() {
  // Resume checks after an offline/hidden session without flooding a slow device.
  if (document.hidden || updateCheckPending || Date.now() - updateCheckAttempted < 5 * 60 * 1000) return;
  updateCheckPending = true;
  updateCheckAttempted = Date.now();
  try {
    updateInfo = await api('/api/update?check=1');
    renderFoot();
    renderUpdateNotice();
  } catch { /* Offline, or this build has no updater: try again later. */ }
  finally { updateCheckPending = false; }
}

// notes renders the little Markdown that release notes use: ### headings,
// - bullets, **bold**, `code` and links (as their text).
function notes(md) {
  const inline = (t) => t.replace(/\[([^\]]+)\]\([^)]+\)/g, '$1').split(/(\*\*[^*]+\*\*|`[^`]+`)/).filter(Boolean).map((part) =>
    part.startsWith('**') ? el('b', {}, part.slice(2, -2)) : part.startsWith('`') ? el('code', {}, part.slice(1, -1)) : part);
  const out = [];
  let list = null;
  for (const line of md.split('\n')) {
    const t = line.trim();
    if (t.startsWith('- ')) {
      if (!list) out.push(list = el('ul'));
      list.append(el('li', {}, inline(t.slice(2))));
      continue;
    }
    list = null;
    if (!t || /^##\s+what's new$/i.test(t)) continue;
    if (t.startsWith('#')) out.push(el('h4', {}, t.replace(/^#+\s*/, '')));
    else out.push(el('p', {}, inline(t)));
  }
  return el('div', { class: 'notes' }, out);
}

function updateResult(state, msg) {
  const mark = state === 'busy' ? el('span', { class: 'spinner' }) : el('span', { class: 'mark' }, state === 'ok' ? '✓' : '✕');
  return el('div', { class: `result ${state}` }, mark, el('span', {}, msg));
}

// mountUpdate shows the running version, checks GitHub for a newer one and
// installs it.
function mountUpdate(box) {
  let u = null;
  let error = '';
  let timer = null;
  const alive = () => box.isConnected;

  async function load(path, method) {
    try {
      u = method ? await send(method, path) : await api(path);
      error = '';
    } catch (e) {
      error = e.message;
    }
    if (u) updateInfo = u;
    renderFoot();
    renderUpdateNotice();
    if (alive()) render();
  }

  // Glovebox restarts after the install: wait for the new version to
  // answer, then load its page.
  async function waitForRestart(from) {
    const until = Date.now() + 5 * 60 * 1000;
    while (alive() && Date.now() < until) {
      await new Promise((r) => setTimeout(r, 3000));
      try {
        const st = await api('/api/status');
        if (st.version !== from) {
          box.replaceChildren(el('h3', {}, 'Software update'), updateResult('ok', `Updated to ${st.version}. Loading the new version…`));
          setTimeout(() => location.reload(), 1500);
          return;
        }
      } catch { /* still restarting */ }
    }
    if (alive()) {
      error = 'Not back yet. Reload in a minute, or unplug the Pi and plug it back in.';
      u = { ...u, state: 'idle' };
      render();
    }
  }

  function poll() {
    clearTimeout(timer);
    if (!alive() || !u) return;
    if (u.state === 'restarting') waitForRestart(u.current);
    else if (u.state === 'installing' || u.state === 'checking') timer = setTimeout(async () => { await load('/api/update'); poll(); }, 1500);
  }

  function render() {
    const head = el('div', { class: 'update-head' },
      el('div', {},
        el('h3', {}, 'Software update'),
        el('p', { class: 'muted' }, u ? `Version ${u.current}${u.release ? '' : ' (test build)'}` : 'Checking…')));
    const parts = [head];
    if (!u || !u.supported) {
      if (u) parts.push(el('p', { class: 'muted' }, `Glovebox can't update itself here: ${u.why}.`));
      if (error) parts.push(updateResult('bad', error));
      box.replaceChildren(...parts);
      return;
    }
    const busy = u.state === 'installing' || u.state === 'restarting';
    const check = el('button', { class: 'btn', disabled: busy || u.state === 'checking' }, 'Check for updates');
    check.addEventListener('click', async () => { u = { ...u, state: 'checking' }; render(); await load('/api/update/check', 'POST'); });
    head.append(check);

    if (u.state === 'checking') parts.push(updateResult('busy', 'Asking GitHub for the latest version…'));
    else if (u.state === 'installing') parts.push(updateResult('busy', `${u.step || 'Installing'}…`));
    else if (u.state === 'restarting') parts.push(updateResult('busy', `Restarting with ${u.latest?.version}…`));
    else if (u.available && u.latest) {
      const l = u.latest;
      const install = el('button', { class: 'btn btn-primary' }, `Install ${l.version}`);
      install.addEventListener('click', async () => {
        if (!confirm(`Install ${l.version}? Restarts in about a minute; the car keeps recording.`)) return;
        install.disabled = true;
        await load('/api/update/install', 'POST');
        poll();
      });
      parts.push(el('div', { class: 'update-new' },
        el('div', { class: 'update-title' }, el('b', {}, `Glovebox ${l.version} is available`),
          l.published ? el('span', { class: 'muted' }, ` · ${fmtDay(new Date(l.published))}`) : null),
        l.notes ? notes(l.notes) : null,
        el('div', { class: 'row' }, install,
          l.url ? el('a', { href: l.url, target: '_blank', rel: 'noopener' }, 'Release notes on GitHub') : null)));
    } else if (u.latest && !u.error) {
      parts.push(updateResult('ok', `Up to date${u.checked ? ` · checked ${ago(new Date(u.checked))}` : ''}`));
    }
    const err = error || u.error;
    if (err && !busy) parts.push(updateResult('bad', err));
    box.replaceChildren(...parts);
  }

  render();
  load('/api/update?check=1').then(poll);
}

function showWelcome() {
  const box = el('div', { class: 'setup-page' });
  page('settings', 'Guided setup', box);
  window.GloveboxSetup.wizard(box, { onFinish: () => { location.hash = '#/'; refreshStatus(); } });
}

// The first visit before Glovebox is set up starts the wizard.
async function firstRun() {
  if (location.hash && location.hash !== '#/') return false;
  try {
    const w = await api('/api/wifi');
    if (!w.setup_done) {
      location.hash = '#/welcome'; // routes it
      return true;
    }
  } catch { /* Wi-Fi isn't managed here */ }
  return false;
}

// ---------- routing ----------

function route() {
  const [, view, a, b, c] = location.hash.split('/').map(decodeURIComponent);
  lockedBox = null;
  // While the encrypted drive is locked there's no footage to show; the
  // Car and Settings pages still work.
  if (driveLocked() && !['car', 'settings', 'setup', 'welcome'].includes(view)) return showLockedDrive();
  if (view === 'event' && KIND[a] && b) return showEvent(a, b, c === 'enc');
  if ((view === 'drive' || view === 'recent') && a) return showDrive(a, b === 'enc');
  if (view === 'recent') return showRecent();
  if (KIND[view]) return showEvents(view);
  if (view === 'car') return showCar();
  if (view === 'settings' || view === 'setup') return showSetup(a); // #/setup: before 0.4
  if (view === 'welcome') return showWelcome();
  return showOverview();
}

window.addEventListener('hashchange', route);
// the encryption status first, so a locked drive shows at once
api('/api/encryption').then((e) => { status.encryption = e; }).catch(() => {})
  .then(firstRun).then((moved) => { if (!moved) route(); });
refreshStatus();
setInterval(refreshStatus, 10000);
// Ask the existing cached release checker even if Settings is never opened.
checkForUpdates();
setInterval(checkForUpdates, 60 * 60 * 1000);
window.addEventListener('online', checkForUpdates);
document.addEventListener('visibilitychange', () => { if (!document.hidden) checkForUpdates(); });

// An open tip closes when anything else is tapped, and when the page scrolls.
document.addEventListener('click', (e) => {
  for (const d of document.querySelectorAll('details.tip[open]')) if (!d.contains(e.target)) d.open = false;
});
document.addEventListener('scroll', () => {
  for (const d of document.querySelectorAll('details.tip[open]')) d.open = false;
}, { capture: true, passive: true });

// An opening tip is placed under its "?", kept inside the screen (above it
// if there's no room below).
document.addEventListener('toggle', (e) => {
  const d = e.target;
  if (!(d instanceof HTMLDetailsElement) || !d.classList.contains('tip') || !d.open) return;
  const body = d.querySelector('.tip-body');
  const q = d.querySelector('summary').getBoundingClientRect();
  const w = body.offsetWidth;
  const h = body.offsetHeight;
  const margin = 10;
  body.style.left = `${Math.max(margin, Math.min(q.left + q.width / 2 - w / 2, innerWidth - w - margin))}px`;
  body.style.top = `${q.bottom + 6 + h > innerHeight - margin ? q.top - 6 - h : q.bottom + 6}px`;
}, true);
