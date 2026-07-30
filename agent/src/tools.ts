// The tool list: everything an agent needs to operate the TV.
//
// One definition serves two consumers — the MCP server (mcp.ts) and the
// DeepSeek tool-calling loop (agent.ts) — so the schemas are plain JSON
// Schema rather than a validator library's types.
//
// The descriptions carry the operating rules, not just the parameters. An
// agent that doesn't know there is one tuner, that a recording evicts live
// playback, and that changing channel is a single call will make bad
// decisions with a technically correct API.

import { ApiError, type FerriteClient } from "./client.ts";

export interface Tool {
  name: string;
  description: string;
  inputSchema: {
    type: "object";
    properties: Record<string, unknown>;
    required?: string[];
    additionalProperties?: boolean;
  };
  /** Returns text for the agent. Throw to signal a tool error. */
  run(client: FerriteClient, args: Record<string, any>): Promise<string>;
}

const TZ = process.env.TZ || "Asia/Tokyo";

function local(iso: string): string {
  return new Date(iso).toLocaleString("sv-SE", { timeZone: TZ }).replace("T", " ");
}

function ok(data: unknown): string {
  return JSON.stringify(data, null, 1);
}

/** Guide window used when resolving an event_id for a timer. */
const EVENT_LOOKUP_DAYS = 8;

