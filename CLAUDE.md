# CLAUDE.md

Guidance for Claude Code in this repo.

## What this is

`ferrite` is the Go daemon for a self-hosted ISDB-T TV stack. It owns
the DVB adapter(s), runs EPG ingestion, schedules and executes
recordings, and serves live HLS + a static web UI.

It is **the orchestrator** — the heavy lifting is done by external
binaries spawned as subprocesses:

- `dvb-rs` (Rust, sibling repo) — tune frontend, parse PAT/PMT, tap PIDs,
  emit TS on stdout; also `dvb-rs epg` for EIT ingestion.
- `b25-rs` (Rust, sibling repo `libaribb25-rs`) — ARIB B25 descrambler;
  reads encrypted TS on stdin, writes plain TS on stdout.
- `arib-b24` (Rust, sibling repo `libaribb24-rs`) — ARIB STD-B24 text
  decoder. Used *inside* dvb-rs (not spawned directly by ferrite) to decode
  SDT service names and EIT programme text to UTF-8.
- `ffmpeg` — TS → HLS remux + AAC re-encode; or TS → MP4 for recordings.

Pipeline (per active tune):
```
dvb-rs stdout → b25-rs stdin → b25-rs stdout → fanout.Broadcaster
                                        ├─ recorder writer
                                        └─ hls ffmpeg stdin → m3u8 + .ts
```

## Layout

- `cmd/isdbd/` — daemon entrypoint.
- `cmd/ferrite-tui/` — terminal remote control (Bubble Tea). A pure REST
  client with no TV state of its own, so it stays consistent with the web
  UI. Runs on the machine you are sitting at (`--host` points it at the
  tuner box) and spawns a local mpv for video; with no display it declines
  to spawn and reports the stream URL instead, since over ssh the window
  would open on the tuner box. The recordings view plays a recording
  (mpv against the file endpoint — no tuner involved) and deletes one
  behind a y/n confirmation.
