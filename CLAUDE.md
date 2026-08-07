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
  - `caption/` — the live subtitle renditions: a second fanout consumer feeds
    `arib-caption cues --regions`, and this segments the resulting cues into
    WebVTT beside the video segments (`subs.m3u8`, `sub{N}.vtt`) *and* into the
    structured form the browser's own ARIB overlay draws (`sub{N}.json`). One
    `Pipeline` (one decoder, one subscription) per *channel*, with one
    `Rendition` attached per quality tier — the words are the same at every
    bitrate, the segment numbering is not.
  - `hls/` — one session per channel *and quality*: acquire lease → ffmpeg
    subprocess → `{hls_root}/{channel}/{quality}/`. Refcounted; teardown when
    the last viewer leaves. On start it delays audio via `-af asetpts` to
    correct ISDB's A/V skew — ports the live_hls.py auto-offset (config
    `ffprobe_bin` / `probe_seconds`). The measurement is **cached per
    channel** in `av_offsets`; only a cache miss pays the ffprobe pass.
    Output is normalized to square pixels (1440x1080 SAR 4:3 → 1920x1080
    SAR 1:1) because hls.js does not reliably honour the SAR in the H.264
    VUI. `quality.go` holds the tier list and the one place the GOP is
    derived from the segment length.
  - `scan/` — building `channels.json` from the air: sweep UHF 13–62, one
    `Pool.Reserve(PrioBackground)` per transport, `dvb-rs scan --merge
    --add-new` folding each mux that locks into the document. Runs at first
    start when there is no channel list, and on `POST /api/scan` (SSE
    progress) thereafter.
  - `postprocess/` — what happens to a recording after the tuner lets go:
    transcode to an `.mp4` a browser can open, plus `.ass`/`.vtt` sidecars
    from `arib-caption`. Serialized, niced, and it waits while any recording
    is in progress. The queue is the `post_state` column, written in the same
    statement that marks a recording done, so it survives a crash.
  - `netaddr/` — the addresses a viewer can reach this daemon at, labelled
    `local` / `lan` / `tailscale` / `public`. Only the daemon can answer
    this (a remote would be enumerating its own interfaces), so it is
    reported on `/api/status` rather than derived by clients.
  - `api/` — chi router for `/api/...` + serves `internal/web/dist`.
    **List endpoints must answer `[]`, not `null`** — wrap them in `orEmpty`.
    A nil slice is invisible in Go and crashes every other client.
  - `web/` — `//go:embed dist` of Next.js `output: 'export'` build. Which
    includes `web/public/fonts/` — the ARIB caption font travels in the binary,
    see the invariant on ASS font sizes.

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
  `/api/scan` (GET: is one running; POST: sweep the band, SSE progress),
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
  Four pages: Live (hls.js player + `ARIB | Text | Off` captions + record now /
  stop), Guide (EPG, one click books a programme), Schedules (create/cancel),
  Recordings (watch, stop a running one, convert, download, delete). Static
  `output: 'export'`; all data fetching is client-side via SWR against the
  `/api/*` endpoints.
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
- `--sub-file` from the TUI: mpv does not auto-detect a sidecar over HTTP, so
  playing a recording there needs the `.ass` URL passed explicitly.
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

**Verified in a browser and on the air (2026-08-06, live ARIB captions):** the
overlay draws what the broadcast sent. Against libass first, since that is the
cheap question: one caption of a real recording — ruby, a DRCS glyph, an
enclosure box — drawn on a canvas at 1920×1080 and the same caption burned onto
the same frame by `ffmpeg -vf ass`, with the shipped `.woff2` registered for
fontconfig so both used the one font. Ink bounding box **identical** (x
319..1528, y 852..993 against 851..993), background boxes identical (rows
838..1017), 0.5% of pixels differing by more than 8 and all of them on glyph
edges — the difference image is hairline outlines, not ghosts. Then live on this
box, headless Chromium 1217 against NHK 総合: `ARIB` enabled itself off the
manifest, the canvas had ink 2s later, and the caption came up yellow in the
broadcast's own two boxed lines with an emergency 震度4 superimpose in white on
green above it — none of which reaches the `.vtt`. Placement holds under a
letterboxed picture (box 1296 wide, picture 767.8, canvas within 0.19px of the
picture and not the box). Exactly one form at a time: switching to `Text` blanks
the canvas to `display:none` and puts one cue on the native track; taking the
bare video into fullscreen does the same and reverses on the way out, with `ARIB`
still lit as the choice. And the two clocks agree — with the text track kept
`hidden` beside the overlay, canvas ink and `activeCues` matched on **99.4%** of
180 samples over 45s across 6 captions, the only disagreement a single 0.25s
sample at a boundary.

