# isdbd

Go daemon for a self-hosted ISDB-T TV stack. Owns adapter resources,
runs EPG ingestion + scheduling + recording, and serves live HLS + a
static web UI.

External binaries it drives (each in its own sibling repo):

- [`dvbr`](https://github.com/DuckFeather10086/dvbr) — tune / scan / EPG
- [`b25`](https://github.com/DuckFeather10086/libaribb25-rs) — ARIB B25 descrambler
- `ffmpeg` — remux / encode for HLS and recordings

## Status

Substantively implemented in Go (race-clean tests across the board);
**not yet wired to b25** and **not exercised against real hardware**.
See `CLAUDE.md` "Current implementation status" for the per-package
breakdown.

## Layout

```
cmd/isdbd/           entrypoint
internal/
  config/            channels.json + daemon TOML
  proc/              subprocess helpers (pgrp, stderr-to-slog)
  tuner/             adapter pool + dvbr CLI wrapper + leases
  fanout/            TS stream 1→N broadcaster (chunk pool, drop on slow)
  store/             sqlite (epg events, recordings, schedules)
  epg/               periodic dvbr epg ingest
  recorder/          recording job lifecycle
  scheduler/         cron-driven recording trigger
  hls/               ffmpeg subprocess + m3u8 serving
  api/               chi router + handlers
  web/               embedded Next.js static export
configs/             example daemon config
scripts/             systemd unit etc.
```

## Build

```bash
go build ./...
```

Real run is not wired yet.

## Workspace

This repo is meant to be cloned alongside the Rust crates under a
single `~/code/isdb-workspace/` dir. See that dir's `README.md`.