- `internal/`:
  - `config/` — load channels.json (shared format with `dvb-rs`) + daemon TOML.
  - `proc/` — subprocess helpers: `setpgid`, kill-by-pgrp, stderr→slog.
    Mirrors the contracts in the legacy `live_hls.py` (see archived
    `isdb-test` repo) — especially the **adapter lock** convention:
    when ferrite spawns `dvb-rs` it should set `DVBR_SKIP_ADAPTER_LOCK=1`
    only if ferrite itself holds the flock; otherwise let dvb-rs lock.
  - `tuner/` — `Pool` of adapters; `Lease` represents an active
    subscription to a tuned service. Same-frequency/same-service
    leases share one underlying `dvb-rs` subprocess via `fanout`.
  - `fanout/` — TS broadcaster. **Slow consumer policy is drop, not
    block.** A stuck recorder must never starve live HLS.
  - `store/` — sqlite. Open with `_journal_mode=WAL` (so EPG batch
    writes don't block UI reads). No write-concurrency tuning needed.
  - `epg/` — cron-tick → `dvb-rs epg --json` → ingest into store.
  - `recorder/` — schedule-fired job: acquire lease, write file,
    update store row.
  - `scheduler/` — `robfig/cron` driving recordings from `schedules`
    table.
  - `hls/` — per-channel session: acquire lease → ffmpeg subprocess
    → m3u8 dir. Refcounted; teardown when last viewer leaves. On start
    it delays audio via `-af asetpts` to correct ISDB's A/V skew — ports
    the live_hls.py auto-offset (config `ffprobe_bin` / `probe_seconds`).
    The measurement is **cached per channel** in `av_offsets`; only a
    cache miss pays the ffprobe pass. Output is normalized to square
    pixels (1440x1080 SAR 4:3 → 1920x1080 SAR 1:1) because hls.js does
    not reliably honour the SAR in the H.264 VUI.
  - `api/` — chi router for `/api/...` + serves `internal/web/dist`.
    **List endpoints must answer `[]`, not `null`** — wrap them in `orEmpty`.
    A nil slice is invisible in Go and crashes every other client.
  - `web/` — `//go:embed dist` of Next.js `output: 'export'` build.

## Current implementation status

Implemented and tested (race-clean):
- `proc.Spawn` / `proc.SpawnOpt(Stdin:true)` — subprocess + pgrp
  lifecycle, stderr→slog, stdin pipe for ffmpeg.
- `tuner.DvbrCLI.Tune` — wraps `dvb-rs tune` into a TsStream.
- `tuner.Pool` — refcounted leases, same-channel sharing, source-EOF
  recovery. Now chains `dvb-rs | b25-rs` so leases emit descrambled TS
  (see `DvbrCLI.B25Bin`; empty disables descrambling for FTA/no-card).
  Also the single adapter arbiter: `AcquireAt(prio)` / `Reserve(prio)`,
  preemption of strictly-lower-priority holders, `ErrNoAdapter` when
  nothing can be freed, and `CanServe` for cheap upfront rejection.
- `fanout.Broadcaster` — 1→N chunk fanout with drop-on-slow, pooled
  buffers.
- `store` — sqlite (WAL) with embedded migrations; CRUD for
  `epg_events`, `schedules`, `recordings`; custom MarshalJSON for
  clean API output.
- `config` — TOML daemon config + channels.json loader; `Channels.Find`
  mirrors dvbr's `find_entry`.
- `epg.Parse` + `epg.Refresher` — parse `dvb-rs epg --json` (JST→UTC),
  periodic refresh loop with cron-like interval. Each pass holds a
  `Pool.Reserve(PrioBackground)`, aborts its dvb-rs child when preempted
  (reporting `ErrPreempted`), defers the first pass by `StartupDelay`,
  and retries a preempted pass after `RetryAfterPreempt` (10m) instead
  of idling until the next 6h tick.
- `recorder.Runner` — job lifecycle: lead-in wait → Acquire →
  open file → drain chunks with startup/stall watchdogs → finalize row.
  `Job.Stop` is the graceful early finish (row → 'done' with the bytes
  written, as opposed to canceling ctx, which is 'failed'); `Job.OnStart`
  hands the new row id to the caller.
- `recorder.Manager` — "record now" jobs: `Start` (open-ended, capped at
  `MaxAdhocDuration` 12h) returns the row id, `Stop(id)` ends it,
  `StopAllAndWait` is the shutdown path. Must be called *before* the
  store closes, or the row stays stuck in state 'recording'. Its `Base`
  context must not be the daemon's signal context — cancellation would
  beat the graceful stop and mark shutdown recordings 'failed'.
- `scheduler.Scheduler` — tick → DueSchedules → reserve row →
  dispatch → finalize state.
- `hls.Manager` + `Session` — refcounted ffmpeg-per-channel, idle
  janitor, m3u8 + segments on disk. A/V offset cache (`Offsets`,
  `OffsetMaxAge`; raw measurement stored, `AudioOffsetBias` applied at
  use time so config changes need no re-probe).
- `api` (chi) — `/health`, `/stream.m3u8` (shortcut to last-opened
  HLS session), `/api/status` (with adapter occupancy), `/api/channels`,
  `/api/epg`, `/api/now`, `/api/schedule` (CRUD), `/api/recordings`
  + `/api/recordings/{id}/file` (download/stream, Range-capable) and
  `DELETE /api/recordings/{id}` (file + row),
  `/api/record` + `/api/record/{id}/stop` (record now / stop),
  `/api/live/{channel}.m3u8` + segments, `/api/live/{channel}/stop`, and
  `/api/live/{channel}/switch` (change channel: close other sessions,
  tune, wait for the playlist). Also serves the embedded web UI
  (SPA fallback) for all non-`/api` routes.
- `tuner.DvbrCLI.Tune` — chains `dvb-rs | b25-rs`: dvbr's stdout is pumped
  into `b25-rs -v 0 - -` and the descrambled stdout is the lease stream.
  A copy goroutine bridges the two pipes and closes b25's stdin on
  dvb-rs EOF; `tuneStream.Close` tears down both subprocesses.
- `internal/web` — `//go:embed all:dist` of Next.js `output: 'export'`
  (Bun). The source project lives in `ferrite/web/`; `bun run build`
  compiles it and copies `out/` → `internal/web/dist/`. hls.js is
  bundled via npm (no CDN needed — works fully offline on LAN).
- `agent/` — Bun + TypeScript. `src/tools.ts` is the single tool list ("how to
  operate the TV"), consumed both by an MCP server (`src/mcp.ts`, stdio) and a
  DeepSeek tool-calling loop (`src/agent.ts`). Channel resolution there mirrors
  `config.Channels.Find` exactly — first record whose name *or* aliases match,
  in file order. Diverging means one tool reads a channel while another acts on
  a different one. `bun test` needs no network and no API key.
- `web/` — Bun + Next.js 16 (App Router, TypeScript, Tailwind CSS).
  Four pages: Live (hls.js player), Guide (EPG), Schedules (create/
  cancel), Recordings (download / delete). Static `output: 'export'`; all
  data fetching is client-side via SWR against the `/api/*` endpoints.

**Not implemented:**
- Subtitles: `arib_caption` is present in the recorded TS (and in the
  tapped service PIDs) but nothing decodes or serves it yet.
- Channel-list hygiene: duplicate records survive from the legacy
  migrate (`TOKYO MX1` + `TOKYO MX1_2` for separate service_ids,
  `515.14MHz#23864`), and several muxes carry the same service name on
  4 services, so names get `_2`/`_3` suffixes. Nothing is broken, but a
  human pass over `channels.json` would tidy it.
- Watch-one-channel-while-recording-another needs a second tuner; with
  one adapter the recording wins and live drops.

**Hardware-verified (2026-07-30, single Siano adapter, Tokyo):** EPG
preempted by record and by live; record-now → file grows ~1.4 MB/s →
stop → row 'done' (mpeg2video 1440x1080 + AAC 48k + arib_caption,
`scrambling_control` 0 across the sampled packets, 477 frames decoded
clean after the first partial GOP); `switch` between NHK_G and TBS1
(~14s cold, dominated by the 5s A/V probe + ffmpeg's first segment);
same-channel record+live sharing one tune (refs=2); SIGTERM
mid-recording finalizing as 'done'.

## Architecture invariants

- **One process per adapter at a time.** Cross-process serialization
  is `dvb-rs`'s flock on `/tmp/dvbr-adapter{N}.lock`.
- **`tuner.Pool` is the *only* in-process arbiter of an adapter.**
  Everything that touches the hardware goes through it — including work
  that drives dvb-rs out-of-process. `epg.Refresher` used to spawn
  `dvb-rs epg` directly and take the flock behind the Pool's back, which
  starved live HLS for minutes at a time (the first `GET /api/live/{ch}`
  after boot 404'd). Code that needs the adapter without a fanout takes
  a `Pool.Reserve` instead of spawning dvb-rs itself.
- **`name` is the identifier; `display_name` is the label.** Every request
  takes `name`. What a UI *shows* is `config.Channel.DisplayName()`, served
  as `display_name` on `/api/channels`, because `channels.json` mixes three
  provenances: legacy mojibake names with the real name in `aliases`
  (`NHKEFl1El5~` → `NHKEテレ1東京`), curated ASCII keys (`asahi` →
  `テレビ朝日`), and scanned names that are already fine (`J：COMテレビ`,
  whose *alias* is the mojibake). Clients render the field; they must not
  re-derive a label from the alias list — the web UI used to take
  `aliases[0]` and showed mojibake for half the list.
- **Channel lookup is first-match-wins over each record's name *and* aliases,
  in file order** (`config.Channels.Find`). Two consequences: every record must
  be selectable by its own name (a name that is an earlier record's alias makes
  the later record unreachable — `dvbr scan --merge` guards against creating
  that), and any other client must reimplement this order rather than something
  that merely looks equivalent.
- **Priority, not first-come.** `PrioRecord > PrioLive > PrioBackground`.
  A claim preempts only a *strictly* lower one, so: a recording evicts
  live playback (a missed recording is unrecoverable), live and
  recording both evict EPG, and live never evicts live — two viewers on
  different channels get `ErrNoAdapter`, which is why changing channel
  goes through `POST /api/live/{ch}/switch` (close, then open) rather
  than open-then-close.
- **A preempted holder must let go, and the preemptor must wait for it.**
  `Reservation.Preempted()` fires, the holder kills its child and calls
  `Release()`, and only then does the preemptor tune — that ordering is
  what guarantees the dead child's flock is gone. Subprocess plumbing
  needs `cmd.WaitDelay` set: without it a grandchild holding the
  inherited stdout pipe keeps `cmd.Wait` blocked and wedges the adapter.
- **A recording's `path` column is untrusted input.** The file endpoints
  resolve it against `storage_root` and refuse anything outside
  (`api.Deps.StorageRoot`, `recordingPath`). It is our own recorder that
  writes the column, but a hand-edited or corrupted row is otherwise the
  difference between a download endpoint and an arbitrary-file read — and,
  for DELETE, an arbitrary-file unlink. Keep the check on any new endpoint
  that opens a path out of the database.
- **Subprocess stderr is not /dev/null.** Pipe to slog at warn level.
  The legacy Python silently masked failures and we missed recordings.
- **Always validate the pipeline produces bytes.** Port the watchdog
  pattern from `live_hls.py`: if the source delivers nothing for N
  seconds, kill and surface a non-zero result. Recording jobs must
  fail loudly to a status row, not silently produce an empty file.
- **Channel lookup** must accept name + aliases (mirroring
  `dvbr::config::find_entry`). Don't reinvent — the canonical
  matching rules live in dvb-rs.

## Running it on this box

`make install-service` installs `scripts/ferrite.service` as a systemd
**user** unit (`@DIR@` substituted for the checkout) and symlinks
`ferrite-tui` into `~/.local/bin`. `make status` / `logs` / `restart` are
the day-to-day handles; `restart` rebuilds the Go binaries first.

Two things the unit must keep: `WorkingDirectory` at the checkout — the
config addresses `./target/release/dvbr`, `channels.json` and `./var`
relatively — and `TimeoutStopSec` well above the shutdown path's own
budget (`StopAllAndWait` 5s + HTTP 5s), or SIGTERM cuts off recording
finalization and a row stays stuck in state 'recording'.

A user unit is correct here rather than a system one: the DVB device and
the B-CAS reader are reached through the invoking user's permissions
(pcscd's polkit rule is per-user). `scripts/isdbd.service` remains for a
real deployment with binaries in `/usr/local/bin` and config in `/etc`.

## External dependencies

- `pcscd` running + polkit rule for the invoking user (B-CAS reader).
- `ffmpeg` / `ffprobe` on `$PATH`.
- USB tuner at `/dev/dvb/adapter*/`.

## Things not to do

- Don't shell out to `dvbv5-zap`. That path is retired; use `dvb-rs` only.
- Don't write recordings to SD card on a Pi. Always external storage.
- Don't add CGO deps unless absolutely necessary — `modernc.org/sqlite`
  is preferred so cross-compile to arm64 stays a one-liner.
