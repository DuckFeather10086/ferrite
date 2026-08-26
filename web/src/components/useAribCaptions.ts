"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  AribCueStore,
  CAPTION_FONT,
  drawCaption,
  isAribSegment,
  pictureRect,
  resetFontMetrics,
} from "@/lib/aribCaption";

// The live ARIB caption overlay: fetch the structured cues beside the video
// segments, put them on the player's clock, and draw them over the picture.
//
// # Putting broadcast PTS on the player's timeline
//
// A cue's time is the PTS of the transport stream it was decoded from — hours
// into the broadcast day — and `video.currentTime` starts near zero. Reconciling
// the two is the job `X-TIMESTAMP-MAP` does for the WebVTT rendition, and it
// does it on the server's side: only the server can measure which PTS window a
// segment covers, and it has to, because it is writing cue times a player will
// then place on its own with no chance to correct them.
//
// Here it is free. `sub{N}.json` states the window it was cut to, hls.js states
// where fragment N sits on the media timeline, and the difference between them
// is a constant this holds. One subtraction converts every cue, and nothing
// downstream has to re-time anything.
//
// # Which file to fetch
//
// The rendition is not in any playlist and no player asks for it: `sub{N}.json`
// is named after the *video* segment's own sequence number, and hls.js hands
// that same number back as `frag.sn`. So the browser already knows which file it
// wants, and asks for it beside the fragment it just loaded.

/** How long to wait before asking again for a sidecar that was not there. */
const RETRY_MS = 700;

/** How many times to ask again. The publisher writes a segment's sidecar on the
 *  first tick after ffmpeg lists the segment, and its tick is half a segment —
 *  so a fragment hls.js loads the instant it appears can be up to a whole tick
 *  ahead of its own captions. Two retries put the last attempt at 1.4s, which
 *  covers that; one put it at 0.7s, which covered it only if the tick fell the
 *  right way. The cost of being wrong is not a late caption but a missing one:
 *  the sidecar is fetched once per fragment and never revisited. */
const RETRIES = 2;

/** What this needs of an hls.js fragment. Typed structurally so the hook does
 *  not drag hls.js's types into a module that is otherwise about canvas. */
export type LoadedFragment = {
  sn: number | "initSegment";
  /** Seconds on the media timeline — the same clock as `video.currentTime`. */
  start: number;
  url: string;
  type: string;
};

export type AribCaptions = {
  /** Attach to a canvas laid over the video, inside the same positioned box. */
  canvasRef: React.RefObject<HTMLCanvasElement | null>;
  /** Feed every hls.js FRAG_LOADED; non-video fragments are ignored. */
  onFragment: (frag: LoadedFragment) => void;
  /** Forget everything — a channel change, or the player being torn down. */
  reset: () => void;
};

/**
 * @param videoRef the element the captions are drawn over
 * @param active whether to fetch and draw at all
 */
