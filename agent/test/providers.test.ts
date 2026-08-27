// Which endpoint the agent picks, and how it reads a rate-limit rejection.
// No network and no API key: everything here is a pure function over an env
// bag and an error object.

import { expect, test } from "bun:test";

import {
  explainRateLimit,
  freeTierWaitMs,
  isFreeModel,
  PROVIDERS,
  providerById,
  resolveBaseURL,
  resolveModel,
  resolveProvider,
} from "../src/providers.ts";

const DEEPSEEK = { DEEPSEEK_API_KEY: "sk-deepseek" };
const ORCA = { ORCAROUTER_API_KEY: "sk-orca-x" };

test("the provider is the one whose key is set", () => {
  expect(resolveProvider(DEEPSEEK).id).toBe("deepseek");
  expect(resolveProvider(ORCA).id).toBe("orcarouter");
});

// Both keys present is the normal state once a second provider is tried, and
// it must not silently move existing traffic to the new one.
test("with both keys set, the listed order decides", () => {
  expect(resolveProvider({ ...DEEPSEEK, ...ORCA }).id).toBe(PROVIDERS[0]!.id);
  expect(PROVIDERS[0]!.id).toBe("deepseek");
});

test("an explicit choice wins over a key that is merely present", () => {
  const env = { ...DEEPSEEK, ...ORCA, FERRITE_AGENT_PROVIDER: "orcarouter" };
  expect(resolveProvider(env).id).toBe("orcarouter");
});

test("choosing a provider without its key names the key", () => {
  const env = { ...DEEPSEEK, FERRITE_AGENT_PROVIDER: "orcarouter" };
  expect(() => resolveProvider(env)).toThrow(/ORCAROUTER_API_KEY/);
});

test("an unknown provider lists the choices", () => {
  expect(() => resolveProvider({ FERRITE_AGENT_PROVIDER: "nope" })).toThrow(
    /deepseek, orcarouter/,
  );
});

test("no key at all names every way to fix it", () => {
  let message = "";
  try {
    resolveProvider({});
  } catch (err) {
    message = (err as Error).message;
  }
  for (const p of PROVIDERS) expect(message).toContain(p.keyEnv);
});

test("model and base URL fall back to the provider, then to the override", () => {
  const orca = providerById("orcarouter")!;
  expect(resolveModel(orca, ORCA)).toBe("orcarouter/free");
  expect(resolveModel(orca, { ...ORCA, FERRITE_AGENT_MODEL: "anthropic/claude-sonnet-4.6" })).toBe(
    "anthropic/claude-sonnet-4.6",
  );
  expect(resolveBaseURL(orca, ORCA)).toBe("https://api.orcarouter.ai/v1");
  expect(resolveBaseURL(orca, { ...ORCA, FERRITE_AGENT_BASE_URL: "http://localhost:9/v1" })).toBe(
    "http://localhost:9/v1",
  );
});

test("free ids are recognised by suffix and by the router alias", () => {
  expect(isFreeModel("orcarouter/free")).toBe(true);
  expect(isFreeModel("deepseek/deepseek-v4-flash-free")).toBe(true);
  expect(isFreeModel("deepseek/deepseek-chat")).toBe(false);
  expect(isFreeModel("deepseek-chat")).toBe(false);
});

/** A rejection shaped like the one the openai client raises. */
function rateLimited(retryAfter?: string) {
  const headers = new Headers();
  if (retryAfter !== undefined) headers.set("retry-after", retryAfter);
  return { status: 429, code: "free_rate_limited", headers };
}

test("a full window is waited out exactly, not backed off from", () => {
  expect(freeTierWaitMs(rateLimited("12"), 60_000)).toBe(12_000);
});

// No Retry-After means the prompt was over the size cap. Time cannot fix that,
// so it must not turn into a wait.
test("a rejection without Retry-After is never retried", () => {
  expect(freeTierWaitMs(rateLimited(), 60_000)).toBeNull();
});

// A day bucket rolls at 00:00 UTC, so Retry-After can be most of a day. Sitting
// on that inside "switch to テレビ朝日" is worse than failing.
test("a window past the cap is refused rather than slept on", () => {
  expect(freeTierWaitMs(rateLimited("43200"), 60_000)).toBeNull();
});

test("only 429s are retried at all", () => {
  expect(freeTierWaitMs({ status: 500, headers: new Headers() }, 60_000)).toBeNull();
  expect(freeTierWaitMs(new Error("socket hang up"), 60_000)).toBeNull();
  expect(explainRateLimit(new Error("socket hang up"), "orcarouter/free")).toBeNull();
});

test("the two free-tier 429s are explained differently", () => {
  const capped = explainRateLimit(rateLimited(), "orcarouter/free")!;
  expect(capped).toContain("prompt cap");
  expect(capped).toContain("Waiting will not help");

  const windowed = explainRateLimit(rateLimited("43200"), "orcarouter/free")!;
  expect(windowed).toContain("12h 0m");
  expect(windowed).toContain("FERRITE_AGENT_MODEL");
});

test("a paid model is not given free-tier advice", () => {
  const message = explainRateLimit(rateLimited("30"), "deepseek/deepseek-chat")!;
  expect(message).toContain("30s");
  expect(message).not.toContain("free tier");
});
