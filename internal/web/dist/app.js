/* isdbd web UI — hand-written, no build step.
 *
 * Talks to the Go daemon's /api/* endpoints. Live playback uses hls.js
 * when available (Chrome/Firefox via MSE) and falls back to native HLS
 * (Safari/iOS). Everything else is plain fetch + DOM.
 */
'use strict';

// ── small helpers ──────────────────────────────────────────────────
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'dataset') Object.assign(node.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (v != null) node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c == null) continue;
    node.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((body && body.error) || res.statusText || ('HTTP ' + res.status));
  return body;
}

function toast(msg, kind = 'ok') {
  const t = el('div', { class: 'toast ' + kind }, msg);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), 3500);
}

const fmtTime = (s) => s ? new Date(s).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
const fmtDate = (s) => s ? new Date(s).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
function fmtDur(secs) {
  if (!secs) return '';
  const m = Math.round(secs / 60);
  if (m < 60) return m + 'm';
  return Math.floor(m / 60) + 'h' + String(m % 60).padStart(2, '0');
}
function fmtBytes(n) {
  if (n == null) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + ' ' + u[i];
}

// ── state ──────────────────────────────────────────────────────────
const state = {
  channels: [],
  byName: new Map(),
  current: null,   // currently-playing channel name
  hls: null,
};

// ── tabs ───────────────────────────────────────────────────────────
const loaders = {
  live: () => {},          // channels load once at boot
  guide: loadGuide,
  schedules: loadSchedules,
  recordings: loadRecordings,
};

function showView(name) {
  $$('.tab').forEach((t) => t.classList.toggle('active', t.dataset.view === name));
  $$('.view').forEach((v) => v.classList.toggle('hidden', v.id !== 'view-' + name));
  (loaders[name] || (() => {}))();
}

// ── status bar ─────────────────────────────────────────────────────
async function refreshStatus() {
  try {
    const s = await api('/api/status');
    const bits = [el('span', { class: 'chip' }, 'v' + (s.version || '?')),
                  el('span', { class: 'chip' }, 'up ' + (s.uptime || '?'))];
    for (const a of (s.adapters || [])) {
      const busy = a.channel;
      bits.push(el('span', { class: 'chip ' + (busy ? 'busy' : 'idle') },
        'adapter ' + a.adapter + (busy ? `: ${a.channel} ×${a.refs}` : ': idle')));
    }
    const box = $('#status');
    box.replaceChildren(...bits);
  } catch (e) {
    $('#status').textContent = 'offline';
  }
}

// ── channels + live playback ───────────────────────────────────────
async function loadChannels() {
  state.channels = await api('/api/channels') || [];
  state.byName = new Map(state.channels.map((c) => [c.name, c]));
  renderChannelList();
  renderGuideOptions();
}

function renderChannelList() {
  const list = $('#channel-list');
  if (!state.channels.length) {
    list.replaceChildren(el('div', { class: 'empty' }, 'No channels configured.'));
    return;
  }
  list.replaceChildren(...state.channels.map((c) => {
    const sub = c.aliases && c.aliases.length ? c.aliases[0] : ('service ' + c.service_id);
    return el('button', {
      class: 'ch' + (c.name === state.current ? ' active' : ''),
      dataset: { name: c.name },
      onclick: () => play(c.name),
    },
      el('div', { class: 'ch-name' }, c.name),
      el('div', { class: 'ch-sub' }, sub));
  }));
}

function overlay(msg) {
  const o = $('#player-overlay');
  if (msg == null) { o.classList.add('hidden'); o.textContent = ''; }
  else { o.textContent = msg; o.classList.remove('hidden'); }
}

function teardownPlayer() {
  if (state.hls) { state.hls.destroy(); state.hls = null; }
  const v = $('#player');
  v.removeAttribute('src');
  v.load();
}

