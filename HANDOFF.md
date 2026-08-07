# Handoff

## NEXT UP — the WebVTT caption flicker, and why it is a segmentation problem

**Symptom.** Watching live with captions in **Text** mode, the caption box
visibly blinks — it is torn down and rebuilt at every segment boundary. In
**ARIB** mode it does not.

**Mechanism, established.** A WebVTT cue has to be cut to the segment window it
is published in (`caption.writeSegment`), or a caption spanning a boundary is
published twice with different ends and a player draws it twice — that is the
`internal/caption` invariant in CLAUDE.md and it is not negotiable. But a player
dedups cues on start *and end and text* (hls.js hashes exactly those three), so
the cut pieces are **different cues** to it and it rebuilds the box at each one.
Visible in the published files:

```
sub12.vtt   09:46:36.963 --> 09:46:37.963   <舞台で共演してから>
sub13.vtt   09:46:37.963 --> 09:46:38.850   <舞台で共演してから>
            09:46:38.850 --> 09:46:38.963   <仲良くなった timelesz>   ← a 113ms sliver
sub14.vtt   09:46:38.963 --> 09:46:39.963   <仲良くなった timelesz>
```

The rate is the **segment count and nothing else** — no alignment and no
cleverness about *where* to cut changes it. Halving the segment doubles it.

**Already done, and half of it is gone.** Segments went 1s → 2s (`afe1526`),
which took a caption from spanning 2.5 segments on average to 1.7 — 1.5 rebuilds
each down to 0.67, with about a third of captions now fitting inside one segment
and never rebuilding at all. Measured off the published `.vtt` files rather than
out of a browser (`scratchpad/recue.py` pattern: count distinct segment numbers
per caption text; rebuilds = Σ(n−1)). The cost was latency, ~3s → ~6s at the
live edge, which is why it did not go further.

**The proposal that costs no latency.** Segment the *subtitle rendition* coarser
than the video. HLS allows a subtitle rendition its own segmentation — a player
matches subtitle fragments to video by **time**, not by index — so `subs.m3u8`
could publish one 8s fragment per 8 video segments and cut the rebuild rate by
4× at 2s video segments, with the video and its latency untouched.

Where to work:

- `internal/caption/pipeline.go`, `publish()`. It currently mirrors the video
  playlist one-for-one and names each piece after the video segment's sequence
  number. The change is to group N consecutive video segments into one subtitle
  window and write `subs.m3u8` with its own `#EXTINF` and `#EXT-X-MEDIA-SEQUENCE`.
- The window boundaries are already exact: `windowStarts` measures every video
  segment's first video PTS out of the segment itself (`pts.go`), so a grouped
  window is just `starts[i]` to `starts[i+N]` — no arithmetic on declared
  durations.
- **`sub{N}.json` must not move.** The ARIB overlay fetches it by
  `frag.sn` of the *video* fragment; that naming is the whole reason it needs no
  bookkeeping on either side. Only the `.vtt` grouping changes.
- A caption still gets cut at the coarser boundaries, so the doubling guard has
  to keep holding — the pieces must remain contiguous and never overlap.

**What to check afterwards, and how.** The rebuild count per caption (above);
that no two cues are ever active at once (`activeCues.length <= 1` over a run);
that a viewer joining mid-caption still sees it, since the subtitle fragment
covering the playhead is now up to 8s of content; and that the two renditions
still agree about *when* a caption is on screen — CLAUDE.md's invariant is that
they must, or switching ARIB↔Text moves the caption in time. The harness for the
last one is the ARIB-vs-VTT onset comparison that measured 99.4% agreement.

**Do not chase timing on this box by eye.** Live measurement here was noisy
until the encoder had headroom: the captions-off baseline for segment regularity
moved 3% → 8% on programme content alone, and a single 180s A/B blamed the wrong
component twice. Measure off the published files where possible; when a browser
is needed, keep the session alive (a script that only reads
`$RUNTIME_DIRECTORY/hls` never touches the API, and the idle janitor closes the
session out from under it after 60s — which silently truncated several
"180-second" runs to ~70).