**Verified in a browser and against libass (2026-08-06, ARIB placement):** the
same recording's `.ass`, measured in the page and burned onto its own MP4 with
ffmpeg, now agree: the caption lands at plane x 338 → ink at 340 in a 1920-wide
frame, two lines filling the background boxes the same file draws, and the
overlay tracks the picture to 0.2px as opened, resized and fullscreen. Before
the fix the overlay was laid out across the letterbox bars (box 1230px wide for a
995.6px picture) and the glyphs were 90% of the cell in the browser and 72% under
libass. With the font now served by the daemon the browser measures its line box
at 1.3950 em — the number `ass::DEFAULT_FONT_SIZE_RATIO` was derived from — so
the em comes out at exactly 36 plane units and a run of *n* cells at exactly
40 *n*: 3 cells 120, 9 cells 360, measured. The `.woff2` is served as
`font/woff2`, `immutable`, 1,015,868 bytes.

**Verified in a browser (2026-08-06, where captions sit and how they go on):**
headless Chromium 1217 against this box, caption boxes measured out of the UA
shadow tree over CDP. A recording's `.vtt` cue used to be drawn 76px above the
bottom of a 560px picture (13.6%) whether or not the control bar was up — the
browser's own reservation, unreachable by `line:-1`; with `line:94%` in the file
it lands 31px above it (5.6%), just clear of where the scrubber draws, and
`cue.line` reads back as `94`. On the Live page the new `CAPTIONS On` fetches
`subs.m3u8` and every `sub{N}.vtt`, the cues appear at the same 5.6%, and across
`TBS1 → NHK総合 → TBS1` the choice survives each channel change
(`subtitlePreference`) with the track showing on arrival. Segment clamping holds:
consecutive pieces of one caption run `19.194→20.016`, `20.016→22.017`,
`22.022→22.931` — contiguous, and never two cues on screen at once, where before
the fix `activeCues` held the same line twice.

**Verified in a browser (2026-08-05, the recordings player):** headless
Chromium against two real NHK recordings on this box. The MP4 plays
(`readyState` 4, 1920x1080); in ARIB mode ASS.js puts the captions where the
broadcast put them, background boxes and all, including the ruby over 逢引 and
the DRCS glyph the `.vtt` can only spell `〓`; the Text mode mounts one
`captions/日本語` track the browser draws at the bottom; Off mounts neither.
Our fullscreen button takes the box (1280x800 of a 1280x800 viewport) with the
ASS overlay scaled to it; the player's own button takes the video alone and the
`.vtt` stands in for the duration, ARIB returning on exit. Convert on a
`skipped` row walked `converting… → ▶ Watch` in ~7s. The
`requestVideoFrameCallback` wrinkle noted earlier **does not bite** with
assjs 0.1.9 — its `seeking` handler re-frames at the new position, so seeking
while paused keeps the caption (checked at three positions).

**Hardware-verified (2026-08-05, the recording post-pass):** a 7m04s NHK 総合
recording (888 MB) → 288 MB MP4 (H.264 1920x1080 SAR 1:1 + AAC), 1m50s wall on
the N100's iGPU, ~3.9× realtime end to end. `.ass` 46 KB and `.vtt` 8 KB
alongside; all three served over HTTP with Range. The `.ass` burned back onto
its own MP4 puts NHK's two upper caption lines where the broadcast put them,
at the frame the on-screen clock agrees with. DELETE took all four files.

