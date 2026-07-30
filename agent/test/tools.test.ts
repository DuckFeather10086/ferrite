import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { FerriteClient } from "../src/client.ts";
import { callTool, tools, toolsByName } from "../src/tools.ts";
import { event, startFakeDaemon, type FakeDaemon } from "./fake-daemon.ts";

let daemon: FakeDaemon;
let client: FerriteClient;

beforeEach(() => {
  daemon = startFakeDaemon();
  client = new FerriteClient(daemon.url, 5000);
});
afterEach(() => daemon.stop());

async function call(name: string, args: Record<string, unknown> = {}) {
  const res = await callTool(client, name, args);
  return { ...res, data: res.isError ? null : JSON.parse(res.text) };
}

describe("tool list", () => {
  test("every tool has a description and an object schema", () => {
    expect(tools.length).toBeGreaterThan(5);
    for (const t of tools) {
      expect(t.name).toMatch(/^tv_[a-z_]+$/);
      // Descriptions are the agent's operating manual, not a label.
      expect(t.description.length).toBeGreaterThan(40);
      expect(t.inputSchema.type).toBe("object");
      for (const req of t.inputSchema.required ?? []) {
        expect(Object.keys(t.inputSchema.properties)).toContain(req);
      }
    }
  });

  test("names are unique", () => {
    expect(toolsByName.size).toBe(tools.length);
  });

  test("the single-tuner constraint is stated where it changes behaviour", () => {
    // An agent that doesn't know this will happily kill someone's viewing.
    expect(toolsByName.get("tv_record_start")!.description.toLowerCase()).toContain("evict");
    expect(toolsByName.get("tv_watch")!.description.toLowerCase()).toContain("busy");
  });
});

describe("tv_status", () => {
  test("reports idle", async () => {
    const { data } = await call("tv_status");
    expect(data.tuned_channel).toBeNull();
    expect(data.active_recording_ids).toEqual([]);
  });

  test("reports the tuned channel and recordings", async () => {
    daemon.state.tuned = "asahi";
    daemon.state.recording = [4, 7];
    const { data } = await call("tv_status");
    expect(data.tuned_channel).toBe("asahi");
    expect(data.active_recording_ids).toEqual([4, 7]);
    expect(data.adapters[0].priority).toBe("live");
  });
});

describe("tv_channels", () => {
  test("lists channels with aliases", async () => {
    const { data } = await call("tv_channels");
    expect(data).toHaveLength(3);
    expect(data[0]).toEqual({
      channel: "asahi",
      aliases: ["テレビ朝日"],
      service_id: 1064,
    });
  });
});

describe("tv_guide", () => {
  test("marks what is airing and renders local times", async () => {
    daemon.state.events[1064] = [
      event(1064, 1, "past", -120),
      event(1064, 2, "current", -10, 60),
      event(1064, 3, "next", 60),
    ];
    const { data } = await call("tv_guide", { channel: "asahi" });

    const current = data.find((e: any) => e.airing_now);
    expect(current.title).toBe("current");
    expect(current.event_id).toBe(2);
    // Local strings are what the model reasons about.
    expect(current.start_local).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}/);
    expect(current.minutes).toBe(60);
    expect(data.filter((e: any) => e.airing_now)).toHaveLength(1);
  });

  test("resolves a channel by alias", async () => {
    daemon.state.events[1064] = [event(1064, 9, "アニメ", 30)];
    const { data } = await call("tv_guide", { channel: "テレビ朝日" });
    expect(data[0].title).toBe("アニメ");
  });

  test("an empty guide is not an error", async () => {
    const res = await call("tv_guide", { channel: "NHK_G" });
    expect(res.isError).toBe(false);
    expect(res.data).toEqual([]);
  });

  test("an unknown channel says how to recover", async () => {
    const res = await call("tv_guide", { channel: "BBC" });
    expect(res.isError).toBe(true);
    expect(res.text).toContain("tv_channels");
  });

  test("the lookahead window is clamped", async () => {
    await call("tv_guide", { channel: "asahi", hours: 100000 });
    const epg = daemon.state.calls.find((c) => c.path === "/api/epg");
    expect(epg).toBeDefined();
  });
});