## 2026-08-07 — live ARIB captions, and an encoder that had no headroom

Two PRs, merged: ferrite #10 and libaribcaption-rs #2.

**The feature.** Live captions were WebVTT only — the words, and none of the
colours, background boxes, stroke, enclosure, ruby, or the DRCS glyphs a `.vtt`
can only spell `〓`. Now `arib-caption cues --regions` serialises the whole
`model::Caption` (new `render::json`), `internal/caption` publishes it as
`sub{N}.json` beside the `.vtt`, and `web/src/lib/aribCaption.ts` draws the
broadcast's own caption plane on a canvas over the picture. The WebVTT rendition
is untouched and still announced: it is what an iPad gets from the manifest with
nothing of ours running, and the only thing visible inside a bare fullscreen
video. Verified against libass — same ink bounding box to the pixel — and on air.

**Two mistakes worth not repeating**, both written into CLAUDE.md as invariants:

1. The JSON rendition shipped first *without* the open-cue extension, on the
   reasoning that the overlay holds its own cue list so neither of WebVTT's
   adjustments applied. The clamping genuinely does not apply. The extension is
   not an adjustment at all — it is the **only statement the publisher makes
   that a caption is still on screen**, since the real end arrives with the next
   caption. Without it the segments went silent past the decoder's five-second
   guess, and the hole had to be filled at the other end with an invented
   30-second lifetime, which is what left captions on screen long after the
   broadcast moved on.
2. "The caption pipeline's ffprobe is making the segments irregular" was
   concluded from a single A/B and was **not established**. See the note above
   about measurement noise. The real cause was the encoder.

**The encoder.** ~1 live segment in 5 came out between 0.37s and 1.74s against
an advertised 1.001s. Not reception (a recording off the same tune: 307,018
packets, zero continuity breaks). Not the caption decode (`arib-caption` is
**2.3% of one core**). One 720p `libx264 -tune zerolatency` measured **99.8% of
a core** — 1.0× realtime with no margin, on a box with three cores idle, because
zerolatency trades frame threading for latency. At that operating point anything
moves a keyframe. Both live tiers are on VAAPI now: 13.9% of a core at 720p,
18% at 1080p, one segment in sixty off-nominal. `usermod -aG render` is done on
this box, which is what keeps the GPU reachable from a user unit that outlives
the login session.

**Also.** `internal/caption/pts.go` reads a segment's first video PTS itself
rather than spawning ffprobe — same number (delta 0 ms), and cheap enough that
every window is measured rather than derived, which retired `reanchorEvery`.
`hls.Manager.reportDrops` now says out loud when fanout is discarding broadcast
data because the encoder cannot keep up (47 chunks in the first 20s of a session
before VAAPI, 14 after). Caption and quality controls moved inside the player
with a fullscreen button — which is also why ARIB captions used to be invisible
fullscreen: the page had no fullscreen button of its own, so the only way in was
the native one, which takes the bare video and leaves the canvas behind.

## 2026-07-30 (last) — EPG coverage 9 → 38 services, and a list that matches the air

Reported: "quite a lot of channels still have no EPG, and what is
`515.14MHz#23864`?" Both were real, and neither was what it looked like.

**Why the guide was empty.** `dvb-rs epg` filtered EIT sections to the
service named on the command line, and `epg.Parse` stamped every event with
that service. But EIT actual-TS describes the *whole* transport stream: the
tune was already paying for a mux's worth of guide and throwing all but one
service away. Now the harvest keeps every service, each event carries its
own `service_id`, and the ingest files by it. Two dedup keys had to widen
with that — sections by (service, table, section), since section numbering
restarts per service and table, and events by (service, event), since
event_id is unique only within a service.

