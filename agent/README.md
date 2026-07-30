# ferrite-agent

MCP tools and a DeepSeek agent for driving a [ferrite](../) ISDB-T TV daemon.

The tool list in `src/tools.ts` is the single definition of "how to operate
the TV". Two things consume it:

- `src/mcp.ts` — an MCP server over stdio, for any MCP-capable agent.
- `src/agent.ts` — a DeepSeek tool-calling loop, for plain-language requests.

Neither keeps TV state. The daemon is the source of truth, so this stays
consistent with the web UI and the TUI.

## Setup

```sh
bun install
export FERRITE_HOST=http://tuner.lan:8010   # default http://localhost:8010
export DEEPSEEK_API_KEY=...                 # or put it in agent/.env (git-ignored)
```

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

`DEEPSEEK_MODEL` overrides the model (default `deepseek-chat`);
`DEEPSEEK_BASE_URL` overrides the endpoint.

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
| `tv_recordings` | list recordings with state, size, path |
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
