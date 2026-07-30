// A DeepSeek-driven agent loop over the same tools the MCP server exposes.
//
// DeepSeek's API is OpenAI-compatible, so the official `openai` client works
// against api.deepseek.com — no provider-specific SDK.

import OpenAI from "openai";
import type {
  ChatCompletionMessageParam,
  ChatCompletionTool,
} from "openai/resources/chat/completions";

import { FerriteClient } from "./client.ts";
import { callTool, tools } from "./tools.ts";

export const DEFAULT_MODEL = "deepseek-chat";
export const DEFAULT_BASE_URL = "https://api.deepseek.com";

/** Bounds one request, so a confused model can't loop on tool calls forever. */
export const MAX_STEPS = 8;

export const SYSTEM_PROMPT = [
  "You operate a Japanese terrestrial (ISDB-T) television tuner through tools.",
  "",
  "Facts that change what you should do:",
  "- There is normally ONE tuner. Only one channel can be tuned at a time.",
  "- Starting a recording on a different channel evicts live playback. Say so",
  "  when you do it.",
  "- Turning the TV on and changing channel are the same operation (tv_watch).",
  "- Tuning takes a few seconds. That is normal, not a failure.",
  "- Guide data comes from periodic scans; an empty guide means that channel",
  "  has not been scanned yet, not that it is off the air.",
  "",
  "How to work:",
  "- Check tv_status before assuming what the TV is doing.",
  "- Never invent a channel name. Call tv_channels if unsure.",
  "- For something airing later, set a timer with tv_schedule_add and pass the",
  "  event_id from tv_guide rather than retyping times.",
  "- If a tool reports the tuner is busy, explain what holds it instead of",
  "  retrying blindly.",
  "",
  "Answer in the user's language, briefly, saying what you actually did.",
].join("\n");

export function toolSpecs(): ChatCompletionTool[] {
  return tools.map((t) => ({
    type: "function",
    function: {
      name: t.name,
      description: t.description,
      parameters: t.inputSchema,
    },
  }));
}

export interface AgentOptions {
  client?: FerriteClient;
  openai?: OpenAI;
  model?: string;
  maxSteps?: number;
  /** Called for each tool invocation, for progress output. */
  onToolCall?: (name: string, args: Record<string, unknown>) => void;
}

export function createOpenAI(): OpenAI {
  const apiKey = process.env.DEEPSEEK_API_KEY;
  if (!apiKey) {
    throw new Error(
      "DEEPSEEK_API_KEY is not set. Export it, or put it in agent/.env " +
        "(which is git-ignored).",
    );
  }
  return new OpenAI({
    apiKey,
    baseURL: process.env.DEEPSEEK_BASE_URL ?? DEFAULT_BASE_URL,
  });
}

/**
 * Runs one request to completion, executing tool calls as the model asks for
 * them, and returns the final reply.
 */
export async function runAgent(prompt: string, opts: AgentOptions = {}): Promise<string> {
  const client = opts.client ?? new FerriteClient();
  const openai = opts.openai ?? createOpenAI();
  const model = opts.model ?? process.env.DEEPSEEK_MODEL ?? DEFAULT_MODEL;
  const maxSteps = opts.maxSteps ?? MAX_STEPS;

  const messages: ChatCompletionMessageParam[] = [
    { role: "system", content: SYSTEM_PROMPT },
    { role: "user", content: prompt },
  ];

  for (let step = 0; step < maxSteps; step++) {
    const completion = await openai.chat.completions.create({
      model,
      messages,
      tools: toolSpecs(),
    });

    const choice = completion.choices[0];
    const message = choice?.message;
    if (!message) throw new Error("model returned no message");

    const calls = message.tool_calls ?? [];
    if (calls.length === 0) {
      return (message.content ?? "").trim();
    }

    // The assistant turn must be replayed verbatim before its tool results,
    // or the next request is missing the call the results answer.
    messages.push(message);

    for (const call of calls) {
      if (call.type !== "function") continue;
      let args: Record<string, unknown> = {};
      if (call.function.arguments) {
        try {
          args = JSON.parse(call.function.arguments) as Record<string, unknown>;
        } catch {
          messages.push({
            role: "tool",
            tool_call_id: call.id,
            content: `arguments were not valid JSON: ${call.function.arguments}`,
          });
          continue;
        }
      }
      opts.onToolCall?.(call.function.name, args);
      const { text } = await callTool(client, call.function.name, args);
      messages.push({ role: "tool", tool_call_id: call.id, content: text });
    }
  }

  return `stopped after ${maxSteps} steps without a final answer`;
}