**Hardware-verified (2026-08-04, live captions):** NHK 総合 at noon — caption
PID found from the PMT (0x130), cue timeline anchored by ffprobe, and the
WebVTT segment for `stream13.ts` holding exactly the cue overlapping that
segment's real PTS window (43511.667–43513.669 vs cue 43510.789–43514.476).
The browser rendering of that rendition has since been checked in a browser —
see the 2026-08-06 note above, which is also what turned up the two things wrong
with it: no reachable switch, and every caption drawn twice.

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
- **`--merge` folds *names* in; `--merge --add-new` also creates records.**
  Merging matches on SERVICE_ID + FREQUENCY, keeps whatever name a human
  curated and adds the broadcast name as an alias — which is all a rescan of
  a known mux should do. Building a list from nothing is the other job, and
  the one a fresh install has: without `--add-new`, a sweep over an empty
  document finds every service in the band and writes none of them. A service
  is only ever added when the **SDT** named it; a placeholder means the SDT
  could not be read and the names are PAT program numbers, which is not
  enough to decide what belongs in a channel list (`515.14MHz#23864` was
  exactly that — in the PAT, absent from the SDT, zero bytes when tuned).
  Those are reported in `MergeReport::nameless` so a mux that produced
  nothing says why.
- **Both live URLs are multivariant playlists, composed per request.**
  `/stream.m3u8` and `/api/live/{ch}.m3u8` are the same manifest — video plus
  the WebVTT caption rendition — differing only in how far their URIs reach back
  to `/api/live/`, so `masterPlaylist(session, quality, prefix, …)` builds it
  rather than anything writing one to disk. Both carry the captions on purpose: Safari and
  iOS play HLS natively and pick a subtitle rendition out of the manifest, so an
  iPad bookmark gets captions with nothing of ours running in the browser. The
  media playlist therefore needs a URL of its own (`{ch}/{quality}/video.m3u8`)
  — a master that referenced itself is not something a player can follow — and
  it is served with ffmpeg's `-hls_base_url` prefix *stripped*, since from
  inside the tier's path those URIs would resolve two directories too deep. A rendition is
  announced only when `subs.m3u8` exists: naming a playlist that 404s makes some
  players abandon the stream. But a player reads a master *once* and never
  re-fetches it, and a session's first `subs.m3u8` lands a publish tick after the
  video playlist the request already waited for — so for a session that is
  decoding captions (`hls.Session.Captions`) `subsAnnounced` waits up to 3s for
  that first one rather than composing a manifest that silently means "this
  channel has no subtitles tonight". `{ch}/video.m3u8` only ever *serves* a
  session, it never opens one.
- **Live captions come in two forms and exactly one is mounted, same as a
  recording's.** `ARIB | Text | Off` under the picture. *Text* is the WebVTT
  rendition drawn by the browser as a native `TextTrack` — the words at the
  bottom, and it is what an iPad gets from the manifest with nothing of ours
  running. *ARIB* is the broadcast's own caption plane on a canvas over the
  video, drawn from `sub{N}.json` by `web/src/lib/aribCaption.ts`: the colours,
  the per-cell background boxes, the enclosure rules, ruby, and the DRCS glyphs a
  `.vtt` can only spell 〓. Both at once is the same words rendered twice. The
  VTT rendition therefore stays announced in the master playlist whatever the
  overlay does — this work added a form, it did not replace one.
- **The caption *switch* has to be ours, whichever form is drawing.** Leaving it
  to the browser was the first attempt and it made live captions unreachable:
  Chromium's control bar has **no captions button at all** (checked against the
  accessibility tree — pause, fullscreen, mute, and an overflow ⋮), so the only
  way in was ⋮ → "show closed captions menu" → 日本語, two levels down a menu
  nobody opens. On the Live page the control goes *inside* the player, and so
  does every other setting the player has — captions, quality, fullscreen, in one
  bar across the top that fades with the pointer like the browser's own. Under
  the picture is where they started and it is the wrong place: everything below
  the player is unreachable for as long as the viewer is watching fullscreen,
  which is the state where a caption control matters most. The *bottom* of the
  picture is not available either, being where the native bar and the captions
  both are. Nothing is left below, so the player is now exactly the picture and
  the page's idle "▶ Watch" overlay lands on it rather than on it plus a strip.
  `DEFAULT=NO` keeps captions off until asked, and the
  choice rides into the next channel as `subtitlePreference` (detaching the media
  clears the selection, so without that every channel change would quietly turn
  captions back off). A choice made in the browser's own menu is still picked up
  rather than fought with — but only in the *on* direction: a track going
  `disabled` is what hls.js does to every track as it detaches, so adopting that
  would throw the viewer's choice away on each channel change. What the control
  reports as *available* comes from `hls.subtitleTracks`, not from the element: a
  `TextTrack` cannot be removed once added, so the last channel's track outlives
  a channel change and going by that would offer captions on a channel sending
  none.
