import useSWR, { mutate } from "swr";
import { useEffect, useMemo, useState } from "react";

const BASE = ""; // same-origin

export type Channel = {
  name: string;
  // What to show a person. The daemon picks it (config.Channel.DisplayName)
  // so this UI, the TUI and any agent label a channel the same way.
  display_name?: string;
  aliases?: string[];
  service_id: number;
};

export type EPGEvent = {
  service_id: number;
  event_id: number;
  title: string;
  synopsis?: string;
  start: string; // RFC3339
  duration_s: number;
  ingested_at: string;
};

export type Schedule = {
  id: number;
  channel: string;
  service_id: number;
  start: string;
  end: string;
  lead: string;
  trail: string;
  state: string;
};

// Mirrors store.Recording's MarshalJSON. `end` and `size_bytes` are only
// set once the row is finalized, so both are null while it is recording.
export type Recording = {
  id: number;
  schedule_id: number | null;
  channel: string;
  title?: string;
  start: string;
  end: string | null;
  path: string;
  size_bytes: number | null;
  state: string;
  error?: string;
  // How far the post-pass has got: the .mp4 a browser can open and the
  // subtitle sidecars beside it. Absent while the recording is still
  // running — nothing can be derived from a file still being written.
  // 'skipped' is what the migration wrote over the recordings that
  // predate the feature, so it means "never asked for", not "failed".
  post_state?: "pending" | "running" | "done" | "failed" | "skipped";
  post_error?: string;
};

// A recording is playable in a browser only once the post-pass has made
// the .mp4 — the .ts is MPEG-2 video with ARIB-flavoured audio and no
// browser will touch it.
export function isPlayable(r: Recording) {
  return r.state === "done" && r.post_state === "done";
}

export type AdapterStatus = {
  adapter: number;
  // The delivery systems this frontend can tune ("ISDBT", "ISDBS").
  // Reported because "no adapter supports this delivery system" is
  // otherwise a claim with nothing on screen to check it against.
  systems?: string[];
  channel?: string;
  refs: number;
  // The claim's priority — "record" | "live" | "background". Absent when
  // the adapter is idle.
  prio?: string;
  // Held without a fanout: an EPG pass has the adapter but no channel to
  // report. Without this the UI reads a reserved adapter as idle.
  reserved?: boolean;
};

// One address the daemon answers on, labelled by reach: local | lan |
// tailscale | public. Only the daemon can enumerate these — a browser
// would be listing the viewer's own interfaces — so they arrive on
// /api/status rather than being derived here.
export type Address = {
  kind: string;
  host: string;
  base: string; // "http://192.168.1.42:8010"
  iface?: string;
};

export type StatusResp = {
  // The live quality tiers this daemon offers, first one the default.
  // Absent on a daemon that predates them, which the player reads as
  // "one tier, do not offer a choice".
  live_qualities?: LiveQuality[];
  version: string;
  started: string;
  uptime: string;
  // The single live playlist path, whatever is tuned ("/stream.m3u8").
  stream?: string;
  addresses?: Address[];
  adapters?: AdapterStatus[];
  // Row ids of recordings in progress.
  recording?: number[];
};

// ── hooks ────────────────────────────────────────────────────────

function fetcher<T>(url: string): Promise<T> {
  return fetch(BASE + url).then((r) => {
    if (!r.ok) throw new Error(r.statusText);
    return r.json();
  });
}

export function useChannels() {
  // Channels don't change at runtime — fetch once.
  return useSWR<Channel[]>("/api/channels", fetcher, { revalidateOnFocus: false, revalidateOnReconnect: false });
}

export function useEPG(serviceId?: number) {
  // Round the window to a 10-minute bucket. Date.now() changes every
  // render, and an ms-precision ISO string in the URL would make the
  // SWR cache key unstable — every re-render would mint a brand-new key
  // and refire /api/epg (the "frantic EPG refresh" bug). Flooring to a
  // coarse bucket keeps the key — and the cache entry — stable.
  const bucketMs = 10 * 60_000;
  const bucket = Math.floor(Date.now() / bucketMs) * bucketMs;
  const from = new Date(bucket - 3600_000).toISOString();
  const to = new Date(bucket + 12 * 3600_000).toISOString();
  const qs = serviceId
    ? `?service=${serviceId}&from=${from}&to=${to}`
    : `?from=${from}&to=${to}`;
  // EPG is DB-backed and changes only when the daemon's background
  // refresher ingests a new batch, so fetch once and refresh manually.
  return useSWR<EPGEvent[]>(`/api/epg${qs}`, fetcher, { revalidateOnFocus: false, revalidateOnReconnect: false });
}

