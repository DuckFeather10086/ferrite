// Exercises the real MCP protocol over an in-memory transport pair, so a
// broken handler shows up here rather than as a silent no-op inside whatever
// agent connects to it.

import { afterEach, beforeEach, expect, test } from "bun:test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";

import { FerriteClient } from "../src/client.ts";
import { createServer } from "../src/mcp.ts";
import { event, startFakeDaemon, type FakeDaemon } from "./fake-daemon.ts";

let daemon: FakeDaemon;
let client: Client;

beforeEach(async () => {
  daemon = startFakeDaemon();
  const server = createServer(new FerriteClient(daemon.url, 5000));
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  client = new Client({ name: "test", version: "0" });
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
});

afterEach(async () => {
  await client.close();
  daemon.stop();
});

test("tools/list advertises every tool with its schema", async () => {
  const { tools } = await client.listTools();
  const names = tools.map((t) => t.name);
  expect(names).toContain("tv_status");
  expect(names).toContain("tv_watch");
  expect(names).toContain("tv_record_start");
  expect(names).toContain("tv_schedule_add");

  const watch = tools.find((t) => t.name === "tv_watch")!;
  expect(watch.inputSchema.required).toEqual(["channel"]);
  expect(watch.description).toBeTruthy();
});

test("tools/call runs a tool and returns text", async () => {
  daemon.state.tuned = "asahi";
  const res = await client.callTool({ name: "tv_status", arguments: {} });
  expect(res.isError).toBeFalsy();
  const text = (res.content as Array<{ type: string; text: string }>)[0]!.text;
  expect(JSON.parse(text).tuned_channel).toBe("asahi");
});

test("tools/call passes arguments through", async () => {
  daemon.state.events[1064] = [event(1064, 5, "報道ステーション", -5, 60)];
  const res = await client.callTool({ name: "tv_guide", arguments: { channel: "asahi" } });
  const text = (res.content as Array<{ text: string }>)[0]!.text;
  expect(text).toContain("報道ステーション");
});

test("a failing tool comes back as isError, not a protocol error", async () => {
  // The agent should be able to read the reason and choose what to do.
  const res = await client.callTool({ name: "tv_guide", arguments: { channel: "BBC" } });
  expect(res.isError).toBe(true);
  const text = (res.content as Array<{ text: string }>)[0]!.text;
  expect(text).toContain("unknown channel");
});

test("an unknown tool name is reported without killing the session", async () => {
  const res = await client.callTool({ name: "tv_nope", arguments: {} });
  expect(res.isError).toBe(true);
  // The session still works afterwards.
  const { tools } = await client.listTools();
  expect(tools.length).toBeGreaterThan(0);
});