**One-seg needed a second PID.** ARIB STD-B10 puts the guide for
partial-reception (ワンセグ / 携帯) services on **PID 0x0027**, not 0x0012.
That single omission was every one-seg service's empty schedule. Both PIDs
are tapped now, one assembler each (continuity counters are per PID).

**`epg_channels` is now one entry per mux**, and gained the two frequencies
nobody had ever scanned (473.14 J：COM, 485.14 テレ玉). The refresher skips a
channel whose frequency it already covered this pass — a second entry on
one mux just re-collects the same EIT for another minute of tuner time.

Result on this box: **38 of 39 services carry a guide, up from 9 of 32**;
10562 events. The one holdout is `Gガイド` (sid 1183), which is a data
service broadcasting guide data *for other receivers* — it has no
programmes of its own. Subchannels that simulcast their parent legitimately
carry only 2–10 events; that is what the broadcaster sends.

**What `515.14MHz#23864` was.** Not a dead mux — 515.14 is the TOKYO MX
transport and it is healthy (1624 events on MX1). Service 23864 was in the
**PAT**, which is why `dvbr tune` accepted it and started 11 PID taps, but
absent from the **SDT**, so it had no name (hence the placeholder) and no
EIT — and its PIDs delivered **zero bytes in 12 seconds**. Dropped. The
rule going forward: let the SDT decide what is a channel; the runtime
watchdogs (recorder startup 15s, dvb-rs `DVR_STALL_TIMEOUT` 30s) already
handle "locks, then delivers nothing" by failing loudly.

**The list was also missing real services.** An audit of all ten muxes
against their SDT found eight on air but absent from `channels.json`:
フジテレビ ×3 more, 日テレ2, 日本テレビ_2, NHK総合2東京, NHK携帯G東京,
tvkワンセグ2. Added, with tuning maps derived from a sibling on the same
frequency (every DVBv5 key except SERVICE_ID belongs to the mux) and names
from the SDT, `_2`/`_3`-suffixed where they would otherwise collide with an
earlier record's name *or* alias. The three 「臨時」 event-only services were
left out on purpose: real SDT entries, but empty except during special
broadcasts.

### Near-miss worth remembering

While investigating I ran `dvbr scan --frequency 515142857` with no `-o`.
`--output` defaults to `channels.json` and a non-`--merge` scan writes
*only* the transport it scanned — so a 32-record curated list became 5 bare
records. Recovered with `git checkout channels.json`, which only worked
because it was committed. `scan` now **refuses** to overwrite an existing
channel list and names the three ways forward (`--merge`, `-o` elsewhere,
`--force`). The old docs' `scan -o channels.json` example was a loaded gun.

## 2026-07-30 (later) — channel labels: `display_name`

Reported from the TUI: "didn't we fix the mojibake channel list?" The
`scan --merge` pass on 2026-07-30 put the real broadcast names into
`aliases` and deliberately left `name` alone, so every client that
rendered `name` still showed `NHKEFl1El5~` and `NHK7HBS2`. The web UI
rendered `aliases[0]` instead, which is wrong just as often — for records
migrated from the legacy conf the first alias *is* the mojibake
(`J!'COM|ÆìÓ`).

`config.Channel.DisplayName()` decides once, server-side, and rides on
`/api/channels` as `display_name`. The rule: prefer the record's own name
when it contains kana or kanji, else the first alias that does, else the
name unchanged. Nothing tries to detect mojibake — picking the candidate
that reads as Japanese gets all 32 records right:

    asahi        → テレビ朝日      NHKEFl1El5~ → NHKEテレ1東京
    NHK_G        → NHK総合         NHK7HBS2    → NHK携帯2
    NTV          → 日テレ          FUJI        → フジテレビ
    J：COMテレビ  → (unchanged)    TBS1        → (unchanged)

Two traps it has to avoid, both pinned by tests: U+3000 must **not** count
as Japanese (`TOKYO MX1` has an alias differing only by the ideographic
space), and the `_2`/`_3` suffixes that keep four services of one mux
apart live on `name` — preferring an alias would collapse them to four
identical rows.