export const tools: Tool[] = [
  {
    name: "tv_status",
    description:
      "What the TV is doing right now: which channel is tuned, how the tuner " +
      "is occupied and at what priority, and which recordings are running. " +
      "Call this first when the user asks what's on or whether anything is " +
      "recording. There is normally ONE tuner, so at most one channel can be " +
      "tuned at a time.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    async run(client) {
      const s = await client.status();
      return ok({
        tuned_channel: s.adapters.find((a) => a.channel)?.channel ?? null,
        adapters: s.adapters.map((a) => ({
          adapter: a.adapter,
          channel: a.channel ?? null,
          holders: a.refs,
          priority: a.prio ?? "idle",
          held_by_epg_scan: a.reserved ?? false,
        })),
        active_recording_ids: s.recording ?? [],
        daemon_uptime: s.uptime,
      });
    },
  },

  {
    name: "tv_channels",
    description:
      "List every channel the TV can tune, with its aliases and service id. " +
      "Any of the names or aliases may be used wherever a tool takes a " +
      "channel. Call this when the user names a station you have not seen " +
      "yet, rather than guessing a name.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    async run(client) {
      const channels = await client.channels();
      return ok(
        channels.map((c) => ({
          channel: c.name,
          aliases: c.aliases ?? [],
          service_id: c.service_id,
        })),
      );
    },
  },

  {
    name: "tv_guide",
    description:
      "Programme guide for one channel: what is airing now and what follows. " +
      "Times are given both as UTC (`start`) and in the TV's local timezone " +
      "(`start_local`) — reason in local time, but pass `event_id` to " +
      "tv_schedule_add rather than retyping timestamps. Guide data comes from " +
      "periodic EPG scans, so a channel that has not been scanned yet returns " +
      "an empty list; that is not an error.",
    inputSchema: {
      type: "object",
      properties: {
        channel: { type: "string", description: "Channel name or alias." },
        hours: {
          type: "number",
          description: "How far ahead to look. Default 12, max 192 (8 days).",
        },
      },
      required: ["channel"],
      additionalProperties: false,
    },
    async run(client, args) {
      const serviceId = await resolveServiceId(client, args.channel);
      const hours = Math.min(Math.max(Number(args.hours) || 12, 1), 24 * EVENT_LOOKUP_DAYS);
      const from = new Date(Date.now() - 60 * 60 * 1000);
      const to = new Date(Date.now() + hours * 60 * 60 * 1000);
      const events = await client.schedule(serviceId, from, to);
      const now = Date.now();
      return ok(
        events.map((e) => {
          const start = new Date(e.start).getTime();
          const end = start + e.duration_s * 1000;
          return {
            event_id: e.event_id,
            airing_now: now >= start && now < end,
            start,
            start_local: local(e.start),
            end_local: local(new Date(end).toISOString()),
            minutes: Math.round(e.duration_s / 60),
            title: e.title,
            synopsis: e.synopsis ?? undefined,
          };
        }),
      );
    },
  },

  {
    name: "tv_watch",
    description:
      "Turn the TV on, or change channel — it is the same single operation. " +
      "The daemon drops whatever was playing, tunes this channel, and answers " +
      "once the stream is ready, which takes roughly 7 seconds; do not treat " +
      "that wait as a hang. Returns a playlist URL any HLS player (mpv, VLC, " +
      "a browser) can open. Refuses with a 'tuner busy' message if a " +
      "recording holds the tuner — recordings outrank live viewing.",
    inputSchema: {
      type: "object",
      properties: { channel: { type: "string", description: "Channel name or alias." } },
      required: ["channel"],
      additionalProperties: false,
    },
    async run(client, args) {
      const res = await client.switchTo(String(args.channel));
      return ok({
        watching: res.channel,
        playlist_url: client.playlistUrl(res.channel),
        closed_previous: res.closed ?? [],
      });
    },
  },

  {
    name: "tv_off",
    description:
      "Stop live playback and release the tuner. Recordings are unaffected. " +
      "Safe to call when nothing is playing.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    async run(client) {
      const s = await client.status();
      const tuned = s.adapters.find((a) => a.channel)?.channel;
      if (!tuned) return ok({ stopped: null, note: "nothing was playing" });
      await client.stopLive(tuned);
      return ok({ stopped: tuned });
    },
  },

  {
    name: "tv_record_start",
    description:
      "Start recording a channel immediately. Open-ended unless `minutes` is " +
      "given (a forgotten recording is capped at 12 hours). Returns a " +
      "recording id — the handle for tv_record_stop. With one tuner this " +
      "EVICTS live playback if it is on another channel, so say so when " +
      "reporting back. For something that airs later, use tv_schedule_add " +
      "instead of waiting.",
    inputSchema: {
      type: "object",
      properties: {
        channel: { type: "string", description: "Channel name or alias." },
        minutes: { type: "number", description: "Stop automatically after this long." },
        title: {
          type: "string",
          description:
            "Names the file. Omit to let the daemon use whatever the guide " +
            "says is airing.",
        },
      },
      required: ["channel"],
      additionalProperties: false,
    },
    async run(client, args) {
      const minutes = Number(args.minutes) || 0;
      const res = await client.record(
        String(args.channel),
        args.title ? String(args.title) : undefined,
        minutes > 0 ? minutes * 60 : undefined,
      );
      return ok({
        recording_id: res.id,
        channel: res.channel,
        title: res.title || "(from guide)",
        stops: minutes > 0 ? `after ${minutes} min` : "when stopped (12h cap)",
      });
    },
  },

  {
    name: "tv_record_stop",
    description:
      "Stop a recording in progress. Omit `id` to stop the most recently " +
      "started one. What is already on disk is kept and the recording counts " +
      "as finished, not failed.",
    inputSchema: {
      type: "object",
      properties: { id: { type: "number", description: "Recording id from tv_record_start." } },
      additionalProperties: false,
    },
    async run(client, args) {
      let id = Number(args.id) || 0;
      if (!id) {
        const s = await client.status();
        const active = (s.recording ?? []).slice().sort((a, b) => b - a);
        if (active.length === 0) throw new Error("nothing is recording");
        id = active[0]!;
      }
      await client.stopRecording(id);
      return ok({ stopped_recording_id: id });
    },
  },

  {
    name: "tv_recordings",
    description:
      "List recordings, newest first: state (recording/done/failed), channel, " +
      "size and file path. A failed row carries the reason.",
    inputSchema: {
      type: "object",
      properties: {
        limit: { type: "number", description: "How many to return. Default 20." },
      },
      additionalProperties: false,
    },
    async run(client, args) {
      const limit = Math.min(Math.max(Number(args.limit) || 20, 1), 200);
      const recs = await client.recordings();
      return ok(
        recs.slice(0, limit).map((r) => ({
          id: r.id,
          state: r.state,
          channel: r.channel,
          title: r.title || undefined,
          started_local: local(r.start),
          megabytes: r.size_bytes == null ? null : Math.round(r.size_bytes / 1e6),
          path: r.path,
          error: r.error || undefined,
        })),
      );
    },
  },

  {
    name: "tv_schedule_list",
    description: "List timer recordings that have not run yet, and their state.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    async run(client) {
      const list = await client.schedules();
      return ok(
        list.map((s) => ({
          id: s.id,
          channel: s.channel,
          starts_local: local(s.start),
          ends_local: local(s.end),
          state: s.state,
        })),
      );
    },
  },

  {
    name: "tv_schedule_add",
    description:
      "Set a timer recording for a programme that airs later. Prefer passing " +
      "the `event_id` from tv_guide — the exact times are then taken from the " +
      "guide, which is safer than retyping them. Otherwise give `start` and " +
      "`end` as ISO 8601. A little padding is added either side by default " +
      "because broadcasts drift.",
    inputSchema: {
      type: "object",
      properties: {
        channel: { type: "string", description: "Channel name or alias." },
        event_id: { type: "number", description: "Programme id from tv_guide." },
        start: { type: "string", description: "ISO 8601 start, if no event_id." },
        end: { type: "string", description: "ISO 8601 end, if no event_id." },
        pad_before_min: { type: "number", description: "Lead-in minutes. Default 0.5." },
        pad_after_min: { type: "number", description: "Trail minutes. Default 1." },
      },
      required: ["channel"],
      additionalProperties: false,
    },
    async run(client, args) {
      const channel = String(args.channel);
      const serviceId = await resolveServiceId(client, channel);

      let start: string;
      let end: string;
      let title: string | undefined;

      if (args.event_id != null) {
        const wanted = Number(args.event_id);
        const from = new Date(Date.now() - 60 * 60 * 1000);
        const to = new Date(Date.now() + EVENT_LOOKUP_DAYS * 24 * 60 * 60 * 1000);
        const events = await client.schedule(serviceId, from, to);
        const match = events.find((e) => e.event_id === wanted);
        if (!match) {
          throw new Error(
            `event_id ${wanted} is not in ${channel}'s guide for the next ` +
              `${EVENT_LOOKUP_DAYS} days — call tv_guide again, ids change ` +
              `as the guide is refreshed`,
          );
        }
        start = match.start;
        end = new Date(new Date(match.start).getTime() + match.duration_s * 1000).toISOString();
        title = match.title;
      } else {
        if (!args.start || !args.end) {
          throw new Error("give either event_id, or both start and end");
        }
        start = new Date(String(args.start)).toISOString();
        end = new Date(String(args.end)).toISOString();
        if (!(new Date(end) > new Date(start))) throw new Error("end must be after start");
      }

      const leadMin = args.pad_before_min == null ? 0.5 : Number(args.pad_before_min);
      const trailMin = args.pad_after_min == null ? 1 : Number(args.pad_after_min);
      const res = await client.createSchedule({
        channel,
        service_id: serviceId,
        start,
        end,
        lead_s: Math.round(leadMin * 60),
        trail_s: Math.round(trailMin * 60),
      });
      return ok({
        schedule_id: res.id,
        channel,
        title,
        starts_local: local(start),
        ends_local: local(end),
      });
    },
  },

  {
    name: "tv_schedule_cancel",
    description: "Cancel a timer recording by its schedule id.",
    inputSchema: {
      type: "object",
      properties: { id: { type: "number", description: "Schedule id." } },
      required: ["id"],
      additionalProperties: false,
    },
    async run(client, args) {
      const id = Number(args.id);
      if (!id) throw new Error("id is required");
      await client.cancelSchedule(id);
      return ok({ cancelled_schedule_id: id });
    },
  },
];