- **In ARIB mode the text track is `hidden`, not `disabled`, and
  `hls.subtitleDisplay` is what keeps it that way.** Hidden means hls.js still
  treats the rendition as selected and keeps loading it
  (`subtitle-track-controller`'s `onTextTracksChanged` reads a hidden track as
  the current one) while the browser draws nothing — so the fullscreen fallback
  below is a mode flip rather than a wait for the next subtitle fragment. But
  hls.js re-selects the rendition after every channel change and sets it
  `showing` when it does, which would put the browser's own captions underneath
  ours; `hls.subtitleDisplay = false` is the knob that says "select it, do not
  display it", and it has to be set before the first selection, not after.
- **Fullscreen decides which caption form can be drawn here too, exactly as on
  the Recordings page.** The canvas is a sibling of the `<video>`, so the
  player's *native* fullscreen button — which takes the video alone and cannot be
  talked out of it — leaves the overlay behind on the page. `effective` therefore
  falls back from `arib` to `vtt` for as long as the bare video is fullscreen and
  reverses on the way out; the control keeps showing the viewer's choice, because
  this is a rendering fallback and not a change of mind. Our own fullscreen
  button takes the container, which is the only way to watch fullscreen with the
  broadcast's placement — so the Live player has to *have* one. It did not at
  first, only the `f` key, which left the native button as the only way in and
  therefore left ARIB captions looking as though they simply did not work
  fullscreen. It lives in the in-player bar above, inside the element being
  fullscreened.
- **The ARIB overlay puts broadcast PTS on the player's clock in the browser,
  from two numbers it is already given.** `sub{N}.json` states the PTS window it
  was cut to and hls.js states where fragment N sits on the media timeline
  (`frag.start`, in the same seconds as `video.currentTime`); the difference is a
  constant, refreshed on every `FRAG_LOADED`, and one subtraction converts every
  cue. Nothing re-times anything downstream. The sidecar is not in any playlist
  and no player asks for it — it is named after the *video* segment's own
  sequence number, which hls.js hands back as `frag.sn`, so the browser already
  knows which file it wants. Only `frag.type === 'main'`: the subtitle and audio
  renditions number their fragments separately, and an init segment has no
  sequence number at all.
- **The JSON rendition drops the clamping and keeps the extension, and the two
  are not the same kind of thing.** The clamping is about how a *player* holds
  cues: one dedups by start, end and text, so an uncut caption spanning a
  boundary reads as two cues and gets drawn twice. The overlay keys on `start_ms`
  alone and replaces in place, so a caption delivered in five segments is one
  caption seen five times — and cutting its start would make it five different
  ones. The extension is not a workaround at all: it is the *only* statement the
  publisher ever makes that a caption is still on screen. An ARIB caption's end
  arrives with the next caption, so until then all the decoder has is a guess
  (`PROVISIONAL_MS`, five seconds), and publishing that guess and stopping leaves
  every caption that outlives it missing from the windows in between, with
  nothing to say whether it ended or is simply still up. Shipped that way once:
  the hole then has to be filled at the other end, and the 30-second lifetime
  invented to fill it is what left captions on screen long after the broadcast
  had moved on.
- **So an open cue's end is the end of the window it is written into, in both
  renditions, and they have to agree to the millisecond.** An assignment, not a
  max — `writeSegment` appears to extend upward but then clamps to the same
  window, so what it really states is the window's own end, and
  `writeSegmentJSON` states the same. The overlay then takes `end_ms` exactly as
  given and invents nothing. Two renditions disagreeing about when a caption left
  the screen is a caption that jumps when the viewer switches between them, which
  is the same class of bug as the two disagreeing about `line:94%`. Checked by
  capturing both forms of every window off a live tune and comparing the cue
  counts; they matched on all of them.