`name` remains the identifier everywhere. The TUI keeps the canonical name
visible next to the guide (`テレビ朝日  (asahi · sid 1064)`) since that is
what curl and `dvb-rs tune` take, and relabels adapter status and
recording rows through the same map — otherwise the status bar disagrees
with the list. Its recordings table now lays columns out by display width
instead of `%-Ns`: kana is 3 bytes and 2 cells per rune, and the state
cell carries ANSI, so fmt sheared the table.

Also: `ferrite-tui --host` now rejects a value containing `<` or `>`. A
pasted `<tunerbox>` placeholder otherwise surfaced as
`lookup <tunerbox>: no such host` beside an empty channel list, which
reads like the daemon lost `channels.json`.

## 2026-07-30 (later still) — recording download + delete

The first "Still open" item below is done: recordings can now be watched
back and thrown away, from all three front ends.

- `GET /api/recordings/{id}/file` — the raw TS via `http.ServeContent`,
  so **Range works** and a player can seek instead of streaming a
  two-hour file from the top. A row still in state 'recording' is served
  too (the bytes so far are a valid TS prefix, `Cache-Control: no-store`
  because it is still growing); a row whose file is gone answers **410**,
  not 404 — the recording exists, it just has nothing to serve.
- `DELETE /api/recordings/{id}` — file then row, answering
  `{"id":…,"file_deleted":…}`. A missing file is not an error (the row
  still goes; that is what the caller asked for), and the now-empty day
  directory is pruned. An in-progress recording is **refused with 409**:
  unlinking under the recorder leaves it writing to an inode nobody can
  reach and then finalizing a row that no longer exists. `?force=1`
  overrides, for rows stranded in 'recording' by a hard kill — the daemon
  only finalizes those on a graceful shutdown.

**`Deps.StorageRoot` is load-bearing, not decoration.** Both handlers
resolve the row's `path` against it and refuse anything outside. The
recorder writes that column, so the check never fires in normal use — but
it is a filesystem path in a database, and one bad row is otherwise the
difference between a download endpoint and an arbitrary-file read (and an
arbitrary-file *unlink*). A row pointing outside the root still loses its
row on DELETE; nothing is unlinked. Tests cover both an absolute escape
and a `..` traversal.

Where it shows up:

- **TUI** recordings view: `⏎` plays the highlighted recording in mpv
  (against the file URL — no tuner, so it never interrupts live or a
  recording), `d` then `y` deletes. Enter used to tune whatever the
  *channel* cursor was on even in this view; it now routes by view.
  Playback uses `DefaultFileArgs`, deliberately not the live
  `--profile=low-latency` — that profile shrinks the cache seeking needs.
- **Web** recordings table: a Download link and a Delete button
  (disabled while recording). `web/src/lib/api.ts`'s `Recording` type was
  wrong — it claimed `file_path` and a non-null `size_bytes` where the
  daemon sends `path` and null-until-finalized — so `fmtBytes` rendered
  "null B" on a running recording. Type and formatters now match the wire.
- **Agent/MCP**: `tv_recording_delete` (12 tools now), and every row in
  `tv_recordings` carries `play_url`. The on-disk path means nothing to a
  user sitting somewhere else; the URL is the part they can open.

### A filename bug this surfaced

`recorder.slugify` capped titles with `s[:80]` — **bytes**, not runes. Every
Japanese title long enough to hit the cap ended in a half-written UTF-8
sequence, and that invalid byte travelled into the download's
`Content-Disposition`. Now truncated on a rune boundary
(`truncateRunes`). Names are Japanese, so the header sends both forms per
RFC 6266: an ASCII-transliterated `filename` and the real name in
`filename*=UTF-8''…` (percent-encoded by hand — net/url's escapers all
leave something unencoded that is legal in a URL but not in a header
parameter).

