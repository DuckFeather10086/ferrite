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
- `arib-caption` (Rust, sibling repo `libaribcaption-rs`) — ARIB STD-B24
  *caption* decoder: reads a service TS, decodes the caption PID, and prints
  cues (`cues`, JSON) or a subtitle file (`vtt`). A Rust port of the decoder
  half of xqq/libaribcaption, with the caption model and the renderers kept
  apart. ffmpeg cannot do this — its `arib_caption` codec has no decoder
  unless built against libaribb24/libaribcaption, and Ubuntu's is not.
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
  behind a y/n confirmation. `banner.go` is the header — wordmark plus the
  watch URLs from `/api/status`; `View` counts every row the frame draws
  (see `countLines`) so a new header line cannot push the key hints off
  screen.
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
  - `caption/` — the live subtitle rendition: a second fanout consumer feeds
    `arib-caption cues`, and this segments the resulting cues into WebVTT
    beside the video segments (`subs.m3u8`, `sub{N}.vtt`, `master.m3u8`).
  - `hls/` — per-channel session: acquire lease → ffmpeg subprocess
    → m3u8 dir. Refcounted; teardown when last viewer leaves. On start
    it delays audio via `-af asetpts` to correct ISDB's A/V skew — ports
    the live_hls.py auto-offset (config `ffprobe_bin` / `probe_seconds`).
    The measurement is **cached per channel** in `av_offsets`; only a
    cache miss pays the ffprobe pass. Output is normalized to square
    pixels (1440x1080 SAR 4:3 → 1920x1080 SAR 1:1) because hls.js does
    not reliably honour the SAR in the H.264 VUI.
  - `netaddr/` — the addresses a viewer can reach this daemon at, labelled
    `local` / `lan` / `tailscale` / `public`. Only the daemon can answer
    this (a remote would be enumerating its own interfaces), so it is
    reported on `/api/status` rather than derived by clients.
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
- `api` (chi) — `/health`, `/stream.m3u8` (the single live URL: serves
  whatever is tuned, segment URIs rebased — see the invariant below),
  `/api/status` (adapter occupancy + the addresses to watch at), `/api/channels`,
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
  Four pages: Live (hls.js player + record now / stop), Guide (EPG, one
  click books a programme), Schedules (create/cancel), Recordings
  (stop a running one, download, delete). Static `output: 'export'`; all
  data fetching is client-side via SWR against the `/api/*` endpoints.
  Colour lives in one place — the `@theme` block in `app/globals.css`,
  from which Tailwind derives the utilities (`text-dim`, `border-line`),
  so components carry class names and not `style={{ color: "var(…)" }}`.
  It is near-monochrome on purpose, chroma being spent on the states that
  cost something to miss; the palette is a sibling of the TUI's
  bold/faint/reverse rather than its own idea of the product.
  Chrome is English and broadcast data is whatever the air carried —
  that split is the rule, since the UI had drifted into `Refresh` beside
  `予約` with no principle deciding which.

**Not implemented:**
- Subtitles for recordings. Live playback has its WebVTT rendition
  (`internal/caption` + `arib-caption`), and `arib-caption ass` now writes the
  richer sidecar form — ARIB's real positions, colours, cell sizes, ruby, and
  DRCS glyphs drawn as vector outlines rather than read as 〓. **ferrite does
  not run it yet**: what is missing is a post-pass after a recording finalizes,
  an endpoint to serve the sidecar (with the same `storage_root` containment as
  the file endpoints), and `--sub-file` from the TUI, since mpv does not
  auto-detect a sidecar over HTTP. The consumer is mpv either way — a browser
  cannot decode the MPEG-2 in a recording at all.
