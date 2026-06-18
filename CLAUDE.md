# CLAUDE.md

Guidance for Claude Code in this repo.

## What this is

`isdb-hub` is the Go daemon for a self-hosted ISDB-T TV stack. It owns
the DVB adapter(s), runs EPG ingestion, schedules and executes
recordings, and serves live HLS + a static web UI.

It is **the orchestrator** — the heavy lifting is done by external
binaries spawned as subprocesses:

- `dvb-rs` (Rust, sibling repo) — tune frontend, parse PAT/PMT, tap PIDs,
  emit TS on stdout; also `dvb-rs epg` for EIT ingestion.
- `b25-rs` (Rust, sibling repo `libaribb25-rs`) — ARIB B25 descrambler;
  reads encrypted TS on stdin, writes plain TS on stdout.
- `arib-b24` (Rust, sibling repo `libaribb24-rs`) — ARIB STD-B24 text
  decoder. Used *inside* dvb-rs (not spawned directly by isdb-hub) to decode
  SDT service names and EIT programme text to UTF-8.
- `ffmpeg` — TS → HLS remux + AAC re-encode; or TS → MP4 for recordings.

Pipeline (per active tune):
```
dvb-rs stdout → b25-rs stdin → b25-rs stdout → fanout.Broadcaster
                                        ├─ recorder writer
                                        └─ hls ffmpeg stdin → m3u8 + .ts
```

## Layout

- `cmd/isdbd/` — entrypoint.
- `internal/`:
  - `config/` — load channels.json (shared format with `dvb-rs`) + daemon TOML.
  - `proc/` — subprocess helpers: `setpgid`, kill-by-pgrp, stderr→slog.
    Mirrors the contracts in the legacy `live_hls.py` (see archived
    `isdb-test` repo) — especially the **adapter lock** convention:
    when isdb-hub spawns `dvb-rs` it should set `DVBR_SKIP_ADAPTER_LOCK=1`
    only if isdb-hub itself holds the flock; otherwise let dvb-rs lock.
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
    it probes the first audio/video PTS (ffprobe) and delays audio via
    `-af asetpts` to correct ISDB's A/V skew — ports the live_hls.py
    auto-offset (config `ffprobe_bin` / `probe_seconds`).
  - `api/` — chi router for `/api/...` + serves `internal/web/dist`.
  - `web/` — `//go:embed dist` of Next.js `output: 'export'` build.

## Current implementation status

Implemented and tested (race-clean):
- `proc.Spawn` / `proc.SpawnOpt(Stdin:true)` — subprocess + pgrp
  lifecycle, stderr→slog, stdin pipe for ffmpeg.
- `tuner.DvbrCLI.Tune` — wraps `dvb-rs tune` into a TsStream.
- `tuner.Pool` — refcounted leases, same-channel sharing, source-EOF
  recovery. Now chains `dvb-rs | b25-rs` so leases emit descrambled TS
  (see `DvbrCLI.B25Bin`; empty disables descrambling for FTA/no-card).
- `fanout.Broadcaster` — 1→N chunk fanout with drop-on-slow, pooled
  buffers.
- `store` — sqlite (WAL) with embedded migrations; CRUD for
  `epg_events`, `schedules`, `recordings`; custom MarshalJSON for
  clean API output.
- `config` — TOML daemon config + channels.json loader; `Channels.Find`
  mirrors dvbr's `find_entry`.
- `epg.Parse` + `epg.Refresher` — parse `dvb-rs epg --json` (JST→UTC),
  periodic refresh loop with cron-like interval.
- `recorder.Runner` — job lifecycle: lead-in wait → Acquire →
  open file → drain chunks with startup/stall watchdogs → finalize row.
- `scheduler.Scheduler` — tick → DueSchedules → reserve row →
  dispatch → finalize state.
- `hls.Manager` + `Session` — refcounted ffmpeg-per-channel, idle
  janitor, m3u8 + segments on disk.
- `api` (chi) — `/health`, `/api/status` (with adapter occupancy),
  `/api/channels`, `/api/epg`, `/api/now`, `/api/schedule` (CRUD),
  `/api/recordings`, `/api/live/{channel}.m3u8` + segments. Also serves
  the embedded web UI (SPA fallback) for all non-`/api` routes.
- `tuner.DvbrCLI.Tune` — chains `dvb-rs | b25-rs`: dvbr's stdout is pumped
  into `b25-rs -v 0 - -` and the descrambled stdout is the lease stream.
  A copy goroutine bridges the two pipes and closes b25's stdin on
  dvb-rs EOF; `tuneStream.Close` tears down both subprocesses.
- `internal/web` — hand-written no-build SPA in `dist/` (Live / Guide /
  Schedules / Recordings), `//go:embed all:dist`, mounted by `api`.

**Not implemented:**
- HLS segment URLs: the playlist is served at `/api/live/{channel}.m3u8`
  and segments at `/api/live/{channel}/{segment}`; ffmpeg is given
  `-hls_base_url {channel}/` so segment URIs resolve under the channel
  subpath. The recordings file-download endpoint
  (`/api/recordings/{id}/file`) is still a stub.
- Vendoring hls.js: the UI currently loads it from a CDN with a native
  HLS fallback (Safari/iOS). Drop a copy into `dist/` for fully offline
  LAN playback in Chrome/Firefox.
- End-to-end hardware verification — every package has unit/integration
  tests with mocks/fakes; the `dvbr | b25` chaining is covered by a
  fake-binary integration test, but nothing has been exercised against
  an actual tuner since the switch from `live_hls.py`.

## Architecture invariants

- **One process per adapter at a time.** Cross-process serialization
  is `dvb-rs`'s flock on `/tmp/dvbr-adapter{N}.lock`. Inside isdb-hub, an
  in-memory mutex on the adapter's `Pool` slot is sufficient.
- **Subprocess stderr is not /dev/null.** Pipe to slog at warn level.
  The legacy Python silently masked failures and we missed recordings.
- **Always validate the pipeline produces bytes.** Port the watchdog
  pattern from `live_hls.py`: if the source delivers nothing for N
  seconds, kill and surface a non-zero result. Recording jobs must
  fail loudly to a status row, not silently produce an empty file.
- **Channel lookup** must accept name + aliases (mirroring
  `dvbr::config::find_entry`). Don't reinvent — the canonical
  matching rules live in dvb-rs.

## External dependencies

- `pcscd` running + polkit rule for the invoking user (B-CAS reader).
- `ffmpeg` / `ffprobe` on `$PATH`.
- USB tuner at `/dev/dvb/adapter*/`.

## Things not to do

- Don't shell out to `dvbv5-zap`. That path is retired; use `dvb-rs` only.
- Don't write recordings to SD card on a Pi. Always external storage.
- Don't add CGO deps unless absolutely necessary — `modernc.org/sqlite`
  is preferred so cross-compile to arm64 stays a one-liner.