- **Where a WebVTT cue sits is ours to say, and the only setting that works is a
  bare `line:<percent>`.** A browser's default placement is not the bottom of the
  picture: it reserves room for its control bar whether or not the controls are
  showing — measured at 13.6% of the picture height in Chromium 1217 — and reads
  as a subtitle floating in the lower third. Snap-to-lines cannot go below that
  reservation (`line:-1` lands in exactly the same place as `line:auto`), so the
  percentage form is the way down, and it places the box by its *bottom* edge:
  `line:94%` sits just clear of the progress bar (~96%) and a caption of two,
  three or four lines grows upward from there. Written bare, with no line
  alignment after it: WebVTT allows `line:94%,end` and hls.js parses it, but
  Chromium's own parser discards the whole setting when it sees the comma
  (`cue.line` comes back `auto`) and implements no `lineAlign` to align with. The
  live rendition goes through hls.js's parser and a recording's `.vtt` through the
  browser's, so the form has to satisfy both — and `internal/caption` and
  libaribcaption-rs's `render/vtt.rs` have to agree on it, or a channel's captions
  move when you record it.
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
- **And it is cut at the segment boundary, or it is drawn twice.** That
  provisional end is the reason: a caption spanning a boundary belongs in both
  segments, but in the first one it ends where the segment does and in the second
  it ends where the broadcast finally said. A player dedups cues by their start,
  end and text — hls.js hashes exactly those three — so those are two cues to it,
  and it draws the line twice with the stale copy hanging over the caption after
  it (this is what live captions looked like: every caption doubled, since almost
  all of them outlive a 2s segment). Nor can the publisher fix it by rewriting the
  segment: a player fetches one the moment it appears in the playlist and never
  looks again, so it always has the *first* version. `writeSegment` therefore
  clamps every cue to `[segment start, segment end)`. The pieces then meet instead
  of overlapping — the caption stays on screen across the boundary, drawn once —
  and no two cues in the rendition are ever active at the same time, which also
  matters because a percentage `line:` is not laid out to avoid collisions.
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
- **Everything derived from a recording is *named* after it, not stored.**
  `foo.ts` → `foo.mp4`, `foo.ass`, `foo.vtt`. Three more path columns would be
  three more untrusted paths to guard; deriving means they inherit the check
  the `.ts` already gets, and nothing can name a file the recording could not.
  It also means DELETE has to take them with it (`derivedExts`) — the `.mp4`
  is the biggest file of the set and orphaning it is expensive.
- **What the browser plays is the post-pass's MP4, and its captions are a DOM
  overlay.** The `.ts` is MPEG-2 video with ARIB audio and no browser will open
  it, so a recording is watchable only once `post_state` is `done` — which is
  why the Recordings page shows *Convert* (POST `…/postprocess`) on the rest
  rather than a play button that fails. The captions then come in two forms and
  exactly one is mounted at a time: ASS.js drawing the `.ass`, which keeps
  ARIB's own placement, or a `<track>` of the `.vtt`, which the browser draws
  itself. Both at once is the same words rendered twice.
- **Fullscreen decides which caption form can be drawn, so the player switches
  forms rather than losing them.** ASS.js is a DOM overlay *beside* the video,
  so it is visible only while the fullscreen element contains both: our own
  fullscreen button takes that container, and it is the only way to watch with
  ARIB placement. The player's *native* fullscreen button takes the video alone
  and cannot be talked out of it — `controlsList="nofullscreen"` does not remove
  it (Chrome draws it either way, checked against the accessibility tree), and
  re-targeting fullscreen to the container from `fullscreenchange` is a
  permissions error, because the UA's own request has already consumed the
  click's transient activation. What does work is the platform's own mechanism:
  a native TextTrack is drawn *inside* the fullscreen video. So `effective`
  falls back from `ass` to `vtt` for exactly as long as the bare video is
  fullscreen, and reverses on the way out; the toolbar keeps showing the
  viewer's choice, which this does not change.
- **`import ASS from 'assjs'` must stay dynamic, inside an effect.** The module
  calls `document.createElement` at load time, and the static export prerenders
  every page at build time, where there is no document.
