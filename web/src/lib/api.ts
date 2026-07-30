import useSWR, { mutate } from "swr";

const BASE = ""; // same-origin

export type Channel = {
  name: string;
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
};

export type AdapterStatus = {
  adapter: number;
  channel?: string;
  refs: number;
};

export type StatusResp = {
  version: string;
  started: string;
  uptime: string;
  adapters?: AdapterStatus[];
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
  return useSWR<Recording[]>("/api/recordings", fetcher, { revalidateOnFocus: false });
}

// ── actions ──────────────────────────────────────────────────────

export async function createSchedule(body: {
  channel: string;
  service_id: number;
  start: string;
  end: string;
  lead_s?: number;
  trail_s?: number;
}) {
  const r = await fetch(BASE + "/api/schedule", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error ?? r.statusText);
  }
  await mutate("/api/schedule");
  return r.json() as Promise<{ id: number }>;
}

export async function cancelSchedule(id: number) {
  const r = await fetch(BASE + "/api/schedule/" + id, { method: "DELETE" });
  if (!r.ok) throw new Error(r.statusText);
  await mutate("/api/schedule");
}

export async function stopLive(channel: string) {
  await fetch(BASE + "/api/live/" + encodeURIComponent(channel) + "/stop", { method: "POST" });
}

// The recorded TS itself. The endpoint honours Range, so this works as a
// download link and as a URL to hand to mpv/VLC.
export function recordingFileUrl(id: number) {
  return BASE + "/api/recordings/" + id + "/file";
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

// Display name: prefer the first alias (human-readable Japanese service
// name), fall back to the canonical machine key. The /api/live/{name}.m3u8
// endpoint still receives c.name because the backend matches name+aliases.
export function displayName(c: Channel): string {
  return c.aliases?.[0] || c.name;
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