describe("tv_watch", () => {
  test("returns an absolute playlist URL and what it closed", async () => {
    daemon.state.tuned = "NHK_G";
    const { data } = await call("tv_watch", { channel: "テレビ朝日" });
    expect(data.watching).toBe("asahi");
    expect(data.playlist_url).toBe(`${daemon.url}/api/live/asahi.m3u8`);
    expect(data.closed_previous).toEqual(["NHK_G"]);
  });

  test("a busy tuner is explained, not just reported", async () => {
    daemon.state.fail["/api/live/asahi/switch"] = [
      409,
      { error: "tuner busy: held by a recording" },
    ];
    const res = await call("tv_watch", { channel: "asahi" });
    expect(res.isError).toBe(true);
    expect(res.text).toContain("tuner busy");
    expect(res.text).toContain("tv_status");
  });
});

describe("tv_off", () => {
  test("stops the tuned channel", async () => {
    daemon.state.tuned = "asahi";
    const { data } = await call("tv_off");
    expect(data.stopped).toBe("asahi");
    expect(daemon.state.tuned).toBeNull();
  });

  test("is safe when nothing is playing", async () => {
    const res = await call("tv_off");
    expect(res.isError).toBe(false);
    expect(res.data.stopped).toBeNull();
  });
});

describe("recording", () => {
  test("starts open-ended and reports the handle", async () => {
    const { data } = await call("tv_record_start", { channel: "asahi" });
    expect(data.recording_id).toBeGreaterThan(0);
    expect(data.stops).toContain("12h cap");
    const body = daemon.state.calls.find((c) => c.path === "/api/record")?.body as any;
    expect(body.duration_s).toBeUndefined();
  });

  test("minutes become seconds on the wire", async () => {
    await call("tv_record_start", { channel: "asahi", minutes: 90 });
    const body = daemon.state.calls.find((c) => c.path === "/api/record")?.body as any;
    expect(body.duration_s).toBe(5400);
  });

  test("stop without an id stops the newest", async () => {
    daemon.state.recording = [3, 11, 7];
    const { data } = await call("tv_record_stop");
    expect(data.stopped_recording_id).toBe(11);
  });

  test("stop with nothing running says so", async () => {
    const res = await call("tv_record_stop");
    expect(res.isError).toBe(true);
    expect(res.text).toContain("nothing is recording");
  });

  test("listing summarizes state and size", async () => {
    const started = await call("tv_record_start", { channel: "asahi" });
    await call("tv_record_stop", { id: started.data.recording_id });
    const { data } = await call("tv_recordings");
    expect(data[0].state).toBe("done");
    expect(data[0].megabytes).toBe(12);
  });
});

