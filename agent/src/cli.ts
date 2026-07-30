#!/usr/bin/env bun
// Ask the TV for something in plain language.
//
//   bun run agent "今何やってる?"
//   bun run agent "テレビ朝日に変えて"
//   bun run agent "今晩のニュース録って"
//
// With no argument it reads a request per line from stdin, so it can sit
// behind any channel later (a Telegram bot, a webhook) without changing this
// file: the loop in agent.ts and the tools it drives are the reusable part.

import { FerriteClient } from "./client.ts";
import { createOpenAI, runAgent } from "./agent.ts";

const client = new FerriteClient();
const openai = createOpenAI();

const verbose = process.env.FERRITE_AGENT_QUIET !== "1";
const onToolCall = verbose
  ? (name: string, args: Record<string, unknown>) => {
      const shown = Object.keys(args).length ? ` ${JSON.stringify(args)}` : "";
      process.stderr.write(`  · ${name}${shown}\n`);
    }
  : undefined;

async function answer(prompt: string): Promise<void> {
  try {
    const reply = await runAgent(prompt, { client, openai, onToolCall });
    console.log(reply);
  } catch (err) {
    console.error(`error: ${err instanceof Error ? err.message : String(err)}`);
    process.exitCode = 1;
  }
}

const prompt = process.argv.slice(2).join(" ").trim();
if (prompt) {
  await answer(prompt);
} else if (process.stdin.isTTY) {
  console.error('usage: bun run agent "<request>"   (or pipe requests on stdin)');
  process.exit(2);
} else {
  for await (const line of console) {
    const text = line.trim();
    if (text) await answer(text);
  }
}