export const toolsByName = new Map(tools.map((t) => [t.name, t]));

/**
 * Runs a tool and renders the outcome as text for the agent.
 *
 * Failures come back as readable text rather than exceptions: an agent that
 * is told "tuner busy: a recording holds it" can decide what to do, whereas
 * a thrown error usually just ends the turn.
 */
export async function callTool(
  client: FerriteClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<{ text: string; isError: boolean }> {
  const tool = toolsByName.get(name);
  if (!tool) {
    return { text: `unknown tool ${name}`, isError: true };
  }
  try {
    return { text: await tool.run(client, args as Record<string, any>), isError: false };
  } catch (err) {
    if (err instanceof ApiError) {
      const hint = err.busy
        ? " (the tuner is occupied — tv_status shows by what; a recording outranks live viewing)"
        : "";
      return { text: `${name} failed: ${err.message}${hint}`, isError: true };
    }
    return {
      text: `${name} failed: ${err instanceof Error ? err.message : String(err)}`,
      isError: true,
    };
  }
}

/**
 * Resolve a channel name or alias to its service id.
 *
 * Mirrors the daemon's own lookup exactly (`config.Channels.Find`): walk
 * records in file order and check each one's name *and* aliases together, so
 * the first record that matches either wins.
 *
 * Getting this order wrong is worse than it sounds. A global "names before
 * aliases" pass looks more principled but disagrees with the daemon whenever a
 * name is also an earlier record's alias — and then tv_guide reads one
 * service's schedule while tv_watch and tv_record_start act on another. That
 * really happened: "テレビ朝日" was service 1065's name and 1064's alias, so
 * the guide came back empty for a channel that tuned fine.
 *
 * No case-insensitive fallback, for the same reason: anything this resolves
 * that the daemon would not is a divergence waiting to surprise someone.
 */
async function resolveServiceId(client: FerriteClient, channel: unknown): Promise<number> {
  const wanted = String(channel);
  const channels = await client.channels();
  const match = channels.find((c) => c.name === wanted || (c.aliases ?? []).includes(wanted));
  if (!match) {
    throw new Error(`unknown channel ${JSON.stringify(wanted)} — call tv_channels for the list`);
  }
  return match.service_id;
}
