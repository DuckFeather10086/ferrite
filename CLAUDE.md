# CLAUDE.md

Guidance for Claude Code in this repo.

## What this is

`isdbd` is the Go daemon for a self-hosted ISDB-T TV stack. It owns
the DVB adapter(s), runs EPG ingestion, schedules and executes
recordings, and serves live HLS + a static web UI.

It is **the orchestrator** — the heavy lifting is done by external
binaries spawned as subprocesses:

- `dvbr` (Rust, sibling repo) — tune frontend, parse PAT/PMT, tap PIDs,
  emit TS on stdout; also `dvbr epg` for EIT ingestion.
- `b25` (Rust, sibling repo `libaribb25-rs`) — ARIB B25 descrambler;
  reads encrypted TS on stdin, writes plain TS on stdout.
- `ffmpeg` — TS → HLS remux + AAC re-encode; or TS → MP4 for recordings.

Pipeline (per active tune):
```
dvbr stdout → b25 stdin → b25 stdout → fanout.Broadcaster
                                        ├─ recorder writer
                                        └─ hls ffmpeg stdin → m3u8 + .ts
```

## Layout

- `cmd/isdbd/` — entrypoint.
- `internal/`:
  - `config/` — load channels.json (shared format with `dvbr`) + daemon TOML.
  - `proc/` — subprocess helpers: `setpgid`, kill-by-pgrp, stderr→slog.
    Mirrors the contracts in the legacy `live_hls.py` (see archived
    `isdb-test` repo) — especially the **adapter lock** convention:
    when isdbd spawns `dvbr` it should set `DVBR_SKIP_ADAPTER_LOCK=1`
    only if isdbd itself holds the flock; otherwise let dvbr lock.
  - `tuner/` — `Pool` of adapters; `Lease` represents an active
    subscription to a tuned service. Same-frequency/same-service
    leases share one underlying `dvbr` subprocess via `fanout`.
  - `fanout/` — TS broadcaster. **Slow consumer policy is drop, not
    block.** A stuck recorder must never starve live HLS.
  - `store/` — sqlite. Open with `_journal_mode=WAL` (so EPG batch
    writes don't block UI reads). No write-concurrency tuning needed.
  - `epg/` — cron-tick → `dvbr epg --json` → ingest into store.
  - `recorder/` — schedule-fired job: acquire lease, write file,
    update store row.
  - `scheduler/` — `robfig/cron` driving recordings from `schedules`
    table.
  - `hls/` — per-channel session: acquire lease → ffmpeg subprocess
    → m3u8 dir. Refcounted; teardown when last viewer leaves.
  - `api/` — chi router for `/api/...` + serves `internal/web/dist`.
  - `web/` — `//go:embed dist` of Next.js `output: 'export'` build.

## Architecture invariants

- **One process per adapter at a time.** Cross-process serialization
  is `dvbr`'s flock on `/tmp/dvbr-adapter{N}.lock`. Inside isdbd, an
  in-memory mutex on the adapter's `Pool` slot is sufficient.
- **Subprocess stderr is not /dev/null.** Pipe to slog at warn level.
  The legacy Python silently masked failures and we missed recordings.
- **Always validate the pipeline produces bytes.** Port the watchdog
  pattern from `live_hls.py`: if the source delivers nothing for N
  seconds, kill and surface a non-zero result. Recording jobs must
  fail loudly to a status row, not silently produce an empty file.
- **Channel lookup** must accept name + aliases (mirroring
  `dvbr::config::find_entry`). Don't reinvent — the canonical
  matching rules live in dvbr.

## External dependencies

- `pcscd` running + polkit rule for the invoking user (B-CAS reader).
- `ffmpeg` / `ffprobe` on `$PATH`.
- USB tuner at `/dev/dvb/adapter*/`.

## Things not to do

- Don't shell out to `dvbv5-zap`. That path is retired; use `dvbr` only.
- Don't write recordings to SD card on a Pi. Always external storage.
- Don't add CGO deps unless absolutely necessary — `modernc.org/sqlite`
  is preferred so cross-compile to arm64 stays a one-liner.