export function useNow(serviceId?: number) {
  return useSWR<EPGEvent | null>(serviceId ? `/api/now?service=${serviceId}` : null, fetcher, {
    refreshInterval: 60_000,
    revalidateOnFocus: false,
    revalidateOnReconnect: false,
  });
}

// Next event after the currently-airing one for a service. Uses the
// same EPG window as useEPG but filters to events starting after now.
export function useNextEvent(serviceId?: number) {
  const bucketMs = 10 * 60_000;
  const bucket = Math.floor(Date.now() / bucketMs) * bucketMs;
  const from = new Date(bucket).toISOString();
  const to = new Date(bucket + 6 * 3600_000).toISOString();
  const qs = serviceId ? `?service=${serviceId}&from=${from}&to=${to}` : null;
  const { data, ...rest } = useSWR<EPGEvent[]>(qs, fetcher, {
    refreshInterval: 60_000,
    revalidateOnFocus: false,
    revalidateOnReconnect: false,
  });
  const now = Date.now();
  const next = data
    ?.filter((e) => new Date(e.start).getTime() > now)
    .sort((a, b) => new Date(a.start).getTime() - new Date(b.start).getTime())[0];
  return { data: next, ...rest };
}

// Re-render on the minute, returning the current minute as epoch ms.
//
// Both the guide's "on now" highlight and the sidebar's now-playing line
// are derived from Date.now() during render, which makes them only as
// fresh as the last render React happened to do for some other reason.
// This is the clock they need. Ticking faster than a minute and letting
// the state bail out on an unchanged value keeps it near the boundary
// without a render a second.
export function useMinuteTick(): number {
  const [minute, setMinute] = useState(() => Math.floor(Date.now() / 60_000));
  useEffect(() => {
    const id = setInterval(() => setMinute(Math.floor(Date.now() / 60_000)), 15_000);
    return () => clearInterval(id);
  }, []);
  return minute * 60_000;
}

// What is airing right now on every service, keyed by service_id.
//
// The channel sidebar wants a now-playing line per row, which it used to
// get by mounting one useNow() per channel — 39 requests to /api/now on
// every page load. This derives all of them from the one all-services
// window the guide already fetches (same SWR key, so visiting either page
// warms the other): 312 events, ~180 KB, once.
export function useNowByService() {
  const { data } = useEPG();
  const at = useMinuteTick();
  return useMemo(() => {
    const m = new Map<number, EPGEvent>();
    for (const e of data ?? []) {
      if (isAiring(e, at)) m.set(e.service_id, e);
    }
    return m;
  }, [data, at]);
}

export function useStatus() {
  return useSWR<StatusResp>("/api/status", fetcher, {
    refreshInterval: 15_000,
    revalidateOnFocus: false,
  });
}

export function useSchedules() {
  return useSWR<Schedule[]>("/api/schedule", fetcher, { revalidateOnFocus: false });
}

export function useRecordings() {
  // A running recording's size only lands in the row when it finalizes,
  // but its state does change under us — poll while the page is open.
  //
  // Faster while anything is in flight: a recording that stops and a
  // transcode that finishes both turn into a button on this page (Convert,
  // then Watch), and half a minute of staring at "converting…" after it is
  // already done reads as a stuck queue.
  return useSWR<Recording[]>("/api/recordings", fetcher, {
    refreshInterval: (rows) =>
      rows?.some(
        (r) =>
          r.state === "recording" ||
          r.post_state === "pending" ||
          r.post_state === "running",
      )
        ? 5_000
        : 30_000,
    revalidateOnFocus: false,
  });
}

// ── channel labelling ────────────────────────────────────────────

// Lookup by the two keys the rest of the API hands out: a channel `name`
// (schedules, recordings, adapter status) and a `service_id` (EPG rows).
//
// Every page needs this. The guide, the schedule list and the recordings
// list each used to print the raw key, which for a record migrated from
// the legacy dvbv5 conf is mojibake — "NHKEFl1El5~" where the label is
// "NHKEテレ1東京". Only the Live sidebar had been fixed. One index, so a
// new page cannot regress it again.
export type ChannelIndex = {
  channels: Channel[];
  byName: Map<string, Channel>;
  byServiceId: Map<number, Channel>;
  // Label for a channel name; the name itself if it is not in the list
  // (a recording of a channel since removed from channels.json).
  label: (name: string) => string;
  // Label for a service id; "SID 1024" if unknown, which is what an EPG
  // row for a service not in channels.json deserves.
  labelForServiceId: (sid: number) => string;
};

