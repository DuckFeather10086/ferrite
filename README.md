# isdb-hub

Go orchestrator for a self-hosted ISDB-T TV stack. Owns adapter resources,
runs EPG ingestion + scheduling + recording, and serves live HLS + a
static web UI.

External components (each in its own sibling repo):

- [`dvbr`](https://github.com/DuckFeather10086/dvb-rs) — tune / scan / EPG.
  Internally depends on [`libaribb24-rs`](https://github.com/DuckFeather10086/libaribb24-rs)
  (ARIB STD-B24 text decoder) for SDT service names and EIT programme text → UTF-8.
- [`libaribb25-rs`](https://github.com/DuckFeather10086/libaribb25-rs) — ARIB STD-B25 descrambler
  (MULTI2 decrypt via B-CAS card over PC/SC). Optional for FTA-only setups.
- `ffmpeg` — remux / encode for HLS and recordings

## Status

Implemented in Go (race-clean tests). The tuner pipeline chains
`dvb-rs | b25-rs` (descrambled TS) and the daemon serves an embedded static
web UI (Live / Guide / Schedules / Recordings). See `CLAUDE.md`
"Current implementation status" for the per-package breakdown.

## Layout

```
cmd/isdbd/           entrypoint
internal/
  config/            channels.json + daemon TOML
  proc/              subprocess helpers (pgrp, stderr-to-slog)
  tuner/             adapter pool + dvb-rs CLI wrapper + leases
  fanout/            TS stream 1→N broadcaster (chunk pool, drop on slow)
  store/             sqlite (epg events, recordings, schedules)
  epg/               periodic dvb-rs epg ingest
  recorder/          recording job lifecycle
  scheduler/         cron-driven recording trigger
  hls/               ffmpeg subprocess + m3u8 serving
  api/               chi router + handlers
  web/               embedded static SPA (no-build, //go:embed)
configs/             example daemon config
scripts/             systemd unit
```

## Build

```bash
go build ./...
```

## Workspace

This repo is a submodule of [`isdb-workspace`](https://github.com/DuckFeather10086/isdb-workspace).
Clone recursively to get the full stack.