- The DRCS→Unicode replacement table (keyed by the glyph's MD5). Only the text
  forms need it now that ASS draws the glyph; `arib-caption drcs` prints what a
  stream sends, as ASCII art, which is what such a table gets built from.
- Channel-list hygiene: duplicate records survive from the legacy
  migrate (`TOKYO MX1` + `TOKYO MX1_2` for separate service_ids,
  `515.14MHz#23864`), and several muxes carry the same service name on
  4 services, so names get `_2`/`_3` suffixes. Nothing is broken, but a
  human pass over `channels.json` would tidy it.
- Watch-one-channel-while-recording-another needs a second tuner; with
  one adapter the recording wins and live drops.

**Hardware-verified (2026-08-04, live captions):** NHK 総合 at noon — caption
PID found from the PMT (0x130), cue timeline anchored by ffprobe, and the
WebVTT segment for `stream13.ts` holding exactly the cue overlapping that
segment's real PTS window (43511.667–43513.669 vs cue 43510.789–43514.476).
The browser rendering of that rendition — now the browser's own captions menu
on the Live page's player — has not been checked in a browser yet.

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
- **EPG covers a mux, not a channel.** `dvb-rs epg` taps both EIT PIDs —
  0x0012, plus **0x0027** for one-seg / 携帯 services, whose guide ARIB
  STD-B10 puts on its own PID — and returns every service in the tuned
  transport stream, each event carrying its own `service_id`. Two
  consequences: `epg.Parse` files events by that field, never by the
  channel the pass asked for, and `epg_channels` wants **one entry per
  distinct FREQUENCY** (the refresher skips a channel whose mux it already
  covered this pass). Services still empty after a pass are data services
  (`Gガイド`) or simulcast subchannels the broadcaster only publishes
  present/following for — the broadcast, not a bug.
- **A PAT entry is not necessarily a channel.** `515.14MHz#23864` was in
  the PAT (so `dvbr tune` accepted it and tapped 11 PIDs) but absent from
  the SDT, had no EIT, and delivered zero bytes — it has been dropped. Let
  the SDT decide what belongs in `channels.json`; the runtime watchdogs
  (`recorder` startup 15s, dvb-rs `DVR_STALL_TIMEOUT` 30s) are the backstop
  for anything that locks and then delivers nothing.
- **`dvb-rs scan` without `--merge` writes only the mux it scanned** and
  `--output` defaults to `channels.json`, so a bare `scan` used to replace a
  whole curated list. It now refuses when the target is an existing channel
  list (`--force` overrides). Auditing means scanning to a scratch path and
  diffing, or `--merge`.
- **Both live URLs are multivariant playlists, composed per request.**
  `/stream.m3u8` and `/api/live/{ch}.m3u8` are the same manifest — video plus
  the WebVTT caption rendition — differing only in how far their URIs reach back
  to `/api/live/`, so `masterPlaylist(channel, prefix, …)` builds it rather than
  anything writing one to disk. Both carry the captions on purpose: Safari and
  iOS play HLS natively and pick a subtitle rendition out of the manifest, so an
  iPad bookmark gets captions with nothing of ours running in the browser. The
  media playlist therefore needs a URL of its own (`{ch}/video.m3u8`) — a master
  that referenced itself is not something a player can follow — and it is served
  with ffmpeg's `-hls_base_url` prefix *stripped*, since from inside the
  channel's path those URIs would resolve one directory too deep. A rendition is
  announced only when `subs.m3u8` exists: naming a playlist that 404s makes some
  players abandon the stream. But a player reads a master *once* and never
  re-fetches it, and a session's first `subs.m3u8` lands a publish tick after the
  video playlist the request already waited for — so for a session that is
  decoding captions (`hls.Session.Captions`) `subsAnnounced` waits up to 3s for
  that first one rather than composing a manifest that silently means "this
  channel has no subtitles tonight". `{ch}/video.m3u8` only ever *serves* a
  session, it never opens one.
- **The captions control is the browser's, not ours.** hls.js turns the
  manifest's subtitle rendition into a native `TextTrack`
  (`renderTextTracksNatively`, its default), which is what puts captions in the
  player's own control bar — where a viewer already looks for them, present in
  fullscreen, and gone with the controls. An overlay button of ours was the
  first attempt and it could not be got rid of: it sits above the video, so it
  is on screen whether or not the controls are. `DEFAULT=NO` keeps captions off
  until asked, hls.js maps a selection made in that menu back onto the subtitle
  track to load, and the player carries the choice into the next channel as
  `subtitlePreference` — detaching the media clears the selection, so without
  that every channel change would quietly turn captions back off. This is also
  the only caption UI iOS Safari can be given, since hls.js does not run there.
- **Live TV is one URL, and a playlist's segment URIs are relative to
  where the playlist is served.** `/stream.m3u8` is what any player or
  bookmark gets (the `live_hls.py` contract): a channel change must not
  invalidate it, so no channel name appears in it. ffmpeg writes the segment
  URIs with `-hls_base_url {channel}/`, which resolves correctly under
  `/api/live/` and would resolve to the unrouted `/{channel}/streamN.ts`
  from the root — where the SPA fallback answers *HTML*, which a player
  reports as a corrupt stream rather than a 404. `api.rebaseSegments`
  rewrites them to `api/live/{channel}/…`, still relative so the daemon
  survives being mounted behind a path prefix. Serving the same playlist
  file from a third path means rebasing again.
- **A route's page is the `.html` file, never the directory beside it.**
  The Next export writes both: `guide.html` *and* a `guide/` holding the RSC
  payloads client-side navigation fetches. `staticHandler` therefore cannot
  treat a successful `fsys.Open(name)` as "found it" — a directory opens
  fine, and serving it produced an autoindex listing of `__next.*.txt`
  where the page belonged. It tests `isFile` and falls through to
  `name + ".html"`. This only bit a *direct* load (a bookmark, a reload,
  the URL typed in), because a tab click is handled entirely by the client
  router and never asks the server for the route — which is how it survived
  in the committed `dist` unnoticed.
- **Opening a UI must not tune.** The Live page adopts a session that is
  already running — read off `/api/status`, so a reload or a second browser
  joins what is on — but an idle box stays idle until someone asks for a
  channel. It used to tune on load, which meant merely *looking* at the UI
  took the adapter and held it for the session's whole idle timeout, and on
  a one-tuner box that is the difference between EPG running tonight and
  not. Changing channel then goes through `POST /api/live/{ch}/switch`, not
  a hand-rolled stop-then-open: equal priorities do not evict each other,
  so the wrong order deadlocks on `ErrNoAdapter`.
- **A `GET` on a channel playlist tunes that channel, so a player left polling
  one can take the adapter back.** `GET /api/live/{ch}.m3u8` opens a session,
  and live never evicts live — so between `CloseOthers` and `Acquire` inside a
  switch, one poll from the player being replaced re-tunes the old channel and
  the switch fails with `ErrNoAdapter` (this is what "Cannot play this channel /
  no adapter available" was). Two things guard it: the switch endpoint closes
  again and retries once, since a viewer asking for a channel outranks a page
  nobody is looking at, and the Live page waits a frame for hls.js to be
  destroyed before asking for the change. A failed acquire also logs what holds
  the adapter — it used to reach the browser and leave nothing in the journal.
- **Extra consumers of a tune take a `Lease.Subscribe`, not a second lease.**
  Same-channel `Acquire` does share the tune, but it makes one viewer look like
  two live claims, and if that tune had just died it would start a *new* one —
  leaving the first consumer on a dead broadcaster while a second dvb-rs fights
  the flock. The caption decode therefore subscribes to the HLS session's own
  lease and dies with it.
- **Captions are decoded by us, and `-copyts` is load-bearing.** ffmpeg has no
  ARIB caption decoder, so the HLS session drops the caption PID (`-sn -dn`)
  and `internal/caption` decodes it out of a *second* lease on the same tune
  (equal priority, so it joins the tune rather than evicting it). Cue times are
  broadcast PTS; the player reconciles them with the picture through
  `X-TIMESTAMP-MAP` by subtracting the video's first PTS — which only works
  because `-copyts` keeps the broadcast timestamps on the segments. Remove it
  and every cue lands hours away from the frame it belongs to.
- **A caption is published while it is still on screen.** An ARIB caption's end
  arrives with the *next* caption — 2 to 8 seconds later on NHK — so a rendition
  built from finished cues runs that far behind the picture, which is how the
  first working version behaved. `arib-caption cues` therefore emits each
  caption twice (`"open":true` with a provisional end, then the real one, keyed
  on `start_ms`), and a still-open cue is written out to the end of whatever
  segment is being produced: on screen *now*, for as long as it stays. Trusting
  the provisional end instead would drop a long caption out of the segments past
  it, and a player never refetches a segment it already has.
- **A subtitle rendition mirrors the video playlist, and `#EXTINF` lies.** A
  player fetches the subtitle fragment covering the position it is playing, so
  segment N of `subs.m3u8` must cover the same window as segment N of the video
  — and the only way to know which PTS that is, is to measure it (one ffprobe
  on the newest listed segment). Summing durations does not work: ffmpeg writes
  `#EXTINF:2.002` for segments that really run ~11 ms shorter, which is a
  quarter second of drift by segment 20 and more than a whole segment within
  the hour. The measurement is therefore repeated every few segments, not once.
- **`name` is the identifier; `display_name` is the label.** Every request
  takes `name`. What a UI *shows* is `config.Channel.DisplayName()`, served
  as `display_name` on `/api/channels`, because `channels.json` mixes three
  provenances: legacy mojibake names with the real name in `aliases`
  (`NHKEFl1El5~` → `NHKEテレ1東京`), curated ASCII keys (`asahi` →
  `テレビ朝日`), and scanned names that are already fine (`J：COMテレビ`,
  whose *alias* is the mojibake). Clients render the field; they must not
  re-derive a label from the alias list — the web UI used to take
  `aliases[0]` and showed mojibake for half the list.
  This binds every view, not just the channel picker. A schedule row, a
  recording row and an EPG row carry a `channel` name or a `service_id`,
  and printing either raw puts `NHKEFl1El5~` on screen. The web UI resolves
  both through one `useChannelIndex()` so a new page cannot reintroduce it;
  three of its four pages had, long after the picker was fixed.
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
