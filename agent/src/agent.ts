// A tool-calling agent loop over the same tools the MCP server exposes.
//
// The model is reached over the OpenAI chat-completions protocol, which every
// provider in providers.ts speaks — DeepSeek directly, or an OrcaRouter key
// that reaches many providers behind the same shape. Nothing below this line
// knows which one answered.

import OpenAI from "openai";
import type {
  ChatCompletion,
  ChatCompletionCreateParamsNonStreaming,
  ChatCompletionMessageParam,
  ChatCompletionTool,
} from "openai/resources/chat/completions";

import { FerriteClient } from "./client.ts";
import { callTool, tools } from "./tools.ts";
import {
  explainRateLimit,
  freeTierWaitMs,
  isFreeModel,
  PROVIDERS,
  type Provider,
  resolveBaseURL,
  resolveModel,
  resolveProvider,
} from "./providers.ts";

/** Bounds one request, so a confused model can't loop on tool calls forever. */
export const MAX_STEPS = 8;

/** Longest we will sit inside one request waiting out a free-tier window. */
export const MAX_FREE_WAIT_MS = 60_000;

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

/** A configured endpoint: who answers, as what model, over which client. */
export interface AgentSetup {
  provider: Provider;
  model: string;
  openai: OpenAI;
}

export function createSetup(
  env: Record<string, string | undefined> = process.env,
): AgentSetup {
  const provider = resolveProvider(env);
  const model = resolveModel(provider, env);
  return {
    provider,
    model,
    openai: new OpenAI({
      apiKey: env[provider.keyEnv]!,
      baseURL: resolveBaseURL(provider, env),
      // Free ids invert the usual advice about 429s, and the SDK honours
      // Retry-After without a ceiling — a day-bucket window would park a
      // channel change for hours. complete() owns the policy for those.
      ...(isFreeModel(model) ? { maxRetries: 0 } : {}),
    }),
  };
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * One completion, with the free tier's two different 429s told apart: a full
 * rate window is waited out exactly once, an oversized prompt is not retried
 * at all. Anything else is rethrown as it came.
 */
async function complete(
  openai: OpenAI,
  req: ChatCompletionCreateParamsNonStreaming,
): Promise<ChatCompletion> {
  try {
    return await openai.chat.completions.create(req);
  } catch (err) {
    const waitMs = freeTierWaitMs(err, MAX_FREE_WAIT_MS);
    if (waitMs !== null) {
      await sleep(waitMs);
      return await openai.chat.completions.create(req);
    }
    const explained = explainRateLimit(err, req.model);
    throw explained ? new Error(explained, { cause: err }) : err;
  }
}

/**
 * Runs one request to completion, executing tool calls as the model asks for
 * them, and returns the final reply.
 */
export async function runAgent(prompt: string, opts: AgentOptions = {}): Promise<string> {
  const client = opts.client ?? new FerriteClient();
  // Only reach for a real endpoint when the caller didn't bring one: the tests
  // pass a stub, and must not need an API key to run.
  const setup = opts.openai ? undefined : createSetup();
  const openai = opts.openai ?? setup!.openai;
  const model = opts.model ?? setup?.model ?? PROVIDERS[0]!.defaultModel;
  const maxSteps = opts.maxSteps ?? MAX_STEPS;

  const messages: ChatCompletionMessageParam[] = [
    { role: "system", content: SYSTEM_PROMPT },
    { role: "user", content: prompt },
  ];

  for (let step = 0; step < maxSteps; step++) {
    const completion = await complete(openai, {
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