export function useChannelIndex(): ChannelIndex {
  const { data } = useChannels();
  return useMemo(() => {
    const channels = data ?? [];
    const byName = new Map(channels.map((c) => [c.name, c]));
    // First wins, mirroring config.Channels.Find's file order: several
    // services on a mux share a name, and the first is the one a bare
    // request resolves to.
    const byServiceId = new Map<number, Channel>();
    for (const c of channels) {
      if (!byServiceId.has(c.service_id)) byServiceId.set(c.service_id, c);
    }
    return {
      channels,
      byName,
      byServiceId,
      label: (name) => {
        const c = byName.get(name);
        return c ? displayName(c) : name;
      },
      labelForServiceId: (sid) => {
        const c = byServiceId.get(sid);
        return c ? displayName(c) : `SID ${sid}`;
      },
    };
  }, [data]);
}

// Display name: whatever the daemon chose, falling back to the canonical
// key. Requests still carry c.name.
//
// This used to take aliases[0], which is wrong as often as it is right —
// for records migrated from the legacy dvbv5 conf the first alias is the
// mojibake ("J!'COM|ÆìÓ"), and the readable name is either later in the
// list or is c.name itself. The choice now lives in one place, in Go.
export function displayName(c: Channel): string {
  return c.display_name || c.name;
}

// ── actions ──────────────────────────────────────────────────────

async function post<T>(path: string, body?: unknown): Promise<T | null> {
  const r = await fetch(BASE + path, {
    method: "POST",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error ?? r.statusText);
  }
  // 204 on the stop endpoints.
  if (r.status === 204) return null;
  return r.json() as Promise<T>;
}

// ── channel scan ─────────────────────────────────────────────────

export type ScanProgress = {
  physical: number;
  frequency_hz: number;
  done: number;
  total: number;
  locked: boolean;
  services: number;
  error?: string;
  finished?: boolean;
};

export type ScanStatus = { available: boolean; running: boolean };

export function useScanStatus() {
  return useSWR<ScanStatus>("/api/scan", fetcher, { revalidateOnFocus: false });
}

// Sweep the band, calling onProgress for each transport.
//
// POST + a streamed body rather than EventSource, which can only GET —
// and this is not a GET: it drives the tuner for ten minutes. The events
// are ordinary SSE frames read off the response, which is all EventSource
// would have given us anyway.
//
// Closing the connection does not stop the scan. The daemon detaches it
// from the request on purpose: a sweep abandoned half-way leaves
// channels.json describing part of the band with nothing to say so.
export async function scanChannels(
  onProgress: (p: ScanProgress) => void,
  signal?: AbortSignal,
): Promise<void> {
  const r = await fetch(BASE + "/api/scan", { method: "POST", signal });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    throw new Error(e.error ?? r.statusText);
  }
  if (!r.body) throw new Error("no response body");

  const reader = r.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    // Frames are separated by a blank line; a partial one stays in the
    // buffer until the rest of it arrives.
    let cut: number;
    while ((cut = buf.indexOf("\n\n")) !== -1) {
      const frame = buf.slice(0, cut);
      buf = buf.slice(cut + 2);
      for (const line of frame.split("\n")) {
        // ": keepalive" comments keep proxies from dropping an idle
        // connection between transports; they carry nothing.
        if (!line.startsWith("data:")) continue;
        try {
          onProgress(JSON.parse(line.slice(5).trim()) as ScanProgress);
        } catch {
          /* a truncated frame at shutdown; nothing to do */
        }
      }
    }
  }
  // The daemon has re-read channels.json by now, so the list this UI
  // shows is a fetch away rather than a restart away.
  await Promise.all([mutate("/api/channels"), mutate("/api/scan"), mutate("/api/status")]);
}

export async function createSchedule(body: {
  channel: string;
  service_id: number;
  start: string;
  end: string;
  lead_s?: number;
  trail_s?: number;
}) {
  const out = await post<{ id: number }>("/api/schedule", body);
  await mutate("/api/schedule");
  return out!;
}

export async function cancelSchedule(id: number) {
  const r = await fetch(BASE + "/api/schedule/" + id, { method: "DELETE" });
  if (!r.ok) throw new Error(r.statusText);
  await mutate("/api/schedule");
}

// Change the live channel. The daemon closes any other session, tunes
// this one, and does not answer until the playlist is on disk.
//
// This must not be done by hand as stop-then-open: two live sessions
// have equal priority and will not evict each other, so with a single
// adapter the wrong order deadlocks on ErrNoAdapter. The endpoint exists
// to own that order — see api.handleLiveSwitch.
export async function switchLive(channel: string, quality?: string) {
  const out = await post<{ channel: string; quality: string; playlist: string; closed: string[] }>(
    "/api/live/" + encodeURIComponent(channel) + "/switch" + qualityQuery(quality),
  );
  await mutate("/api/status");
  return out!;
}

// One live quality tier, as /api/status reports it. The first is the
// default — the one a bookmark or VLC gets without asking.
export type LiveQuality = { name: string; label: string; bandwidth: number };

