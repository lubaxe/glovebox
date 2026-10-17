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

function mediaURL(file, download) {
  const p = file.path.split('/').map(encodeURIComponent).join('/');
  return `/media/${encodeURIComponent(file.snapshot)}/${p}${download ? '?download=1' : ''}`;
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
  $('#view').replaceChildren(...nodes);
  window.scrollTo(0, 0);
}

function loading() { return el('p', { class: 'muted' }, 'Loading…'); }

function empty(iconName, title, text) {
  return el('div', { class: 'empty' }, icon(iconName), el('h3', {}, title), el('div', {}, text));
}

// ---------- status ----------

const status = { system: null, drive: null, archive: null, car: null };

async function refreshStatus() {
  const [st, dr, ar, car] = await Promise.allSettled([api('/api/status'), api('/api/drive'), api('/api/archive'), api('/api/car')]);
  status.system = st.status === 'fulfilled' ? st.value : null;
  status.drive = dr.status === 'fulfilled' ? dr.value : null;
  status.archive = ar.status === 'fulfilled' ? ar.value : null;
  status.car = car.status === 'fulfilled' ? car.value : null;
  renderChips();
  renderFoot();
  if (current === 'overview') renderOverviewStats();
}

function chip(text, state, title, minor) {
  return el('span', { class: `chip ${state || ''} ${minor ? 'minor' : ''}`, title }, el('span', { class: 'dot' }), text);
}

