#!/usr/bin/env bun
// MCP server exposing the TV as tools over stdio.
//
// Any MCP-capable agent can drive the TV through this — Claude Code, an
// editor, or the DeepSeek loop in agent.ts, which shares the same tool
// definitions rather than duplicating them.
//
// Register with Claude Code:
//   claude mcp add ferrite -- bun run /path/to/ferrite/agent/src/mcp.ts
//
// FERRITE_HOST points at the daemon (default http://localhost:8010).

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

import { FerriteClient } from "./client.ts";
import { callTool, tools } from "./tools.ts";

export function createServer(client = new FerriteClient()): Server {
  const server = new Server(
    { name: "ferrite", version: "0.1.0" },
    {
      capabilities: { tools: {} },
      instructions:
        "Controls a Japanese terrestrial (ISDB-T) TV tuner: live viewing, " +
        "the programme guide, and recording. There is normally a single " +
        "tuner, so only one channel can be tuned at a time and a recording " +
        "takes precedence over live viewing. Call tv_status before assuming " +
        "what the TV is doing.",
    },
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: tools.map((t) => ({
      name: t.name,
      description: t.description,
      inputSchema: t.inputSchema,
    })),
  }));

  server.setRequestHandler(CallToolRequestSchema, async (req) => {
    const { text, isError } = await callTool(
      client,
      req.params.name,
      (req.params.arguments ?? {}) as Record<string, unknown>,
    );
    return { content: [{ type: "text", text }], isError };
  });

  return server;
}

// Only connect stdio when run as a program; importing this for tests must
// not seize the process's stdin.
if (import.meta.main) {
  const server = createServer();
  await server.connect(new StdioServerTransport());
}