// ?q= only on the URLs a person or a bookmark types. Everything below the
// master playlist carries the tier as a path segment, because a relative
// segment URI has to resolve inside the tier's own directory.
export function qualityQuery(quality?: string | null) {
  return quality ? "?q=" + encodeURIComponent(quality) : "";
}

export async function stopLive(channel: string) {
  await post("/api/live/" + encodeURIComponent(channel) + "/stop");
  await mutate("/api/status");
}

// Record now, open-ended (the daemon caps it at MaxAdhocDuration). 201
// does not mean bytes are on disk — the tuner is acquired
// asynchronously and a failure lands in the row's state.
export async function recordNow(channel: string) {
  const out = await post<{ id: number; channel: string; title: string }>("/api/record", { channel });
  await Promise.all([mutate("/api/recordings"), mutate("/api/status")]);
  return out!;
}

// The graceful early finish: the row goes to 'done' with the bytes
// written. Canceling instead would mark it 'failed'.
export async function stopRecording(id: number) {
  await post("/api/record/" + id + "/stop");
  await Promise.all([mutate("/api/recordings"), mutate("/api/status")]);
}

// The recorded TS itself. The endpoint honours Range, so this works as a
// download link and as a URL to hand to mpv/VLC.
export function recordingFileUrl(id: number) {
  return BASE + "/api/recordings/" + id + "/file";
}

// What the post-pass made beside the recording: the MP4 a browser can play
// and the two subtitle forms. All three are named after the .ts rather than
// stored, so they exist only once post_state is 'done' — and a recording
// that carried no captions has the MP4 and neither sidecar.
export function recordingMp4Url(id: number) {
  return BASE + "/api/recordings/" + id + "/mp4";
}

// "ass" keeps ARIB's own placement (the caption where the broadcast put it,
// over the shot it belongs to); "vtt" is the same words as plain lines.
export function recordingSubsUrl(id: number, kind: "ass" | "vtt") {
  return BASE + "/api/recordings/" + id + "/subs." + kind;
}

// Ask for a recording to be (re)processed. The queue is a column in the
// row, so this returns as soon as it is written — post_state is what says
// whether it ran. This is the only way to get an MP4 for a recording made
// before the post-pass existed (the migration marked those 'skipped') and
// the way to retry a failed one.
export async function postprocessRecording(id: number) {
  const out = await post<{ id: number; post_state: string; queued: boolean }>(
    "/api/recordings/" + id + "/postprocess",
  );
  await mutate("/api/recordings");
  return out!;
}

export async function deleteRecording(id: number) {
  const r = await fetch(BASE + "/api/recordings/" + id, { method: "DELETE" });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error ?? r.statusText);
  }
  await mutate("/api/recordings");
  return r.json() as Promise<{ id: number; file_deleted: boolean }>;
}

// ── helpers ──────────────────────────────────────────────────────

// A recording in progress has no end yet, so null is an ordinary value here.
export function fmtTime(iso: string | null | undefined) {
  if (!iso) return "…";
  return new Date(iso).toLocaleTimeString("ja-JP", { hour: "2-digit", minute: "2-digit" });
}

export function fmtDate(iso: string) {
  return new Date(iso).toLocaleDateString("ja-JP", { month: "short", day: "numeric", weekday: "short" });
}

// null while a recording is still running — the size is only written when
// the row is finalized.
export function fmtBytes(n: number | null | undefined) {
  if (n == null) return "—";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
  return (n / (1024 * 1024 * 1024)).toFixed(2) + " GB";
}

export function epgEnd(e: EPGEvent) {
  return new Date(new Date(e.start).getTime() + e.duration_s * 1000).toISOString();
}

export function isAiring(e: EPGEvent, at = Date.now()) {
  const start = new Date(e.start).getTime();
  return start <= at && new Date(epgEnd(e)).getTime() > at;
}

// "1h30m" — a duration in the same shape the daemon's uptime uses.
export function fmtDuration(seconds: number) {
  const m = Math.round(seconds / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 === 0 ? `${h}h` : `${h}h${m % 60}m`;
}

export type ChannelGroup = "GR" | "BS" | "CS" | "SKY" | "Other";

// ISDB service-id convention for grouping (mirrors Mirakurun/arib):
//   GR  < 10000
//   BS  10000..19999
//   CS  20000..29999
//   SKY >= 30000
export function channelGroup(c: Channel): ChannelGroup {
  const sid = c.service_id;
  if (sid < 10000) return "GR";
  if (sid < 20000) return "BS";
  if (sid < 30000) return "CS";
  if (sid >= 30000) return "SKY";
  return "Other";
}

export const CHANNEL_GROUP_ORDER: ChannelGroup[] = ["GR", "BS", "CS", "SKY", "Other"];