- **ASS.js reads the video's size once, so it must not be constructed before the
  video has one.** It fits the caption plane the script declares to the picture,
  which it derives from `videoWidth`/`videoHeight` — and silently falls back to
  the element's own box while those are still 0. That fallback is wrong here by
  construction: the player's box is 16:9 but *wider* than the 16:9 picture inside
  it (`max-h-[70vh]` caps the height, so the video letterboxes), so the whole
  plane was stretched across the black bars — every caption ~120px left of where
  the broadcast put it and a quarter too wide, which is the "not quite right" a
  viewer reports. Nothing recovers from it: the resolution is fixed in the
  constructor and its `ResizeObserver` only watches the element, which does not
  change size when metadata arrives. So the effect waits for `loadedmetadata`.
  Measured after the fix: the ASS box matches the picture to 0.2px as opened,
  after a window resize, and in fullscreen.
- **An ASS `Fontsize` is a line box, not an em, and that makes the caption font
  load-bearing twice.** libass, following VSFilter, scales the face so that
  `usWinAscent + usWinDescent` equals the size asked for; ASS.js does the same
  through canvas `fontBoundingBox*`. So a 36-unit ARIB cell has to be asked for
  as 36 × the font's own ratio (`ass::DEFAULT_FONT_SIZE_RATIO`, 1.395 for the
  rounded gothic the script names), and asking for 36 flat — which this renderer
  did until it was measured — puts a 26-unit glyph in the cell it drew a
  background for. Then the *browser* needs the named family to actually resolve,
  because ASS.js measures the name in the script while the browser draws the
  Japanese from whatever it falls back to: two fonts, two ratios, wrong size
  again. So the font ships with the daemon — `web/public/fonts/`, which the
  export copies into `internal/web/dist` and `go:embed` puts in the binary — and
  `globals.css` declares it, with a locally installed copy preferred ahead of the
  file and the platform gothics behind it as a last resort. Nothing is fetched
  from the internet; the LAN-with-no-internet rule still holds. Two things go
  with it: the player waits on `document.fonts.load` before constructing, since
  ASS.js caches the measurement per family name, and `staticHandler` serves
  `/fonts/` `immutable` rather than `no-store` like the rest of the bundle — a
  megabyte, asked for every time a recording is opened, versioned by its own
  filename. Which is the rule for replacing it: rename the file.
- **Canvas is the mirror of that, and the ratio comes back as the baseline.**
  `ctx.font = "36px …"` **is** the em, so an ARIB cell's height goes in
  unmultiplied — writing `36 × 1.395` there, the number the `.ass` needs, would
  draw the glyph 40% too big. The font's own proportions are still load-bearing,
  just at the other end: ARIB centres the character in its cell and a font's ink
  is not centred on its baseline, so the baseline is `cell centre + (ascent −
  descent) / 2` with both read off `measureText` (`fontBoundingBoxAscent`
  /`Descent`, which is what libass's `usWinAscent + usWinDescent` comes out as in
  Chromium for this font). Both numbers are measured rather than assumed, and the
  cache they land in is thrown away when `document.fonts.load` resolves — the
  same trap ASS.js sets by measuring per family name, reached from the other
  side. Width is measured too, and only ever *squeezed*, never stretched: a
  fullwidth glyph in a half-width MSZ cell is what a television squeezes to 50%,
  and measuring keeps that right when the decoder has already substituted a
  halfwidth character or the named font did not resolve.
- **A caption plane is fitted to the picture, not to the element — in the live
  player as well.** Same trap, same 70vh height cap: past it the box is wider
  than the 16:9 video inside it and the bars are the same black as everything
  else, so laying the plane across them looks merely "not quite right". The
  overlay computes the `object-fit: contain` rectangle from
  `videoWidth`/`videoHeight` itself and positions the canvas on that; measured at
  0.19px against a picture 528px narrower than its box.
- **The file endpoints answer HEAD, not just GET.** "Is this there?" is a
  question a client asks before committing — the Recordings page asks it to
  know whether a recording has captions before offering to show them, and a
  player asks it before range-requesting. chi matches on method, so a route
  registered with `r.Get` alone answers **405** to a HEAD, and a client reads
  that as "no". `http.ServeContent` already does the right thing for HEAD, so
  the handlers need nothing; only the registration does.
