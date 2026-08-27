// The tool-calling loop, with the model stubbed. No network, no API key.

import { afterEach, beforeEach, expect, test } from "bun:test";

import { FerriteClient } from "../src/client.ts";
import { MAX_STEPS, runAgent, SYSTEM_PROMPT, toolSpecs } from "../src/agent.ts";
import { startFakeDaemon, type FakeDaemon } from "./fake-daemon.ts";

let daemon: FakeDaemon;
let client: FerriteClient;

beforeEach(() => {
  daemon = startFakeDaemon();
  client = new FerriteClient(daemon.url, 5000);
});
afterEach(() => daemon.stop());

/** A stub standing in for OpenAI's client, replaying scripted turns. */
function stubModel(turns: Array<Record<string, unknown>>) {
  const requests: any[] = [];
  let i = 0;
  const openai = {
    chat: {
      completions: {
        create: async (req: any) => {
          requests.push(req);
          const message = turns[Math.min(i, turns.length - 1)];
          i++;
          return { choices: [{ message }] };
        },
      },
    },
  };
  return { openai: openai as any, requests };
}

function toolCall(id: string, name: string, args: unknown) {
  return {
    id,
    type: "function",
    function: { name, arguments: JSON.stringify(args) },
  };
}

test("a plain answer is returned as-is", async () => {
  const { openai, requests } = stubModel([{ role: "assistant", content: "  hello  " }]);
  const reply = await runAgent("hi", { client, openai });

  expect(reply).toBe("hello");
  expect(requests).toHaveLength(1);
  expect(requests[0].messages[0].content).toBe(SYSTEM_PROMPT);
  expect(requests[0].messages[1]).toEqual({ role: "user", content: "hi" });
  expect(requests[0].tools.length).toBeGreaterThan(5);
});

test("a tool call is executed and its result fed back", async () => {
  daemon.state.tuned = "asahi";
  const { openai, requests } = stubModel([
    { role: "assistant", content: null, tool_calls: [toolCall("c1", "tv_status", {})] },
    { role: "assistant", content: "asahi をやってます" },
  ]);

  const seen: string[] = [];
  const reply = await runAgent("今何やってる?", {
    client,
    openai,
    onToolCall: (name) => seen.push(name),
  });

  expect(reply).toBe("asahi をやってます");
  expect(seen).toEqual(["tv_status"]);

  // The assistant turn must be replayed before its tool result, or the model
  // sees a result answering a call it never made.
  const second = requests[1].messages;
  expect(second[2].tool_calls[0].id).toBe("c1");
  expect(second[3].role).toBe("tool");
  expect(second[3].tool_call_id).toBe("c1");
  expect(JSON.parse(second[3].content).tuned_channel).toBe("asahi");
});

test("arguments reach the tool", async () => {
  const { openai } = stubModel([
    {
      role: "assistant",
      tool_calls: [toolCall("c1", "tv_record_start", { channel: "asahi", minutes: 30 })],
    },
    { role: "assistant", content: "録画開始しました" },
  ]);

  await runAgent("録って", { client, openai });
  const body = daemon.state.calls.find((c) => c.path === "/api/record")?.body as any;
  expect(body.channel).toBe("asahi");
  expect(body.duration_s).toBe(1800);
});

test("several tool calls in one turn all run", async () => {
  const { openai, requests } = stubModel([
    {
      role: "assistant",
      tool_calls: [toolCall("a", "tv_status", {}), toolCall("b", "tv_channels", {})],
    },
    { role: "assistant", content: "done" },
  ]);

  await runAgent("status and channels", { client, openai });
  const toolMsgs = requests[1].messages.filter((m: any) => m.role === "tool");
  expect(toolMsgs.map((m: any) => m.tool_call_id)).toEqual(["a", "b"]);
});

// A tool failure must reach the model as a readable result so it can choose
// what to do, rather than ending the turn.
test("a failing tool is reported back to the model", async () => {
  daemon.state.fail["/api/live/asahi/switch"] = [409, { error: "tuner busy: recording" }];
  const { openai, requests } = stubModel([
    { role: "assistant", tool_calls: [toolCall("c1", "tv_watch", { channel: "asahi" })] },
    { role: "assistant", content: "録画中なので切り替えられません" },
  ]);

  const reply = await runAgent("asahi にして", { client, openai });
  expect(reply).toContain("切り替えられません");
  const toolMsg = requests[1].messages.find((m: any) => m.role === "tool");
  expect(toolMsg.content).toContain("tuner busy");
});

test("malformed tool arguments are reported instead of throwing", async () => {
  const { openai, requests } = stubModel([
    {
      role: "assistant",
      tool_calls: [{ id: "c1", type: "function", function: { name: "tv_status", arguments: "{{{" } }],
    },
    { role: "assistant", content: "recovered" },
  ]);

  const reply = await runAgent("x", { client, openai });
  expect(reply).toBe("recovered");
  const toolMsg = requests[1].messages.find((m: any) => m.role === "tool");
  expect(toolMsg.content).toContain("not valid JSON");
});

// A model that keeps calling tools must not spin forever.
test("the loop is bounded", async () => {
  const { openai, requests } = stubModel([
    { role: "assistant", tool_calls: [toolCall("c", "tv_status", {})] },
  ]);

  const reply = await runAgent("loop", { client, openai, maxSteps: 3 });
  expect(reply).toContain("stopped after 3 steps");
  expect(requests).toHaveLength(3);
  expect(MAX_STEPS).toBeGreaterThan(1);
});

/** A model that rejects the first n calls with a rate limit, then answers. */
function stubRateLimited(failures: number, retryAfter?: string) {
  let calls = 0;
  const openai = {
    chat: {
      completions: {
        create: async () => {
          calls++;
          if (calls <= failures) {
            const headers = new Headers();
            if (retryAfter !== undefined) headers.set("retry-after", retryAfter);
            throw Object.assign(new Error("429 free_rate_limited"), {
              status: 429,
              headers,
            });
          }
          return { choices: [{ message: { role: "assistant", content: "ok" } }] };
        },
      },
    },
  };
  return { openai: openai as any, calls: () => calls };
}

// A free window refills at its boundary: wait exactly as told, once.
test("a rate-limit window is waited out and the request retried once", async () => {
  const { openai, calls } = stubRateLimited(1, "0");
  const reply = await runAgent("hi", { client, openai, model: "orcarouter/free" });

  expect(reply).toBe("ok");
  expect(calls()).toBe(2);
});

// An oversized prompt comes back as a 429 with no Retry-After, and retrying it
// unchanged fails identically — so the loop must give up and say why.
test("a rejection with no Retry-After fails with the reason", async () => {
  const { openai, calls } = stubRateLimited(1);

  await expect(
    runAgent("hi", { client, openai, model: "orcarouter/free" }),
  ).rejects.toThrow(/prompt cap/);
  expect(calls()).toBe(1);
});

test("tool specs match the MCP tool list", () => {
  // openai types tools as a union; narrow to the function variant.
  const specs = toolSpecs().filter(
    (s): s is Extract<typeof s, { type: "function" }> => s.type === "function",
  );
  expect(specs).toHaveLength(toolSpecs().length);
  const status = specs.find((s) => s.function.name === "tv_status")!;
  expect(status.function.parameters).toMatchObject({ type: "object" });
  expect(status.function.description).toBeTruthy();
});