**Verified on hardware** (single Siano adapter, asahi + NHK_G): a 20s
recording downloaded byte-identical to the file on disk (same sha256) and
ffprobed as mpeg2video 1440x1080 + AAC 48k + arib_caption, decoding clean
past the first partial GOP; `Range: bytes=4-6` → 206; an in-progress
recording served 8.6 MB mid-run; DELETE while recording → 409, then stop →
DELETE → 200 with the file and day directory gone; the same delete driven
through the TUI under a pty and through the MCP tool against the live
daemon.

Still open: nothing decodes the `arib_caption` stream; watching A while
recording B needs a 2nd tuner; Telegram front end (needs a bot token and
a chat-id allow-list).

## 2026-07-30 — adapter arbitration + record-now (EPG starvation FIXED)

The open issue below ("EPG startup scan starves live HLS") is resolved.
`epg_channels` no longer has to be empty — the default config with all
nine channels was used for the hardware run.

**What changed.** `tuner.Pool` became the single in-process arbiter with
a priority ladder (`PrioRecord` > `PrioLive` > `PrioBackground`) and
preemption of strictly-lower claims:

- `Pool.Reserve(prio)` — an exclusive hold for a consumer that drives the
  hardware out-of-process. `epg.Refresher` takes one at
  `PrioBackground`, kills its `dvb-rs epg` child on `Preempted()`, and
  `Release()`s only after the child is reaped — so the flock is gone
  before the preemptor tunes. `cmd.WaitDelay` bounds that reap (a
  grandchild holding the inherited stdout pipe would otherwise block
  `cmd.Wait` and wedge the adapter).
- `Pool.AcquireAt(prio)` — recordings claim at `PrioRecord` and evict
  live. Live never evicts live, which is why changing channel needs
  `POST /api/live/{ch}/switch` (close others, then open) — the naive
  open-then-close deadlocks on `ErrNoAdapter` with one tuner.
- EPG also defers its first pass 15s and retries a preempted pass after
  10m instead of idling until the next 6h tick.

**Record now:** `POST /api/record {channel,title?,duration_s?}` → row id;
`POST /api/record/{id}/stop`. Open-ended by default, capped at 12h.
`recorder.Job.Stop` finalizes as 'done' (ctx cancel remains 'failed').

**Two shutdown traps, both hit for real and fixed** — worth knowing
before touching `cmd/isdbd`:

1. `StopAll()` only *signals*; the deferred `st.Close()` then beat the
   recorder to `FinalizeRecording` and the row stayed stuck in state
   'recording'. Use `StopAllAndWait(5s)`, which returns only once the
   rows are written.
2. `recorder.Manager.Base` must **not** be the signal context. With it,
   SIGTERM cancels the job at the same instant shutdown asks it to stop
   gracefully, `select` picks either, and recordings land as 'failed'
   with their bytes already on disk. Shutdown reaches ad-hoc jobs only
   through `StopAllAndWait`.

### Channel-change latency: 14.4s → 7.3s

Two fixes, both measured on hardware:

1. **A/V offset is cached** (`av_offsets` table, `hls.Manager.Offsets`).
   The ffprobe pass only runs on a cache miss. Only the raw measurement
   is stored; `audio_offset_bias` is applied at use time so retuning it
   in config doesn't invalidate the cache. `GET /api/av-offsets` to
   inspect, `DELETE /api/av-offsets/{channel}` to force a re-probe.
2. **`-analyzeduration 10M` was 10 *seconds*, not 10 MB.** That single
   flag was ~9.7s of the ~14s switch (the tune itself is only ~2.3s —
   `proc.Spawn` returns before the frontend locks, so Acquire looks
   instant and the wait shows up inside ffmpeg). Now `-analyzeduration 1M
   -probesize 5M`; video+audio still both detected.

Remaining budget at 7.3s: ~2.3s tune/lock, ~1s analyze, 2s for the first
`hls_time 2` segment, plus 250ms playlist polling. `hls_time 1` is the
next knob if it needs to feel snappier.