- **Live picture quality is a tier the viewer picks, on demand, one at a
  time.** `[live.quality.*]` in the config; the tier is a path segment in
  every URL below the master playlist (`/api/live/{ch}/{q}/…`) and a `?q=`
  on the master, because a relative segment URI has to resolve inside the
  tier's own directory. The manifest carries **one** variant, never a ladder:
  ABR would mean standing every tier up for every viewer, which is two or
  three simultaneous H.264 encodes of the same picture on an N100's iGPU to
  serve a switch nobody on a LAN needs. Three things follow. All tiers of a
  channel share one tune and one lease (`hls.channelTune`), released when the
  last viewer of the last tier goes — a second tier costs an ffmpeg, not an
  adapter. They share one caption *decode* but get one *rendition* each,
  because a player matches a subtitle fragment to the video fragment it is
  playing and two encodes started minutes apart number their segments from
  their own zero. And `-g` is never a tier's to set: every HLS segment must
  begin with an IDR frame, so the GOP is `segmentSeconds × outputFPS`,
  appended after whatever the tier asked for.
- **The live encoder needs headroom, and running it at exactly realtime is what
  makes everything else look broken.** A single 720p live encode with
  `libx264 -preset superfast -tune zerolatency` measured **99.8% of one core**
  on this box — 1.0× realtime, no margin, with three other cores idle, because
  zerolatency trades frame threading for latency and sliced threads do not scale
  out. At that operating point any perturbation pushes the encoder behind:
  keyframes move, ffmpeg's HLS muxer cuts late and then early to catch up, and
  segments come out at 0.37s or 1.74s against an advertised `#EXTINF:1.001` —
  which then cuts WebVTT cues into 100 ms slivers, since a cue is clamped to the
  segment window it is published in. The fanout drops chunks for the encoder
  alone (`reportDrops`), so the picture is what suffers while a recording taken
  off the *same* tune stays byte-perfect: 307,018 packets, zero continuity
  breaks. So the tiers encode on the iGPU — measured 7.0× realtime at ~10% of a
  core for 720p and 4.9× at ~14% for 1080p, and 1 segment in 60 off-nominal
  where it had been 15 in 69.
  Two traps this cost a day to find. Chasing the *trigger* rather than the
  cause: the caption pipeline's ffprobe was one perturbation among many, and
  "captions on" measured 22% off-nominal against 3% off — but the baseline
  itself moved from 3% to 8% on programme content alone, so single 180s runs
  could not tell a real effect from a noisy one. And blaming the wrong
  component: `arib-caption` parses every packet of a 17 Mbit/s stream and costs
  **2.3% of a core**, so the caption decode was never the expense.
  A note for other hardware: this is per-platform configuration, not code —
  `[live] input_args` plus each tier's `output_args`, the same shape the
  post-pass transcode uses. Software (`yadif` + `libx264`) is what
  `DefaultOutputArgs` still is and what a daemon with no tier config gets. An
  ARM board wants its own encoder in that shape: `h264_v4l2m2m` on a Pi 4,
  `h264_rkmpp` on Rockchip — and nothing on a Pi 5, which has no hardware H.264
  encoder at all.
- **Segment length, GOP and playlist window move together.** `hls_time 2`,
  `-g 60` (2s × 30p after yadif), `hls_list_size 6`. The GOP has to divide the
  segment exactly, or ffmpeg cuts at the next keyframe and segment durations
  drift off the advertised `#EXTINF` — so it is derived
  (`segmentSeconds × outputFPS`) and never a tier's to set. Halving the segment
  doubles the count, so the window a player can reach back into stays ~12s. The
  player's share of the same budget is `liveSyncDurationCount` in
  `VideoPlayer.tsx`; `lowLatencyMode` there does nothing without `EXT-X-PART`
  and is kept only for the day LL-HLS is worth its compatibility cost.
- **The segment length is also the caption flicker, and that is the trade it is
  set by.** A WebVTT cue must be cut to the segment window it is published in or
  a caption spanning a boundary is drawn twice — so a caption spanning N
  segments arrives as N cues with different times, and a player, which dedups on
  start *and end and text*, tears the box down and rebuilds it at each boundary.
  That rebuild is the flicker, its rate is the segment count and nothing else,
  and no amount of cleverness about *where* to cut changes it. Measured on this
  box: at 1s a caption spanned 2.5 segments on average (1.5 rebuilds each); at
  2s, 1.7 (0.67 each, with a third of captions now fitting inside one segment
  and never rebuilding at all). The cost is latency — the floor is one segment,
  plus `liveSyncDurationCount` more that the player sits behind the edge. The
  ARIB overlay does not have the problem at any length: it keys cues by
  `start_ms` and redraws only when the caption itself changes.
