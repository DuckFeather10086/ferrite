// A stand-in for the ferrite daemon, so tests exercise the real HTTP paths
// and JSON shapes without a tuner.

export interface FakeState {
  channels: Array<{ name: string; aliases?: string[]; service_id: number }>;
  events: Record<number, Array<Record<string, unknown>>>;
  recording: number[];
  recordings: Array<Record<string, unknown>>;
  schedules: Array<Record<string, unknown>>;
  tuned: string | null;
  /** Per-path override: return a [status, body] to force an error. */
  fail: Record<string, [number, unknown]>;
  /** Every request the client made, for assertions. */
  calls: Array<{ method: string; path: string; body?: unknown }>;
  nextId: number;
}

export interface FakeDaemon {
  url: string;
  state: FakeState;
  stop(): void;
}

export function startFakeDaemon(overrides: Partial<FakeState> = {}): FakeDaemon {
  const state: FakeState = {
    channels: [
      { name: "asahi", aliases: ["テレビ朝日"], service_id: 1064 },
      { name: "NHK_G", aliases: ["NHK総合1東京"], service_id: 1024 },
      { name: "TOKYO MX1", service_id: 23608 },
    ],
    events: {},
    recording: [],
    recordings: [],
    schedules: [],
    tuned: null,
    fail: {},
    calls: [],
    nextId: 1,
    ...overrides,
  };

  const server = Bun.serve({
    port: 0,
    async fetch(req) {
      const url = new URL(req.url);
      const path = url.pathname;
      let body: unknown;
      if (req.method === "POST" && req.headers.get("content-type")?.includes("json")) {
        body = await req.json().catch(() => undefined);
      }
      state.calls.push({ method: req.method, path: decodeURIComponent(path), body });

      const forced = state.fail[path] ?? state.fail[decodeURIComponent(path)];
      if (forced) {
        return json(forced[1], forced[0]);
      }

      if (path === "/api/channels") return json(state.channels);

      if (path === "/api/status") {
        return json({
          version: "test",
          uptime: "5m",
          adapters: [
            state.tuned
              ? { adapter: 0, channel: state.tuned, refs: 1, prio: "live" }
              : { adapter: 0, refs: 0 },
          ],
          recording: state.recording,
        });
      }

      if (path === "/api/now") {
        const sid = Number(url.searchParams.get("service"));
        const now = Date.now();
        const airing = (state.events[sid] ?? []).find((e) => {
          const start = new Date(String(e.start)).getTime();
          return now >= start && now < start + Number(e.duration_s) * 1000;
        });
        return json(airing ?? null);
      }

      if (path === "/api/epg") {
        const sid = Number(url.searchParams.get("service"));
        return json(state.events[sid] ?? []);
      }

      if (path === "/api/recordings") return json(state.recordings);

      const recFile = path.match(/^\/api\/recordings\/(\d+)\/file$/);
      if (recFile && req.method === "GET") {
        const row = state.recordings.find((r) => r.id === Number(recFile[1]));
        if (!row) return json({ error: "no such recording" }, 404);
        return new Response("TS", { headers: { "content-type": "video/mp2t" } });
      }

      const delRec = path.match(/^\/api\/recordings\/(\d+)$/);
      if (delRec && req.method === "DELETE") {
        const id = Number(delRec[1]);
        const row = state.recordings.find((r) => r.id === id);
        if (!row) return json({ error: "no such recording" }, 404);
        if (row.state === "recording") {
          return json({ error: `recording ${id} is still running — stop it first` }, 409);
        }
        state.recordings = state.recordings.filter((r) => r.id !== id);
        return json({ id, file_deleted: true });
      }
      if (path === "/api/schedule" && req.method === "GET") return json(state.schedules);

      if (path === "/api/schedule" && req.method === "POST") {
        const id = state.nextId++;
        state.schedules.push({ id, state: "pending", ...(body as object) });
        return json({ id }, 201);
      }

      const cancel = path.match(/^\/api\/schedule\/(\d+)$/);
      if (cancel && req.method === "DELETE") {
        state.schedules = state.schedules.filter((s) => s.id !== Number(cancel[1]));
        return new Response(null, { status: 204 });
      }

      if (path === "/api/record" && req.method === "POST") {
        const req_ = (body ?? {}) as { channel?: string; title?: string };
        const id = state.nextId++;
        state.recording.push(id);
        state.recordings.unshift({
          id,
          channel: req_.channel,
          title: req_.title ?? "",
          start: new Date().toISOString(),
          end: null,
          path: `/var/rec/${id}.ts`,
          size_bytes: null,
          state: "recording",
        });
        return json({ id, channel: req_.channel, title: req_.title ?? "" }, 201);
      }

      const stopRec = path.match(/^\/api\/record\/(\d+)\/stop$/);
      if (stopRec && req.method === "POST") {
        const id = Number(stopRec[1]);
        if (!state.recording.includes(id)) return json({ error: "recording not in progress" }, 404);
        state.recording = state.recording.filter((r) => r !== id);
        const row = state.recordings.find((r) => r.id === id);
        if (row) {
          row.state = "done";
          row.size_bytes = 12_345_678;
        }
        return new Response(null, { status: 204 });
      }

      const sw = path.match(/^\/api\/live\/(.+)\/switch$/);
      if (sw && req.method === "POST") {
        const channel = decodeURIComponent(sw[1]!);
        const resolved =
          state.channels.find((c) => c.name === channel || (c.aliases ?? []).includes(channel))
            ?.name ?? channel;
        const closed = state.tuned && state.tuned !== resolved ? [state.tuned] : [];
        state.tuned = resolved;
        return json({
          channel: resolved,
          playlist: `/api/live/${encodeURIComponent(resolved)}.m3u8`,
          closed,
        });
      }

      const stopLive = path.match(/^\/api\/live\/(.+)\/stop$/);
      if (stopLive && req.method === "POST") {
        state.tuned = null;
        return new Response(null, { status: 204 });
      }

      return json({ error: "not found" }, 404);
    },
  });

  return {
    url: `http://localhost:${server.port}`,
    state,
    stop: () => server.stop(true),
  };
}

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** An event starting `offsetMin` from now, for guide fixtures. */
export function event(
  serviceId: number,
  eventId: number,
  title: string,
  offsetMin: number,
  minutes = 30,
): Record<string, unknown> {
  return {
    service_id: serviceId,
    event_id: eventId,
    start: new Date(Date.now() + offsetMin * 60_000).toISOString(),
    duration_s: minutes * 60,
    title,
  };
}