async function play(name) {
  const prev = state.current;
  if (prev && prev !== name) stopSession(prev); // free the adapter promptly
  teardownPlayer();

  state.current = name;
  renderChannelList();
  $('#stop-btn').classList.remove('hidden');
  overlay('Tuning ' + name + '…');
  updateNowPlaying(name);

  const url = '/api/live/' + encodeURIComponent(name) + '.m3u8';
  const video = $('#player');

  if (window.Hls && window.Hls.isSupported()) {
    const hls = new Hls({
      // First request triggers a tune + ffmpeg spawn; the playlist may
      // 404 for a few seconds. Retry generously before giving up.
      manifestLoadingMaxRetry: 10,
      manifestLoadingRetryDelay: 1000,
      manifestLoadingMaxRetryTimeout: 16000,
      liveSyncDurationCount: 3,
    });
    state.hls = hls;
    hls.on(Hls.Events.FRAG_LOADED, () => overlay(null));
    hls.on(Hls.Events.ERROR, (_, data) => {
      if (!data.fatal) return;
      if (data.type === Hls.ErrorTypes.NETWORK_ERROR) hls.startLoad();
      else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) hls.recoverMediaError();
      else { overlay('Playback error: ' + (data.details || data.type)); teardownPlayer(); }
    });
    hls.loadSource(url);
    hls.attachMedia(video);
    video.play().catch(() => {});
  } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
    // Native HLS (Safari / iOS).
    video.src = url;
    video.addEventListener('playing', () => overlay(null), { once: true });
    video.play().catch(() => {});
  } else {
    overlay('This browser cannot play HLS and hls.js failed to load.');
  }
}

function stopSession(name) {
  fetch('/api/live/' + encodeURIComponent(name) + '/stop', { method: 'POST' }).catch(() => {});
}

function stop() {
  if (state.current) stopSession(state.current);
  teardownPlayer();
  overlay(null);
  $('#stop-btn').classList.add('hidden');
  $('#now-title').textContent = 'Stopped.';
  state.current = null;
  renderChannelList();
}

async function updateNowPlaying(name) {
  const ch = state.byName.get(name);
  const titleBox = $('#now-title');
  if (!ch || !ch.service_id) { titleBox.textContent = name; return; }
  try {
    const ev = await api('/api/now?service=' + ch.service_id);
    if (state.current !== name) return; // switched away while fetching
    if (ev && ev.title) {
      titleBox.replaceChildren(
        el('span', {}, ev.title),
        el('span', { class: 'now-sub' }, `${name} · ${fmtTime(ev.start)}–${fmtTime(new Date(new Date(ev.start).getTime() + ev.duration_s * 1000))}`));
    } else {
      titleBox.textContent = name;
    }
  } catch { titleBox.textContent = name; }
}

// ── Guide (EPG) ────────────────────────────────────────────────────
function renderGuideOptions() {
  const sel = $('#guide-channel');
  const withService = state.channels.filter((c) => c.service_id);
  sel.replaceChildren(...withService.map((c) =>
    el('option', { value: c.name }, c.aliases && c.aliases[0] ? `${c.name} (${c.aliases[0]})` : c.name)));
}

async function loadGuide() {
  const sel = $('#guide-channel');
  const list = $('#guide-list');
  const name = sel.value;
  const ch = state.byName.get(name);
  if (!ch || !ch.service_id) {
    list.replaceChildren(el('div', { class: 'empty' }, 'No channel with EPG data selected.'));
    return;
  }
  list.replaceChildren(el('div', { class: 'empty' }, 'Loading…'));
  const from = new Date(Date.now() - 30 * 60e3).toISOString();
  const to = new Date(Date.now() + 24 * 3600e3).toISOString();
  try {
    const events = await api(`/api/epg?service=${ch.service_id}&from=${from}&to=${to}`) || [];
    if (!events.length) {
      list.replaceChildren(el('div', { class: 'empty' }, 'No EPG events. Has the refresher run yet?'));
      return;
    }
    const now = Date.now();
    list.replaceChildren(...events.map((ev) => {
      const start = new Date(ev.start).getTime();
      const end = start + ev.duration_s * 1000;
      const onNow = start <= now && now < end;
      return el('div', { class: 'epg-item' + (onNow ? ' on-now' : '') },
        el('div', { class: 'epg-time' },
          fmtTime(ev.start),
          onNow ? el('div', {}, el('span', { class: 'badge now' }, 'NOW')) : el('div', {}, fmtDur(ev.duration_s))),
        el('div', {},
          el('div', { class: 'epg-title' }, ev.title || '(untitled)'),
          ev.synopsis ? el('div', { class: 'epg-syn' }, ev.synopsis) : null),
        el('button', { class: 'btn btn-primary', onclick: () => record(ch, ev) }, 'Record'));
    }));
  } catch (e) {
    list.replaceChildren(el('div', { class: 'empty' }, 'Error: ' + e.message));
  }
}

