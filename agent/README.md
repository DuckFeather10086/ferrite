# ferrite-agent

MCP tools and a tool-calling agent for driving a [ferrite](../) ISDB-T TV
daemon.

The tool list in `src/tools.ts` is the single definition of "how to operate
the TV". Two things consume it:

- `src/mcp.ts` — an MCP server over stdio, for any MCP-capable agent.
- `src/agent.ts` — a tool-calling loop, for plain-language requests.

Neither keeps TV state. The daemon is the source of truth, so this stays
consistent with the web UI and the TUI.

## Setup

```sh
bun install
export FERRITE_HOST=http://tuner.lan:8010   # default http://localhost:8010
export ORCAROUTER_API_KEY=sk-orca-...       # or put it in agent/.env (git-ignored)
```

## Model provider

The loop speaks the OpenAI chat-completions protocol, so anything that also
speaks it can drive the TV. `src/providers.ts` lists the two set up here:

| provider | key | default model |
|---|---|---|
| `deepseek` | `DEEPSEEK_API_KEY` | `deepseek-chat` |
| `orcarouter` | `ORCAROUTER_API_KEY` | `orcarouter/free` |

[OrcaRouter](https://www.orcarouter.ai) is a gateway: one key reaches OpenAI,
Anthropic, Gemini, DeepSeek and others behind the same protocol, with ids
namespaced by provider (`anthropic/claude-sonnet-4.6`, `deepseek/deepseek-chat`).
Its default here is `orcarouter/free`, a router over the free tier that sizes
each request and picks a free model for it — so the TV agent costs nothing to
run.

Set one key and it is used. Set both and `FERRITE_AGENT_PROVIDER` picks;
without it the first row wins, so an existing DeepSeek setup never moves on
its own. `FERRITE_AGENT_MODEL` overrides the model and
`FERRITE_AGENT_BASE_URL` the endpoint, for either provider.

### The free tier answers 429 two ways

They want opposite responses, so `complete()` in `agent.ts` handles them
rather than leaving it to the SDK's retries:

- **with `Retry-After`** — a rate window is full, and a free window refills
  whole at its boundary rather than easing back. Wait exactly as long as the
  header says, retry once, never back off further. Windows longer than
  `MAX_FREE_WAIT_MS` (60s) fail instead: the day bucket rolls at 00:00 UTC,
  and sitting on that inside "テレビ朝日に変えて" is worse than saying so.
- **without `Retry-After`** — the request was over the free tier's
  per-request prompt cap. Not retried at all; the same request fails
  identically however long you wait.

## Plain-language requests

```sh
bun run agent "今何やってる?"
bun run agent "テレビ朝日に変えて"
bun run agent "今晩の報道ステーション録って"
bun run agent "何が録画中?"
```

Tool calls are traced to stderr; set `FERRITE_AGENT_QUIET=1` to silence them.
With no argument it reads one request per line from stdin, which is how a
chat channel (a Telegram bot, a webhook) can sit in front of it without
touching the loop.

The first stderr line names the provider and model that answered.

## As an MCP server

```sh
claude mcp add ferrite -- bun run /path/to/ferrite/agent/src/mcp.ts
```

Then ask Claude Code directly: *"what's on TV?"*, *"record NHK for 30
minutes"*.

## Tools

| tool | what it does |
|---|---|
| `tv_status` | what is tuned, how the tuner is occupied, what is recording |
| `tv_channels` | channel list with aliases and service ids |
| `tv_guide` | now/next for a channel, with `event_id`s for timers |
| `tv_watch` | turn on / change channel (one operation), returns a playlist URL |
| `tv_off` | stop live playback, release the tuner |
| `tv_record_start` | record now, open-ended or for N minutes |
| `tv_record_stop` | stop a recording (defaults to the newest) |
| `tv_recordings` | list recordings with state, size, path and a playable URL |
| `tv_recording_delete` | delete a recording and its file (not reversible) |
| `tv_schedule_list` | pending timers |
| `tv_schedule_add` | set a timer, ideally by `event_id` from `tv_guide` |
| `tv_schedule_cancel` | cancel a timer |

The descriptions are written for an agent, and carry the operating rules:
one tuner, a recording outranks live viewing, tuning takes a few seconds, an
empty guide means "not scanned yet".

## Tests

```sh
bun test        # no network, no API key: the daemon and the model are stubbed
```

`test/fake-daemon.ts` serves the real HTTP shapes, so the tests exercise
actual request paths rather than mocked methods.