function usbState() {
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
  const nas = chip(n.text, n.state, `${n.detail}\nClick to look for new clips now.`);
  nas.classList.add('clickable');
  nas.addEventListener('click', async () => {
    await fetch('/api/archive/run', { method: 'POST' });
    setTimeout(refreshStatus, 1500);
  });
  chips.push(nas);
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
    updateInfo?.available && updateInfo.latest && updateInfo.state === 'idle'
      ? el('a', { class: 'update-link', href: '#/settings' }, `${updateInfo.latest.version} is available`) : '');
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
    cards.push(stat('', 'disk', 'Snapshot space', `${gb(d.backingfiles_free_bytes)} free`,
      'Older snapshots are cleared automatically when space runs low.'));
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
      let sub = 'Ready to keep the car awake while backing up';
      if (k.active) sub = 'Keeping the car awake for the backup';
      else if (k.last_error) sub = `Last try: ${k.last_error}`;
      else if (k.last_wake) sub = `Last kept awake ${ago(new Date(k.last_wake))}`;
      cards.push(stat(k.last_error ? 'warn' : 'ok', 'bolt', 'Keep-awake', 'Paired', sub, null, '#/car'));
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
      empty('film', 'No footage yet', 'Once the car records to Glovebox, its Sentry events, saved clips and driving footage show up here.'));
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
      locked ? el('p', { class: 'muted', style: 'margin:-6px 0 12px' },
        `${locked === minutes.length ? 'All of this footage is' : 'Some of this footage is'} encrypted by your car: open a drive to download it for dashcam.tesla.com.`) : null,
      el('div', { class: 'grid' }, ds.slice(0, 4).map(driveCard)));
    if (!events.length) {
      latest.append(el('p', { class: 'muted', style: 'margin-top:18px;font-size:14px' },
        'No Sentry or saved events yet. They show up here when Sentry Mode records something or you save a clip in the car.'));
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
    page('recent', 'Recent', empty('film', 'No recent clips yet', 'The car\'s rolling dashcam footage shows up here, minute by minute.'));
    return;
  }
  const nodes = [el('p', { class: 'muted', style: 'margin-top:0' },
    'The car keeps about an hour of driving footage and overwrites it as it goes. Glovebox keeps copies in its snapshots, and backs them up when the car is home.')];
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
      el('p', {}, 'Your Tesla has "Encrypt Dashcam Recordings" turned on, so only the car, or dashcam.tesla.com signed in to your Tesla account, can play these clips. Download them as one zip, unzip it, and drop the files on dashcam.tesla.com. Tesla\'s site only takes files from your computer, so Glovebox can\'t send them there directly.'),
      el('div', { class: 'actions' },
        go,
        el('a', { class: 'btn', href: 'https://dashcam.tesla.com', target: '_blank', rel: 'noopener' }, 'Just open the site', icon('external'))),
      el('p', { class: 'muted', style: 'margin:12px 0 0;font-size:13px' },
        'The download comes from the Pi over Wi-Fi, at about 3–5 MB/s. If these clips are already backed up, opening them from the EncryptedClips folder on your NAS is much faster.')));
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

    const set = new Set();
    for (const c of clips) for (const cam of Object.keys(c.cameras)) set.add(cam);
    this.cams = sortCameras(set);
    this.videos = {};
    $('#cams').replaceChildren(...this.cams.map((cam) => {
      const v = el('video', { muted: true, playsinline: true, preload: 'auto' });
      v.muted = true;
      v.addEventListener('timeupdate', () => this.onTime(v));
      v.addEventListener('ended', () => this.onEnded(v));
      v.addEventListener('loadedmetadata', () => { v.playbackRate = this.speed; });
      this.videos[cam] = v;
      return el('div', { class: 'cam', 'data-cam': cam, onclick: () => this.focus(cam) },
        v, el('span', { class: 'label' }, CAMERA_NAMES[cam] || cam));
    }));
    $('#clip-strip').replaceChildren(...clips.map((c, k) =>
      el('button', { type: 'button', onclick: () => this.load(k, this.playing) }, fmtTime(carTime(c.start)))));

    $('#play').addEventListener('click', () => (this.playing ? this.pause() : this.play()));
    $('#prev').addEventListener('click', () => this.i > 0 && this.load(this.i - 1, this.playing));
    $('#next').addEventListener('click', () => this.i + 1 < this.clips.length && this.load(this.i + 1, this.playing));
    $('#speed').value = String(this.speed);
    $('#speed').addEventListener('change', (e) => {
      this.speed = Number(e.target.value);
      for (const v of Object.values(this.videos)) v.playbackRate = this.speed;
    });
    const scrub = $('#scrub');
    scrub.max = clips.length * 60;
    scrub.value = start * 60;
    scrub.addEventListener('input', () => { this.scrubbing = true; });
    scrub.addEventListener('change', () => { this.scrubbing = false; this.seek(Number(scrub.value)); });
    this.load(start, false);
  },

  load(i, autoplay) {
    this.i = i;
    const clip = this.clips[i];
    this.master = null;
    for (const cam of this.cams) {
      const v = this.videos[cam];
      const box = v.parentElement;
      const file = clip.cameras[cam];
      v.pause();
      if (file) {
        v.src = mediaURL(file);
        box.classList.remove('missing');
        if (!this.master) this.master = v;
      } else {
        v.removeAttribute('src');
        v.load();
        box.classList.add('missing');
      }
    }
    for (const [k, b] of [...$('#clip-strip').children].entries()) b.classList.toggle('current', k === i);
    $('#downloads').replaceChildren(...this.cams.filter((c) => clip.cameras[c]).map((c) =>
      el('a', { href: mediaURL(clip.cameras[c], true) }, icon('download'), CAMERA_NAMES[c] || c)),
      this.zipURL ? el('a', { href: this.zipURL, download: '' }, icon('download'), `Everything (${gb(clipBytes(this.clips))} zip)`) : '');
    this.updateClock(0);
    if (autoplay) this.play();
  },

  setPlayIcon(name) {
    const b = $('#play');
    if (b) b.replaceChildren(icon(name));
  },

  play() {
    this.playing = true;
    this.setPlayIcon('pause');
    for (const v of Object.values(this.videos)) {
      if (v.getAttribute('src')) {
        v.playbackRate = this.speed;
        v.play().catch(() => {});
      }
    }
  },

  pause() {
    this.playing = false;
    this.setPlayIcon('play');
    for (const v of Object.values(this.videos)) v.pause();
  },

  stop() {
    this.playing = false;
    for (const v of Object.values(this.videos)) {
      v.pause();
      v.removeAttribute('src');
      v.load(); // stops downloading
    }
    this.videos = {};
    this.clips = [];
  },

  // keep the other cameras within a quarter second of the master
  onTime(v) {
    if (v !== this.master) return;
    for (const other of Object.values(this.videos)) {
      if (other !== v && other.getAttribute('src') && Math.abs(other.currentTime - v.currentTime) > 0.25) {
        other.currentTime = v.currentTime;
      }
    }
    if (!this.scrubbing) $('#scrub').value = this.i * 60 + v.currentTime;
    this.updateClock(v.currentTime);
  },

  onEnded(v) {
    if (v !== this.master) return;
    if (this.i + 1 < this.clips.length) this.load(this.i + 1, true);
    else this.pause();
  },

  seek(seconds) {
    const total = this.clips.length * 60;
    seconds = Math.max(0, Math.min(seconds, total - 0.1));
    const k = Math.min(Math.floor(seconds / 60), this.clips.length - 1);
    const offset = seconds - k * 60;
    const apply = () => {
      for (const v of Object.values(this.videos)) {
        if (v.getAttribute('src')) v.currentTime = Math.min(offset, (v.duration || 60) - 0.1);
      }
    };
    if (k !== this.i) {
      const playing = this.playing;
      this.load(k, false);
      this.master?.addEventListener('loadedmetadata', () => { apply(); if (playing) this.play(); }, { once: true });
    } else {
      apply();
    }
  },

  updateClock(offset) {
    const clip = this.clips[this.i];
    if (clip) $('#clock').textContent = fmtClock(new Date(carTime(clip.start).getTime() + offset * 1000));
  },

  focus(cam) {
    const cams = $('#cams');
    const box = cams.querySelector(`[data-cam="${cam}"]`);
    const already = box.classList.contains('main');
    for (const b of cams.children) { b.classList.remove('main'); b.style.order = ''; }
    cams.classList.toggle('focused', !already);
    if (!already) { box.classList.add('main'); box.style.order = '-1'; }
  },
};