### Aspect ratio

ISDB-T HD is coded 1440x1080 with SAR 4:3 (DAR 16:9). The transcode
passed that through and relied on the player honouring the H.264 VUI
SAR, which hls.js/MSE does not do reliably → squished picture. The
filter chain now normalizes to square pixels:
`yadif=0,scale=trunc(iw*sar/2)*2:ih,setsar=1` → **1920x1080 SAR 1:1**
(verified via ffprobe on a live segment). Expression-based rather than a
hardcoded 1920x1080 so an already-square mux or a 4:3 SD subchannel
stays geometrically correct. Recordings are unaffected — they are raw TS
`copy`, so the original SAR travels with the stream (this is why
`live_hls.py` was always right here).

### Mojibake status (asked 2026-07-30)

- EPG text: **correct**. 6847 stored titles decode as proper Japanese
  including ARIB pictograms (`幼女戦記Ⅱ`, `🈑🈖🈞`).
- Channel names: **were mojibake in channels.json** — `asahi` had alias
  `'|ÆìÓD+F|'`, `tvk1` had `'tvk|ïó»°1'`, `epg_channels` still lists
  `NHKEFl1El5~`. These are raw ARIB bytes read as Latin-1, inherited
  from `dvbr migrate` of the legacy dvbv5 `.conf`; note
  `legacy_zap_section` equalled the alias in every case — a tell that
  they never went through the B24 decoder at all.
- Root cause for why they were never refreshed: `dvb-rs scan` hung on
  this tuner. **Both fixed the same day** — see the two sections below;
  `channels.json` has been re-merged and now carries real names. The
  mojibake aliases were deliberately left in place so old references
  (including `epg_channels`) keep resolving.

### `dvb-rs scan`: two bugs, both fixed

1. **It could never run on this hardware.** `scan.rs` read PAT/SDT with
   `set_section_filter` (kernel `DMX_SET_FILTER`) — the ioctl smsusb does
   not implement — so it blocked forever (120s timeout, no output, no
   error). Now it taps the PID to DVR and reassembles sections in
   userspace, the same approach `dvbr tune`/`epg` already used. That
   logic moved into a new shared `si_reader` module and `main.rs`'s
   private copy was deleted, so there is one code path instead of two.
   **Do not reintroduce `DMX_SET_FILTER` without testing on smsusb.**
2. **`parse_sdt` had the wrong section layout,** which is why it would
   have produced nothing useful even if it had run. It read a PMT-style
   12-bit service-loop-length at `[11..13]` and used 4-byte service
   entries. Real SDT (EN 300 468 §5.2.3) has *no* loop-length field — the
   loop starts at `[11]` and runs to the CRC — and each entry header is
   5 bytes (there is an EIT-flags byte between service_id and
   descriptors_loop_length). On air this failed as "bad SDT service loop
   length" and every channel fell back to a `program_<n>` placeholder.
   The existing unit test passed because it built its fixture with the
   same wrong layout; it now builds real-layout sections, plus tests for
   a multi-service loop and a truncated trailing entry.

Result: `dvbr scan --frequency 539142857` completes in ~2.7s and reports
`テレビ朝日` for all four services.

### `scan --merge`: fixing the channel list without breaking references

`scan -o channels.json` overwrites the file with just the scanned
transport — the usage in the old docs would have wiped the other 28
records. And adopting broadcast names wholesale would break every
reference to a curated name (`live_hls.py`'s `CHANNEL_MAP`,
`epg_channels`).

`--merge` folds names into an existing document instead, matching on
SERVICE_ID + FREQUENCY, and is deliberately additive:

- a curated `name` (`asahi`, `NHK_G`, `TBS1`) is **kept** and gains the
  broadcast name as an alias;