- **Live segments belong on a tmpfs, and that is about writes, not latency.**
  `hls_root = "$RUNTIME_DIRECTORY/hls"`, with `RuntimeDirectory=ferrite` in the
  unit — systemd creates the directory and exports the variable, so the same
  config works for the user unit (`/run/user/{uid}/ferrite`) and the system one
  (`/run/ferrite`) with no uid written down, and `config.LiveRoot` falls back to
  `storage_root` with a warning when the variable is absent (running from the
  checkout by hand). A live session is a rolling write-and-delete of ~65 GB a
  day at 6 Mbit/s on the same disk a recording is streaming to.
  **`RuntimeDirectorySize=` is not a unit directive** — it is a logind setting
  sizing the whole of `/run/user/{uid}`, and systemd 255 answers it in a unit
  with "Unknown key name" and carries on. Nothing is lost: the usage is bounded
  by `delete_segments` + `hls_list_size 12`, about 10 MB per channel per
  quality (measured 4.3 MB with one 720p session), and the idle janitor closes
  what nobody is watching. `clearStaleSegments` stays — a reboot starts empty,
  but the same directory is reused every time a session on that channel opens.
- **An adapter is labelled with what it can tune, and a claim only ever sees
  the ones that can serve it.** `[[adapter]] n = 0, systems = ["ISDBT"]`;
  the bare `adapters = [0]` form still parses and means the same thing. The
  filter runs before idle selection *and* before preemption — evicting a
  terrestrial tune to make room for a BS channel the same frontend cannot
  receive costs a viewer their picture and gains nothing — and a channel
  nothing can receive gets `ErrNoCapableAdapter` (HTTP 501), not
  `ErrNoAdapter` (409). The distinction is the point: on a mixed card
  (PT3 = 2×T + 2×S) dispatching to the wrong half does not fail, it waits out
  the frontend lock timeout and reports a weak signal, which sends you up a
  ladder to look at the aerial.
- **A channel scan goes through the Pool like everything else, one
  reservation per transport.** A sweep owns the frontend for ten minutes or
  more; live playback and recordings have to be able to take it back, and
  reserving per-mux means a preemption is never more than one transport away.
  Being preempted ends the sweep rather than re-reserving immediately — but
  each mux is merged as it is found, so nothing is lost. A `dvbr_bin` that
  cannot be run aborts on the first transport instead of reporting fifty
  quiet frequencies, because "the whole band is empty" and "the scanner is
  not installed" are otherwise the same output.
- **The transcode's ffmpeg arguments are configuration, not a codec name.**
  `transcode.input_args` / `output_args` are whole argument lists because the
  filter chain and the encoder go together: VAAPI wants `deinterlace_vaapi` +
  `scale_vaapi`, software wants `yadif` + `scale`, and naming only the encoder
  produces an ffmpeg that fails at runtime. Whichever encoder, **scale the
  width by the source SAR** — `scale_vaapi=w=trunc(iw*sar/2)*2:h=ih,setsar=1`,
  which this ffmpeg accepts (checked on the iGPU against a real 1440x1080
  recording: out 1920x1080 SAR 1:1). A hardcoded `w=1920:h=1080` is right for
  HD only by coincidence, 1440 × 4/3 being 1920, and stretches every 4:3 SD
  subchannel into a 16:9 frame. The default is software, which
  works anywhere; this box's config selects VAAPI (measured on the N100: 4.9×
  realtime at 68% of one core, against 2.2× at 309% for libx264 — the CPU
  headroom is the point, since live HLS keeps encoding while this runs).
  Two things that bit during development: the output is written as
  `.mp4.part`, so `-f mp4` is required or ffmpeg cannot choose a muxer from
  the extension; and access to `/dev/dri/renderD128` here comes from a logind
  ACL on the *login session* rather than group membership, so a user unit that
  outlives the session loses the GPU (`usermod -aG render`).
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
config addresses `./target/release/dvb-rs`, `channels.json` and `./var`
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
