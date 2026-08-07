# Live captions with ARIB placement — done, except the subtraction

Live captions used to go through WebVTT only, which keeps the words and throws
away everything else the broadcast sent: colours, background boxes, stroke,
enclosure, ruby, and the DRCS glyphs a `.vtt` can only spell `〓`. A recording
already got all of it (ASS.js drawing the `.ass`); the live page was the residue.

That gap is closed. The Live page carries `ARIB | Text | Off`, and ARIB draws the
broadcast's own caption plane on a canvas over the picture. What is left of the
original plan is the *second* thing it was supposed to buy — see "Not done"
below — and it is now blocked on something the plan did not account for.

## What shipped

1. **A structured cue form out of the decoder.** `arib-caption cues --regions`
   attaches the whole `model::Caption` to each cue line: `regions[].chars[]` with
   place, size, spacing, scale, colours, style, enclosure and ruby, the declared
   plane, and the DRCS bitmaps packed as transmitted and base64'd.
   `libaribcaption-rs/src/render/json.rs`, which renders nothing — it serialises
   the model and leaves every decision about pixels to whoever reads it. Times
   stay raw broadcast PTS. Defaults are omitted, which roughly halves a segment.

   One decode, not two: the flag rides on `cues` rather than being its own
   command, because a second child reading a second copy of the same TS would
   produce the same captions twice. The two forms are matched by `start_ms`
   through a small ring of recent captions rather than a second `Timeline`, which
   is what guarantees they never describe different captions.

2. **Published beside the VTT.** `internal/caption` writes `sub{N}.json` next to
   `sub{N}.vtt`, same window selection, pruned in the same sweep. The `.vtt`
   path is byte-identical to what it was.

   The plan said this form needed neither of the WebVTT segment's two
   adjustments. Half right, and the wrong half was shipped first. The
   **clamping** does fall away — the overlay keys cues by `start_ms`, so a
   caption arriving in five segments is one caption rather than five, and cutting
   its start would make it five. The **open-cue extension** does not: it is the
   only statement the publisher makes that a caption is *still on screen*, since
   a caption's real end arrives with the next one and until then all there is is
   a five-second guess. Without it the segments went silent past that guess and
   the browser had to invent a lifetime to cover the hole — which is exactly how
   captions ended up outliving the broadcast. So an open cue's end is the end of
   the window it is written into, in both renditions, and they agree to the
   millisecond.

3. **PTS → timeline in the browser.** On `FRAG_LOADED`, `sub{frag.sn}.json` is
   fetched beside the fragment and the offset comes from the window it states
   against `frag.start`. Measured at 99.4% agreement with the WebVTT rendition's
   own placement over 45s.

4. **Drawn on a canvas.** `web/src/lib/aribCaption.ts`. The plane is fitted to
   the *picture* — checked against a letterboxed one, 0.19px — and visibility is
   driven off `requestVideoFrameCallback`.

5. **Two forms, one at a time.** `effective: arib | vtt`, with the fall back to
   the native text track while the bare video is fullscreen, ported from the
   Recordings player.

Verified against libass on a real recording and on the air; the numbers are in
CLAUDE.md under the 2026-08-06 note.

## Not done: the ffprobe is still there

The plan's other half was subtraction. The ffprobe anchor exists so the *server*
can know which PTS window each segment covers, which it needs only because a
WebVTT rendition makes it write cue times a player will place on its own. That
mapping is free in the browser, and four mechanisms were supposed to go with it:
`anchor`/`reanchorEvery` and its ffprobe per five segments, `X-TIMESTAMP-MAP`,
the per-segment cue clamping, and the open-cue re-extension across segments.

None of them went. The JSON rendition takes the *documented fallback* — the
server's measured window shipped as a field — rather than recovering the offset
from `INIT_PTS_FOUND` and `frag.elementaryStreams.video.startPTS`, and it does so
because the subtraction is not available either way: **the WebVTT rendition
stays**, and it needs the anchor. It is what Safari and an iPad get from the
manifest with nothing of ours running, and what the browser draws inside its own
fullscreen video. So the four mechanisms have exactly one remaining reason to
exist, and it is a good one.

Two of the four *are* gone from the new path, which is worth saying: the JSON
segments carry no clamping and no open-cue extension. Only the anchor and the
`X-TIMESTAMP-MAP` it feeds are shared, and they are shared with a rendition that
is not going anywhere. Reopen this only if the VTT rendition is ever dropped.

## Do not break

- **Safari/iPad get captions from the manifest with nothing of ours running.**
  That is why the VTT rendition stays announced in the master playlist.
- **`-copyts` stays.** Drop it and the cue PTS and the video PTS stop being the
  same clock.
- **`internal/caption` and `libaribcaption-rs/src/render/vtt.rs` must keep
  agreeing on `line:94%`**, or a channel's captions move when you record it.

## Not this

Carrying the caption PID through ffmpeg and demuxing TS in the browser
(WASM `libaribcaption-rs`) is the pure version — caption and picture in the
same fragment, no sidecar at all. It needs `-c:s copy` to survive the mpegts
muxer with the PES intact, plus a wasm build and a TS demuxer in JS, and it
saves one HTTP request over what shipped. Revisit only if the sidecar turns out
to be the wrong seam; it has not so far.