- only auto-generated placeholders (`u1065_539142857`, `program_1064`,
  `539.14MHz#1065`) are renamed, with `_2`/`_3` suffixes on collision
  (a broadcaster's 4 services often share one service name), and the old
  placeholder is retained as an alias;
- nothing is ever deleted, so stale mojibake aliases keep resolving.

Applied across all 10 muxes: 24 of 32 records updated. `asahi` →
alias `テレビ朝日`, `NHK_G` → `NHK総合1東京`, `NHKEFl1El5~` →
`NHKEテレ1東京`, `NTV` → `日テレ1`; `u1065_539142857` → `テレビ朝日`,
`u29752_485142857` → `テレ玉1`, `u1183_527142857` → `Gガイド`.
Verified end to end: `POST /api/record {"channel":"テレビ朝日"}` resolves
to `asahi` and records. Revert with `git checkout channels.json`.

Also worth correcting the 2026-06-18 note below: 473142857 and 485142857
are **not** dead. They lock and carry J:COMテレビ and テレ玉 — they had
simply never been re-scanned.

Still open: ~~no recording download/delete endpoint~~ (done — see the
entry at the top); nothing decodes the `arib_caption` stream; watching A
while recording B needs a 2nd tuner.

## 2026-07-30 (later) — TUI remote

`cmd/ferrite-tui`, Bubble Tea, a pure REST client. It keeps no TV state:
everything comes from polling the daemon, which is what lets it and the web
UI stay consistent.

Two things about it are not obvious from the code alone:

- **It is meant to run where you are sitting, not on the tuner box.**
  `--host` / `FERRITE_HOST` points it at the daemon. Consequently the player
  URL is built absolute against that host — the switch endpoint answers with
  a *relative* playlist path, which a local mpv cannot open.
- **No display means no spawn.** Run the TUI over ssh and mpv would open its
  window on the tuner box, which is useless. Without
  DISPLAY/WAYLAND_DISPLAY it reports the stream URL instead. `--player none`
  takes the same path deliberately. Neither is treated as an error: the
  stream is up, we simply aren't the ones showing it.

Verified against the live daemon: `wire_live_test.go` (skipped unless
`FERRITE_LIVE` is set) decodes all 32 channels, adapter status and a real
EPG event, and the TUI renders under a pty.

## 2026-07-30 (later still) — MCP tools + DeepSeek agent

`agent/`, Bun + TypeScript. `src/tools.ts` holds the one tool list; the MCP
server (`src/mcp.ts`) and the DeepSeek loop (`src/agent.ts`) both consume it.
DeepSeek is OpenAI-compatible, so the `openai` client works with a base URL.

Register with Claude Code:

```sh
claude mcp add ferrite -- bun run <repo>/agent/src/mcp.ts
```

Verified live: the MCP stdio handshake against a running daemon (11 tools,
real guide data), and one DeepSeek call which read tv_status and correctly
explained that the EPG scan was holding the tuner at background priority.

Telegram is deferred — no bot token yet. `cli.ts` already reads one request
per line from stdin, so a chat channel goes in front of it without touching
the loop. **When it does get built it needs an allow-list of chat ids**: a
Telegram bot accepts messages from anyone who finds it, and these tools
change channel and start recordings.

### Three bugs that only a live daemon could show

1. **A Go nil slice marshals as `null`, not `[]`.** `/api/epg` for a channel
   with no guide data returned `null` and the TS client died on `.map`. Fixed
   server-side (`orEmpty`) *and* defensively in the client. The fake daemon in
   the tests returned `[]`, which is exactly why the tests were green.
2. **`--merge` handed service 1065 the name "テレビ朝日"**, which was already
   an alias of `asahi` (1064). Lookup is first-match-wins over each record's
   name and aliases in file order, so 1065 became unreachable by its own name.
   Fixed in dvb-rs (collisions now consider aliases) and in the data.
3. **Channel resolution has to match the daemon's order exactly.** The TS tool
   originally did a global names-before-aliases pass, which for テレビ朝日
   selected 1065 while the daemon selected 1064 — so `tv_guide` read an empty
   schedule for a channel that `tv_watch` would tune fine. Any future client
   must copy `Channels.Find`, not approximate it.

Noticed in passing, not fixed: some stored EPG titles render the katakana
prolonged sound mark as `「` (`ニュ「ス` for `ニュース`), i.e. a character
mapping gap in libaribb24-rs. Cosmetic, and in a different submodule.

## 2026-06-18 — first real-hardware E2E run of the b25 pipeline

End-to-end verified against the actual tuner through the **full isdbd
HTTP path** (not just the raw `dvbr | b25 | ffmpeg` shell pipeline):

```
GET /api/live/TOKYO MX1.m3u8
  → dvbr tune (adapter0 lock) → b25 descramble → ffmpeg HLS
  → segment served by API
```

The API-fetched segment ffprobes as **mpeg2video 1440x1080 + AAC 48kHz**,
and `ffmpeg -i seg.ts -an -f null -` decodes a clean **58-frame GOP
(4 I / 16 P / 38 B) with zero errors** → descrambling confirmed. (Note:
checking the scrambling-control bits of the *ffmpeg-output* segment proves
nothing — ffmpeg always emits clean TS. The decode succeeding is the
proof: a scrambled MPEG2 stream can't yield a decodable GOP.)

Known-good channels on this box (Tokyo/Kanagawa): `TOKYO MX1`, `TBS1`,
`asahi`, `NHKEFl1El5~`, `tvk1`. The J:COM cable muxes (473142857 /
485142857) have no antenna signal and will block until tune timeout.

### Config change in this commit

`configs/isdbd.toml`:

```
- dvbr_bin = "../dvbr/target/release/dvbr"
+ dvbr_bin = "../target/release/dvbr"
```

The `dvbr` release binary builds into the **parent virtual workspace**
`target/release/dvbr`, not `dvbr/target/release/dvbr`. (`b25_bin` at
`../libaribb25-rs/target/release/b25` was already correct.) Run isdbd
from the `isdbd/` dir so the relative bin/channels/storage paths resolve.

### To reproduce the verification

1. Build: `go build -o /tmp/isdbd ./cmd/isdbd` (Go isn't on the non-login
   PATH — `export PATH=/usr/local/go/bin:$PATH` first).
2. From the repo root: `/tmp/isdbd -config configs/isdbd.toml`
   (EPG can stay enabled since 2026-07-30 — it yields the adapter).
3. `curl /api/live/TOKYO%20MX1.m3u8`, wait ~10-15s for the first
   segment, fetch a `.ts` under `/api/live/`, then
   `ffmpeg -i seg.ts -an -f null -` should decode without errors.

## ~~OPEN ISSUE~~ FIXED 2026-07-30 — EPG startup scan starves live HLS

**Symptom:** first `GET /api/live/{ch}.m3u8` after boot returns 404 and
"live looks broken."

**Root cause:** `epg.Refresher.Run` does an immediate first pass on
startup (then on `epg_cron`). It spawns `dvbr epg --schedule ...` which
takes the flock on `/tmp/dvbr-adapter0.lock` + opens the DVB device —
**outside `tuner.Pool`**, so the Pool's in-memory mutex doesn't serialize
it. A concurrent live request's own `dvbr tune` then fails with
`DVB adapter 0 is already in use`, exits, feeds empty input to
b25 → ffmpeg ("Invalid data found"), no segment is written, and the
playlist endpoint 404s (ServeFile on a missing m3u8). EIT `--schedule`
scans run for minutes, so the whole window is blocked.

**Fixed by doing both** (see the 2026-07-30 entry at the top): `dvbr epg`
now runs under a `tuner.Pool` reservation so EPG / live / recordings share
one arbiter, *and* EPG is opportunistic — background priority, deferred
first pass, preempted on demand, retried later.

The `epg_channels = []` workaround is no longer needed.