document.addEventListener('keydown', (e) => {
  if (!player.clips.length || e.target.closest('input, select, textarea')) return;
  if (e.key === ' ') { e.preventDefault(); player.playing ? player.pause() : player.play(); }
  if (e.key === 'ArrowRight') player.seek(player.i * 60 + (player.master?.currentTime || 0) + 5);
  if (e.key === 'ArrowLeft') player.seek(player.i * 60 + (player.master?.currentTime || 0) - 5);
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
    page(name, 'Recent', empty('film', 'That drive is gone', 'Its snapshots have been cleared to make room.'));
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
  // the console from above: cupholders, then the phone pad with a card on it
  const svg = new DOMParser().parseFromString(`
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 150" class="pad-art" role="img" aria-label="Key card laid flat on the phone charging pad behind the cupholders">
  <rect x="96" y="12" width="168" height="126" rx="22" fill="none" stroke="currentColor" stroke-opacity=".25" stroke-width="2"/>
  <circle cx="148" cy="44" r="18" fill="none" stroke="currentColor" stroke-opacity=".3" stroke-width="2"/>
  <circle cx="212" cy="44" r="18" fill="none" stroke="currentColor" stroke-opacity=".3" stroke-width="2"/>
  <text x="180" y="78" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity=".45">cupholders</text>
  <rect x="122" y="86" width="116" height="44" rx="8" fill="currentColor" fill-opacity=".07" stroke="currentColor" stroke-opacity=".3" stroke-width="2"/>
  <g transform="rotate(-6 180 108)">
    <rect x="146" y="92" width="68" height="32" rx="5" fill="#e8403a"/>
    <rect x="154" y="112" width="22" height="4" rx="2" fill="#fff" fill-opacity=".85"/>
  </g>
  <text x="300" y="104" font-size="11" fill="currentColor" fill-opacity=".65">phone pad</text>
  <path d="M292 100h-50" stroke="currentColor" stroke-opacity=".4" stroke-width="1.5"/>
  <text x="18" y="114" font-size="11" fill="currentColor" fill-opacity=".65">key card</text>
  <path d="M70 110h72" stroke="currentColor" stroke-opacity=".4" stroke-width="1.5"/>
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
  const pairBtn = el('button', { type: 'button', class: car.paired ? '' : 'btn-primary', disabled: !hasVIN }, icon('card'), 'Send pairing request');
  pairBtn.addEventListener('click', () => runStep(pairBtn, pairOut, 'Talking to the car over Bluetooth…', async () => {
    await send('POST', '/api/car/pair');
    return 'Sent. Now lay your key card on the phone pad and confirm on the car\'s screen.';
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
        el('h3', {}, 'Your car\'s VIN'),
        el('p', {}, 'Find it in the car under Controls → Software, or in the Tesla app. Glovebox uses it to find your car over Bluetooth.'),
        el('div', { class: 'row' }, vin, vinBtn), vinOut)),
    el('div', { class: stepState(car.paired, hasVIN && !car.paired, hasVIN) },
      el('span', { class: 'step-num' }, car.paired ? icon('check') : '2'),
      el('div', {},
        el('h3', {}, 'Pair with a key card'),
        el('p', {}, 'Sit in the car with the Pi plugged in and one of your key cards. Then:'),
        el('ol', {},
          el('li', {}, 'Press ', el('b', {}, 'Send pairing request'), '. Nothing shows on the car\'s screen yet.'),
          el('li', {}, 'Within 30 seconds, lay the ', el('b', {}, 'key card flat on the phone charging pad'), ' behind the cupholders (the card reader is underneath) and hold it for a moment.'),
          el('li', {}, 'The car asks to confirm a new key. Check that it\'s a ', el('b', {}, 'limited key'), ', then accept.')),
        padArt(),
        el('div', { class: 'row' }, pairBtn), pairOut)),
    el('div', { class: stepState(car.paired, false, hasVIN) },
      el('span', { class: 'step-num' }, car.paired ? icon('check') : '3'),
      el('div', {},
        el('h3', {}, car.paired ? 'Paired' : 'Check that it worked'),
        el('p', {}, car.paired
          ? `The car accepted Glovebox's key${car.paired_at ? ` on ${new Date(car.paired_at).toLocaleDateString()}` : ''}. Keep-awake is active.`
          : 'Glovebox asks the car for its charge level using the new key. That only works once the car has accepted it.'),
        el('div', { class: 'row' }, checkBtn), checkOut)));

  // side panel: what the key can do, and the car's state
  const k = car.keep_awake || {};
  const stateOut = el('div');
  const stateBtn = el('button', { type: 'button', class: 'btn-quiet', disabled: !hasVIN }, icon('refresh'), 'Ask the car');
  stateBtn.addEventListener('click', () => runStep(stateBtn, stateOut, 'Asking the car…', async () => {
    const res = await fetch('/api/car/state');
    const s = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(s.error || `HTTP ${res.status}`);
    return `The car is in Bluetooth range and ${s.state.toLowerCase()}.`;
  }));
  const forget = el('button', { type: 'button', class: 'btn-quiet', disabled: !hasVIN }, 'Forget this car');
  forget.addEventListener('click', async () => {
    if (!confirm('Forget the VIN and pairing here? The key stays in the car until you remove it under Controls → Locks → Keys.')) return;
    await send('POST', '/api/car/forget');
    showCar();
  });

  const side = el('div', { class: 'side' },
    el('div', { class: 'panel', style: 'margin-top:0' },
      el('h3', { style: 'margin:0 0 4px;font-size:16px' }, 'Why pair?'),
      el('p', { class: 'muted', style: 'margin:0' }, 'A sleeping car turns off its USB ports, which would cut an upload short. While Glovebox backs up what the car recorded away from home, it tells the car to stay awake, for at most an hour.'),
      el('ul', { class: 'can-list' },
        el('li', { class: 'yes' }, icon('check'), 'Wake the car'),
        el('li', { class: 'yes' }, icon('check'), 'Read its charge level'),
        el('li', { class: 'no' }, icon('lock'), 'Unlock or open anything'),
        el('li', { class: 'no' }, icon('lock'), 'Start or drive the car')),
      el('p', { class: 'muted', style: 'margin:12px 0 0;font-size:13px' }, 'The key has the Charging Manager role, so the car itself refuses anything else. Remove it any time under Controls → Locks → Keys.')),
    el('div', { class: 'panel', style: 'margin-top:0' },
      el('h3', { style: 'margin:0;font-size:16px' }, 'Status'),
      el('dl', { class: 'kv' },
        el('dt', {}, 'VIN'), el('dd', {}, car.vin || '—'),
        el('dt', {}, 'Key'), el('dd', {}, car.paired ? 'Paired' : 'Not paired'),
        el('dt', {}, 'Keep-awake'), el('dd', {}, !car.paired ? 'Waiting for pairing' : k.active ? 'Keeping the car awake' : 'Idle'),
        el('dt', {}, 'Last wake'), el('dd', {}, k.last_wake ? `${ago(new Date(k.last_wake))}${k.car_state ? ` (car ${k.car_state.toLowerCase()})` : ''}` : '—'),
        k.last_error ? [el('dt', {}, 'Last error'), el('dd', {}, k.last_error)] : null),
      el('div', { class: 'row', style: 'margin-top:12px' }, stateBtn, forget), stateOut));

  page('car', 'Car', el('div', { class: 'car-layout' }, steps, side));
}