async function record(ch, ev) {
  const start = new Date(ev.start).toISOString();
  const end = new Date(new Date(ev.start).getTime() + ev.duration_s * 1000).toISOString();
  try {
    await api('/api/schedule', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel: ch.name, service_id: ch.service_id, start, end }),
    });
    toast('Scheduled: ' + (ev.title || ch.name), 'ok');
  } catch (e) {
    toast('Schedule failed: ' + e.message, 'err');
  }
}

// ── Schedules ──────────────────────────────────────────────────────
async function loadSchedules() {
  const box = $('#sched-list');
  box.replaceChildren(el('div', { class: 'empty' }, 'Loading…'));
  try {
    const rows = await api('/api/schedule') || [];
    if (!rows.length) { box.replaceChildren(el('div', { class: 'empty' }, 'No schedules.')); return; }
    const table = el('table', {},
      el('thead', {}, el('tr', {},
        ...['Channel', 'Start', 'End', 'State', ''].map((h) => el('th', {}, h)))),
      el('tbody', {}, ...rows.map((s) => el('tr', {},
        el('td', {}, s.channel),
        el('td', {}, fmtDate(s.start)),
        el('td', {}, fmtDate(s.end)),
        el('td', {}, el('span', { class: 'badge ' + s.state }, s.state)),
        el('td', {}, (s.state === 'pending' || s.state === 'running')
          ? el('button', { class: 'btn btn-danger', onclick: () => cancelSchedule(s.id) }, 'Cancel')
          : '')))));
    box.replaceChildren(table);
  } catch (e) {
    box.replaceChildren(el('div', { class: 'empty' }, 'Error: ' + e.message));
  }
}

async function cancelSchedule(id) {
  try {
    await api('/api/schedule/' + id, { method: 'DELETE' });
    toast('Schedule canceled', 'ok');
    loadSchedules();
  } catch (e) {
    toast('Cancel failed: ' + e.message, 'err');
  }
}

// ── Recordings ─────────────────────────────────────────────────────
async function loadRecordings() {
  const box = $('#rec-list');
  box.replaceChildren(el('div', { class: 'empty' }, 'Loading…'));
  try {
    const rows = await api('/api/recordings') || [];
    if (!rows.length) { box.replaceChildren(el('div', { class: 'empty' }, 'No recordings yet.')); return; }
    const table = el('table', {},
      el('thead', {}, el('tr', {},
        ...['Channel', 'Title', 'Start', 'Size', 'State', 'Path'].map((h) => el('th', {}, h)))),
      el('tbody', {}, ...rows.map((r) => el('tr', {},
        el('td', {}, r.channel),
        el('td', {}, r.title || '—'),
        el('td', {}, fmtDate(r.start)),
        el('td', {}, fmtBytes(r.size_bytes)),
        el('td', {}, el('span', { class: 'badge ' + r.state }, r.state),
          r.error ? el('div', { class: 'epg-syn' }, r.error) : null),
        el('td', { class: 'wrap' }, r.path)))));
    box.replaceChildren(table);
  } catch (e) {
    box.replaceChildren(el('div', { class: 'empty' }, 'Error: ' + e.message));
  }
}

// ── boot ───────────────────────────────────────────────────────────
function init() {
  $$('.tab').forEach((t) => t.addEventListener('click', () => showView(t.dataset.view)));
  $('#stop-btn').addEventListener('click', stop);
  $('#guide-channel').addEventListener('change', loadGuide);
  $('#guide-refresh').addEventListener('click', loadGuide);
  $('#sched-refresh').addEventListener('click', loadSchedules);
  $('#rec-refresh').addEventListener('click', loadRecordings);

  showView('live');
  loadChannels().catch((e) => toast('Failed to load channels: ' + e.message, 'err'));
  refreshStatus();
  setInterval(refreshStatus, 10000);
}

document.addEventListener('DOMContentLoaded', init);