describe("tv_schedule_add", () => {
  test("takes times from the guide when given an event_id", async () => {
    daemon.state.events[1064] = [event(1064, 42, "巨人の星", 180, 60)];
    const { data } = await call("tv_schedule_add", { channel: "asahi", event_id: 42 });

    expect(data.schedule_id).toBeGreaterThan(0);
    expect(data.title).toBe("巨人の星");
    const body = daemon.state.calls.find(
      (c) => c.path === "/api/schedule" && c.method === "POST",
    )?.body as any;
    expect(body.service_id).toBe(1064);
    // Padding defaults, so a drifting broadcast is still captured.
    expect(body.lead_s).toBe(30);
    expect(body.trail_s).toBe(60);
    const span = new Date(body.end).getTime() - new Date(body.start).getTime();
    expect(span).toBe(60 * 60 * 1000);
  });

  test("a stale event_id explains itself", async () => {
    daemon.state.events[1064] = [event(1064, 1, "something else", 60)];
    const res = await call("tv_schedule_add", { channel: "asahi", event_id: 999 });
    expect(res.isError).toBe(true);
    expect(res.text).toContain("tv_guide");
  });

  test("accepts explicit times", async () => {
    const start = new Date(Date.now() + 3_600_000).toISOString();
    const end = new Date(Date.now() + 7_200_000).toISOString();
    const { data } = await call("tv_schedule_add", { channel: "NHK_G", start, end });
    expect(data.schedule_id).toBeGreaterThan(0);
  });

  test("rejects a backwards window", async () => {
    const res = await call("tv_schedule_add", {
      channel: "NHK_G",
      start: new Date(Date.now() + 7_200_000).toISOString(),
      end: new Date(Date.now() + 3_600_000).toISOString(),
    });
    expect(res.isError).toBe(true);
    expect(res.text).toContain("end must be after start");
  });

  test("requires either an event_id or both times", async () => {
    const res = await call("tv_schedule_add", { channel: "NHK_G", start: "2026-01-01T00:00:00Z" });
    expect(res.isError).toBe(true);
    expect(res.text).toContain("event_id");
  });

  test("cancel removes the timer", async () => {
    daemon.state.events[1064] = [event(1064, 7, "x", 120)];
    const added = await call("tv_schedule_add", { channel: "asahi", event_id: 7 });
    const listed = await call("tv_schedule_list");
    expect(listed.data).toHaveLength(1);

    await call("tv_schedule_cancel", { id: added.data.schedule_id });
    const after = await call("tv_schedule_list");
    expect(after.data).toHaveLength(0);
  });
});

describe("callTool", () => {
  test("an unknown tool is an error, not a throw", async () => {
    const res = await callTool(client, "tv_nope");
    expect(res.isError).toBe(true);
    expect(res.text).toContain("unknown tool");
  });

  test("a daemon that is down reports the reason", async () => {
    const dead = new FerriteClient("http://127.0.0.1:1", 500);
    const res = await callTool(dead, "tv_status");
    expect(res.isError).toBe(true);
    expect(res.text).toContain("tv_status failed");
  });
});

// Regression: a Go nil slice marshals as `null`, not `[]`. Real daemons did
// this for an empty guide and an empty recording list, and `.map` on null
// throws — so the client must tolerate it regardless of what the server does.
describe("null list bodies", () => {
  test("an empty guide arriving as null reads as no events", async () => {
    daemon.state.fail["/api/epg"] = [200, null];
    const res = await call("tv_guide", { channel: "asahi" });
    expect(res.isError).toBe(false);
    expect(res.data).toEqual([]);
  });

  test("null recordings, schedules and channels are all survivable", async () => {
    daemon.state.fail["/api/recordings"] = [200, null];
    daemon.state.fail["/api/schedule"] = [200, null];
    expect((await call("tv_recordings")).data).toEqual([]);
    expect((await call("tv_schedule_list")).data).toEqual([]);
  });
});

// Regression: channel lookup must agree with the daemon's, which walks records
// in order checking name and aliases together. A global name-first pass picks a
// different service when a name is also an earlier record's alias — and then
// the guide describes one channel while recording acts on another.
describe("channel resolution matches the daemon", () => {
  test("an earlier record's alias wins over a later record's name", async () => {
    daemon.state.channels = [
      { name: "asahi", aliases: ["テレビ朝日"], service_id: 1064 },
      { name: "テレビ朝日", service_id: 1065 },
    ];
    daemon.state.events[1064] = [event(1064, 1, "報道ステーション", -5, 60)];
    daemon.state.events[1065] = [event(1065, 2, "wrong service", -5, 60)];

    const { data } = await call("tv_guide", { channel: "テレビ朝日" });
    expect(data[0].title).toBe("報道ステーション");
  });

  test("a later record is still reachable by its own unique name", async () => {
    daemon.state.channels = [
      { name: "asahi", aliases: ["テレビ朝日"], service_id: 1064 },
      { name: "テレビ朝日_4", aliases: ["テレビ朝日"], service_id: 1065 },
    ];
    daemon.state.events[1065] = [event(1065, 2, "sub service", -5, 60)];

    const { data } = await call("tv_guide", { channel: "テレビ朝日_4" });
    expect(data[0].title).toBe("sub service");
  });
});