// ---------- setup ----------

function showSetup() {
  const box = el('div');
  const upd = el('div', { class: 'panel update' });
  page('settings', 'Settings', el('div', { class: 'setup-page' },
    el('div', { class: 'welcome' },
      el('div', {},
        el('h2', {}, 'All settings'),
        el('p', {}, 'Or let the guided setup walk you through Wi-Fi, backups, extra drives and your car, skipping what\'s already set up.')),
      el('a', { class: 'btn', href: '#/welcome' }, 'Guided setup')),
    box, upd));
  window.GloveboxSetup.mount(box, { onSaved: refreshStatus });
  mountUpdate(upd);
}

// ---------- software update ----------

let updateInfo = null; // the last /api/update answer, for the sidebar

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
      error = 'Glovebox hasn\'t come back after the update. Give it a minute and reload the page; if it still doesn\'t answer, unplug the Pi and plug it back in.';
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
        el('p', { class: 'muted' }, u ? `This is Glovebox ${u.current}${u.release ? '' : ' (a test build)'}.` : 'Checking…')));
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
    else if (u.state === 'restarting') parts.push(updateResult('busy', `Restarting with ${u.latest?.version}. The car's drive stays connected.`));
    else if (u.available && u.latest) {
      const l = u.latest;
      const install = el('button', { class: 'btn btn-primary' }, `Install ${l.version}`);
      install.addEventListener('click', async () => {
        if (!confirm(`Install Glovebox ${l.version}? Glovebox restarts to finish, which takes about a minute. The car's drive stays connected.`)) return;
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
      parts.push(updateResult('ok', `Up to date: ${u.latest.version} is the latest version${u.checked ? ` (checked ${ago(new Date(u.checked))})` : ''}.`));
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
  if (view === 'event' && KIND[a] && b) return showEvent(a, b, c === 'enc');
  if ((view === 'drive' || view === 'recent') && a) return showDrive(a, b === 'enc');
  if (view === 'recent') return showRecent();
  if (KIND[view]) return showEvents(view);
  if (view === 'car') return showCar();
  if (view === 'settings' || view === 'setup') return showSetup(); // #/setup: before 0.4
  if (view === 'welcome') return showWelcome();
  return showOverview();
}

window.addEventListener('hashchange', route);
firstRun().then((moved) => { if (!moved) route(); });
refreshStatus();
setInterval(refreshStatus, 10000);
// what the last check found, for the sidebar (without asking GitHub again)
api('/api/update').then((u) => { updateInfo = u; renderFoot(); }).catch(() => {});