export function useAribCaptions(
  videoRef: React.RefObject<HTMLVideoElement | null>,
  active: boolean,
): AribCaptions {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const storeRef = useRef(new AribCueStore());
  // PTS seconds minus media-timeline seconds. Set from the first segment
  // document that arrives and refreshed by every one after it, which also
  // absorbs a discontinuity rather than needing to detect one.
  const offsetRef = useRef<number | null>(null);
  const fetchedRef = useRef(new Set<number>());
  // Bumped when the caption font resolves and the measurements taken against
  // whatever stood in for it are thrown away.
  const [fontEpoch, setFontEpoch] = useState(0);
  // Read inside the fragment callback, which hls.js owns and which must not be
  // re-registered every time the viewer changes caption mode.
  const activeRef = useRef(active);
  useEffect(() => {
    activeRef.current = active;
  }, [active]);

  const reset = useCallback(() => {
    storeRef.current.clear();
    fetchedRef.current.clear();
    offsetRef.current = null;
    const canvas = canvasRef.current;
    const ctx = canvas?.getContext("2d");
    if (canvas && ctx) ctx.clearRect(0, 0, canvas.width, canvas.height);
  }, []);

  const onFragment = useCallback((frag: LoadedFragment) => {
    // Only the video fragments: the subtitle and audio renditions have their
    // own numbering, and `frag.start` for a subtitle fragment is the same
    // number by a different route — but nothing guarantees that, and an init
    // segment has no sequence number at all.
    if (frag.type !== "main" || typeof frag.sn !== "number") return;
    if (!activeRef.current) return;
    const sn = frag.sn;
    if (fetchedRef.current.has(sn)) return;
    fetchedRef.current.add(sn);
    // A rolling window; the set would otherwise grow for as long as the
    // channel is watched.
    if (fetchedRef.current.size > 256) {
      for (const old of fetchedRef.current) {
        if (old < sn - 128) fetchedRef.current.delete(old);
      }
    }

    let url: string;
    try {
      const u = new URL(frag.url, window.location.href);
      u.pathname = u.pathname.replace(/[^/]*$/, `sub${sn}.json`);
      url = u.toString();
    } catch {
      return;
    }
    const start = frag.start;
    // Once, then RETRIES times more. A 404 is ordinary — a channel sending no
    // captions, or a session whose first rendition has not been published — but
    // it is also what the *newest* segment answers for the moment between
    // hls.js seeing it in the playlist and the caption publisher's next tick
    // writing the sidecar. (It is no longer what an *old* segment answers:
    // internal/caption keeps a segment past the window for exactly that
    // reason — see pruneGrace.) Missing a segment is not harmless: an open
    // cue's end only advances when a window says it is still up, so a hole
    // leaves the caption disappearing for the length of it.
    const load = (): Promise<boolean> =>
      fetch(url)
        .then((r) => (r.ok ? r.json() : null))
        .then((doc: unknown) => {
          if (!isAribSegment(doc)) return false;
          offsetRef.current = doc.start_ms / 1000 - start;
          for (const cue of doc.cues) storeRef.current.add(cue);
          return true;
        })
        .catch(() => false);

    const retry = (left: number) => {
      if (left <= 0) return;
      setTimeout(() => void load().then((ok) => !ok && retry(left - 1)), RETRY_MS);
    };
    void load().then((ok) => !ok && retry(RETRIES));
  }, []);

  // Drawing. Driven off the video's own frames where the browser offers them —
  // `requestVideoFrameCallback` fires with the frame that is being presented,
  // which is the only clock that stays right while seeking or paused — and off
  // the animation frame where it does not.
  useEffect(() => {
    const video = videoRef.current;
    const canvas = canvasRef.current;
    if (!video || !canvas) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    if (!active) {
      canvas.style.display = "none";
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      return;
    }
    canvas.style.display = "";

    let stop = false;
    // What is on the canvas, as the *caption* rather than the object holding
    // it. Every segment the caption is still up in delivers a fresh cue with a
    // later end, so comparing objects would clear and redraw the identical
    // picture once a second for as long as it stays on screen.
    let drawn: number | null = null;
    let fitted = "";

    // Lay the canvas over the picture, not over the element. The two differ
    // whenever the video letterboxes inside its box, which is most of the time
    // here: the player caps its height at 70vh, so past the cap the box is
    // wider than the 16:9 picture in it and the bars are the same black as
    // everything else. Laying the plane across them is the bug the recordings
    // player shipped with — every caption ~120px left of where it belonged.
    const fit = () => {
      const rect = pictureRect(video);
      if (!rect) return null;
      const dpr = window.devicePixelRatio || 1;
      const key = `${rect.left}:${rect.top}:${rect.width}:${rect.height}:${dpr}`;
      if (key !== fitted) {
        fitted = key;
        canvas.style.left = `${rect.left}px`;
        canvas.style.top = `${rect.top}px`;
        canvas.style.width = `${rect.width}px`;
        canvas.style.height = `${rect.height}px`;
        canvas.width = Math.max(1, Math.round(rect.width * dpr));
        canvas.height = Math.max(1, Math.round(rect.height * dpr));
        drawn = null; // the backing store was resized, so it is blank again
      }
      return rect;
    };

    const paint = () => {
      const rect = fit();
      const offset = offsetRef.current;
      if (!rect || offset === null) return;
      const ptsMs = (video.currentTime + offset) * 1000;
      const cue = storeRef.current.at(ptsMs);
      const showing = cue ? cue.start_ms : null;
      if (showing === drawn) return;
      drawn = showing;
      storeRef.current.prune(ptsMs);

      const dpr = window.devicePixelRatio || 1;
      ctx.setTransform(1, 0, 0, 1, 0, 0);
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      if (!cue?.caption) return;
      const { plane_width: pw, plane_height: ph } = cue.caption;
      if (pw <= 0 || ph <= 0) return;
      // One unit is one caption-plane unit from here on, so every coordinate in
      // the model goes straight through — the same trick PlayResX/PlayResY plays
      // for the `.ass`.
      ctx.setTransform((rect.width * dpr) / pw, 0, 0, (rect.height * dpr) / ph, 0, 0);
      drawCaption(ctx, cue.caption);
    };

    // rVFC is per presented frame, which is also per *seek* and per pause —
    // rAF keeps running when the video does not, and stops when the tab is
    // hidden, which is the right behaviour either way.
    const rvfc = (
      video as HTMLVideoElement & {
        requestVideoFrameCallback?: (cb: () => void) => number;
      }
    ).requestVideoFrameCallback?.bind(video);

    const tick = () => {
      if (stop) return;
      paint();
      if (rvfc) rvfc(tick);
      else requestAnimationFrame(tick);
    };
    tick();

    // A frame callback only fires while frames are being presented; a resize
    // of a paused player has to repaint on its own.
    const observer = new ResizeObserver(() => paint());
    observer.observe(video);
    video.addEventListener("loadedmetadata", paint);

    return () => {
      stop = true;
      observer.disconnect();
      video.removeEventListener("loadedmetadata", paint);
      ctx.setTransform(1, 0, 0, 1, 0, 0);
      ctx.clearRect(0, 0, canvas.width, canvas.height);
    };
  }, [active, videoRef, fontEpoch]);

  // Every cell's size and baseline come out of the font's own metrics, so they
  // are wrong until the font the caption names has resolved — and they are
  // cached, which is the same trap ASS.js sets by measuring per family name.
  // Throwing the cache away when the font lands is cheaper than holding the
  // first caption back for it: bumping the epoch also re-runs the loop above,
  // so whatever is on screen is redrawn at the right size.
  useEffect(() => {
    let live = true;
    void document.fonts
      .load(`36px ${CAPTION_FONT}`)
      .then(() => {
        if (!live) return;
        resetFontMetrics();
        setFontEpoch((n) => n + 1);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  // Stable, because the player registers `onFragment` on the hls.js instance it
  // builds in an effect: an identity that changed per render would tear that
  // instance down and re-tune the channel on every keystroke elsewhere.
  return useMemo(() => ({ canvasRef, onFragment, reset }), [onFragment, reset]);
}
