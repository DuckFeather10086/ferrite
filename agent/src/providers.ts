// Which LLM endpoint the agent talks to.
//
// Every provider listed here speaks the OpenAI chat-completions protocol, so
// the official `openai` client works against all of them and the loop in
// agent.ts never learns who it is talking to: a provider is a base URL, the
// environment variable holding its key, and a default model.

/** One OpenAI-compatible endpoint the agent can be pointed at. */
export interface Provider {
  /** Value of `FERRITE_AGENT_PROVIDER` that selects this one. */
  id: string;
  /** Name for logs and error messages. */
  label: string;
  baseURL: string;
  /** Environment variable holding the API key. Its presence also auto-selects. */
  keyEnv: string;
  defaultModel: string;
  /** Where to go when the key is missing. */
  keysUrl: string;
}

// Order matters: with no explicit choice, the first provider whose key is set
// wins, so an existing DEEPSEEK_API_KEY keeps behaving exactly as it did.
export const PROVIDERS: Provider[] = [
  {
    id: "deepseek",
    label: "DeepSeek",
    baseURL: "https://api.deepseek.com",
    keyEnv: "DEEPSEEK_API_KEY",
    defaultModel: "deepseek-chat",
    keysUrl: "https://platform.deepseek.com/api_keys",
  },
  {
    id: "orcarouter",
    label: "OrcaRouter",
    baseURL: "https://api.orcarouter.ai/v1",
    keyEnv: "ORCAROUTER_API_KEY",
    // A gateway over many providers; ids are namespaced (`openai/gpt-4o-mini`,
    // `anthropic/claude-sonnet-4.6`, `deepseek/deepseek-chat`). The default is
    // the built-in router over the free tier: it sizes each request and picks
    // a free model for it, so running the TV agent costs nothing. Override with
    // FERRITE_AGENT_MODEL for a paid id.
    defaultModel: "orcarouter/free",
    keysUrl: "https://www.orcarouter.ai/console",
  },
];

export function providerById(id: string): Provider | undefined {
  return PROVIDERS.find((p) => p.id === id);
}

/**
 * Picks the provider: an explicit `FERRITE_AGENT_PROVIDER`, else the first one
 * whose key is present. Throws with the choices spelled out when neither
 * settles it — a missing key is the most common way to run this wrong.
 */
export function resolveProvider(env: Record<string, string | undefined> = process.env): Provider {
  const wanted = env.FERRITE_AGENT_PROVIDER?.trim();
  if (wanted) {
    const provider = providerById(wanted);
    if (!provider) {
      throw new Error(
        `unknown FERRITE_AGENT_PROVIDER "${wanted}". Choices: ` +
          PROVIDERS.map((p) => p.id).join(", "),
      );
    }
    if (!env[provider.keyEnv]) {
      throw new Error(
        `${provider.label} is selected but ${provider.keyEnv} is not set. ` +
          `Get a key at ${provider.keysUrl}, then export it or put it in ` +
          `agent/.env (which is git-ignored).`,
      );
    }
    return provider;
  }

  const found = PROVIDERS.find((p) => env[p.keyEnv]);
  if (found) return found;

  throw new Error(
    "no model provider is configured. Set one of: " +
      PROVIDERS.map((p) => `${p.keyEnv} (${p.label}, ${p.keysUrl})`).join("; ") +
      ". Either goes in the environment or in agent/.env (git-ignored).",
  );
}

export function resolveModel(
  provider: Provider,
  env: Record<string, string | undefined> = process.env,
): string {
  return env.FERRITE_AGENT_MODEL?.trim() || provider.defaultModel;
}

export function resolveBaseURL(
  provider: Provider,
  env: Record<string, string | undefined> = process.env,
): string {
  return env.FERRITE_AGENT_BASE_URL?.trim() || provider.baseURL;
}

/**
 * Whether a model id is served on OrcaRouter's free tier — the `-free` suffix,
 * or the `orcarouter/free` router that only ever picks from it. Free ids are
 * rate-limited on their own terms, which is why the caller needs to know.
 */
export function isFreeModel(model: string): boolean {
  return model.endsWith("-free") || model === "orcarouter/free";
}

function headerValue(err: unknown, name: string): string | null {
  const headers = (err as { headers?: unknown })?.headers;
  if (!headers) return null;
  if (typeof Headers !== "undefined" && headers instanceof Headers) return headers.get(name);
  const bag = headers as Record<string, string | undefined>;
  return bag[name] ?? bag[name.toLowerCase()] ?? null;
}

function isRateLimit(err: unknown): boolean {
  return (err as { status?: number })?.status === 429;
}

/**
 * How long to wait before retrying a free-tier rejection, or `null` for "do not
 * retry".
 *
 * The free tier answers both of its limits with the same `429`, and they want
 * opposite responses. `Retry-After` present means a rate window is full; the
 * window refills completely at its boundary rather than easing back, so the
 * right move is to wait exactly as long as you were told and try once more —
 * the opposite of the exponential backoff that ordinary quota 429s call for.
 * `Retry-After` absent means the prompt itself was over the free tier's size
 * cap, and retrying it unchanged fails identically, forever.
 *
 * A day-bucket window can be hours away, which is no use inside one TV command,
 * so anything past `capMs` is refused here and explained by `explainRateLimit`.
 */
export function freeTierWaitMs(err: unknown, capMs: number): number | null {
  if (!isRateLimit(err)) return null;
  const raw = headerValue(err, "retry-after");
  if (!raw) return null;
  const seconds = Number.parseFloat(raw);
  if (!Number.isFinite(seconds) || seconds < 0) return null;
  const waitMs = seconds * 1000;
  return waitMs <= capMs ? waitMs : null;
}

function humanDuration(seconds: number): string {
  if (seconds < 90) return `${Math.ceil(seconds)}s`;
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 90) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

/**
 * Turns a rate-limit rejection into something worth printing, or `null` if the
 * error is not one. The two free-tier cases need different advice, and neither
 * is obvious from the gateway's own message.
 */
export function explainRateLimit(err: unknown, model: string): string | null {
  if (!isRateLimit(err)) return null;
  const free = isFreeModel(model);
  const raw = headerValue(err, "retry-after");

  if (!raw) {
    return free
      ? `the request was larger than the free tier's per-request prompt cap ` +
          `(model ${model}). Waiting will not help — shorten the request, or ` +
          `set FERRITE_AGENT_MODEL to a paid id.`
      : `rate limited by the provider (model ${model}), with no Retry-After to go on.`;
  }

  const seconds = Number.parseFloat(raw);
  const waited = Number.isFinite(seconds) ? humanDuration(seconds) : raw;
  return free
    ? `the free tier is rate-limited for another ${waited} (model ${model}). ` +
        `Free ids never roll over to a paid model on their own — set ` +
        `FERRITE_AGENT_MODEL to a paid id, e.g. deepseek/deepseek-chat, to keep going.`
    : `rate limited by the provider for another ${waited} (model ${model}).`;
}
