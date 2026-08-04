"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import Hls from "hls.js";

export type PlayerStatus = "idle" | "loading" | "playing" | "error";

export type VideoPlayerProps = {
  src: string | null;
  // Shown over a dead player instead of a spinner: on a one-tuner box a
  // channel change can legitimately fail (a recording holds the adapter),
  // and the reason comes from the switch call, not from hls.js.
  fatal?: string | null;
  onPrev?: () => void;
  onNext?: () => void;
};

// Start playback muted (always allowed), then unmute — but only when
// the user has interacted with the page at least once. Unmuting with
// no prior interaction makes Chrome pause the element, and (observed
// in Chromium) can hard-fail the whole media pipeline with
// DEMUXER_ERROR_COULD_NOT_PARSE, leaving a dead black player. So the
// auto-tuned channel on a fresh page stays muted; any click (choosing
// a channel, the volume control) grants sticky activation and sound.
function playThenUnmute(video: HTMLVideoElement) {
  video
    .play()
    .then(() => {
      if (navigator.userActivation?.hasBeenActive) {
        video.muted = false;
      }
    })
    .catch(() => {});
}

// A cold tune holds the manifest request open server-side for up to
// ~30s (frontend lock timeout + A/V probe + first segment) before the
// playlist exists. hls.js defaults give up after 10s, which aborts the
// tune and can never converge — so budget a full tune for the first
// byte and keep retrying errors while the tuner warms up.
const HLS_CONFIG = {
  enableWorker: true,
  lowLatencyMode: true,
  manifestLoadPolicy: {
    default: {
      maxTimeToFirstByteMs: 60000,
      maxLoadTimeMs: 90000,
      timeoutRetry: { maxNumRetry: 2, retryDelayMs: 1000, maxRetryDelayMs: 4000 },
      errorRetry: { maxNumRetry: 8, retryDelayMs: 2000, maxRetryDelayMs: 8000 },
    },
  },
  playlistLoadPolicy: {
    default: {
      maxTimeToFirstByteMs: 30000,
      maxLoadTimeMs: 45000,
      timeoutRetry: { maxNumRetry: 2, retryDelayMs: 1000, maxRetryDelayMs: 4000 },
      errorRetry: { maxNumRetry: 8, retryDelayMs: 2000, maxRetryDelayMs: 8000 },
    },
  },
};

// Live HLS player. Wraps <video> + hls.js and surfaces a coarse status
// so it can render an overlay (spinner / error + retry). Fatal hls.js
// errors get up to 3 automatic recoveries (recoverMediaError for media
// errors, startLoad for network errors); after that the overlay shows a
// manual retry button.
//
// Autoplay policy: start muted (browser-compliant), unmute after the
// first play() resolves. enableWorker=true offloads remuxing from the
// main thread (the previous `false` caused stutter on high-bitrate CS).
export function VideoPlayer({ src, fatal, onPrev, onNext }: VideoPlayerProps) {
  const boxRef = useRef<HTMLDivElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const hlsRef = useRef<Hls | null>(null);
  const recoveriesRef = useRef(0);
  const [status, setStatus] = useState<PlayerStatus>("idle");
  const [error, setError] = useState<string | null>(null);
  // Bumped by the retry button. Attaching hls.js lives in exactly one
  // place — the effect below — and retry re-runs it. It used to be a
  // second copy of the same setup, which had already drifted: the copy
  // never installed the recovery path, so the first hiccup after a
  // manual retry was fatal.
  const [reload, setReload] = useState(0);

  const destroy = useCallback(() => {
    if (hlsRef.current) {
      hlsRef.current.destroy();
      hlsRef.current = null;
    }
    const v = videoRef.current;
    if (v) v.removeAttribute("src");
  }, []);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    if (!src) {
      destroy();
      setStatus("idle");
      setError(null);
      return;
    }

    setStatus("loading");
    setError(null);
    recoveriesRef.current = 0;
    destroy();

    const onPlaying = () => setStatus("playing");
    const onWaiting = () => setStatus("loading");
    video.addEventListener("playing", onPlaying);
    video.addEventListener("waiting", onWaiting);
    video.addEventListener("loadstart", onWaiting);

    // Prefer hls.js over the native path: recent Chromium ships a
    // half-working built-in HLS player that makes canPlayType return
    // "maybe" but then dies with DEMUXER_ERROR_COULD_NOT_PARSE on our
    // streams. Native HLS is only for browsers without MSE (iOS
    // Safari), where hls.js is unsupported.
    if (Hls.isSupported()) {
      const h = new Hls(HLS_CONFIG);
      hlsRef.current = h;
      h.loadSource(src);
      h.attachMedia(video);
      h.on(Hls.Events.MANIFEST_PARSED, () => playThenUnmute(video));
      h.on(Hls.Events.ERROR, (_evt, data) => {
        if (!data.fatal) return;
        // Cold tunes start mid-GOP: the first segment can carry
        // audio-only/partial video that Chrome's demuxer rejects as a
        // fatal MEDIA_ERROR even though the stream is fine seconds
        // later. recoverMediaError() rebuilds MediaSource and resumes
        // from the live edge — startLoad() would NOT clear the media
        // element's error state and the player would stay black.
        if (recoveriesRef.current < 3) {
          recoveriesRef.current++;
          try {
            if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
              h.recoverMediaError();
              video.play().catch(() => {});
            } else {
              h.startLoad();
            }
            return;
          } catch {
            /* fall through to error */
          }
        }
        setStatus("error");
        setError(describeHlsError(data));
      });
    } else {
      // No MSE (iOS Safari): native HLS.
      video.src = src;
      playThenUnmute(video);
    }

    return () => {
      video.removeEventListener("playing", onPlaying);
      video.removeEventListener("waiting", onWaiting);
      video.removeEventListener("loadstart", onWaiting);
      destroy();
    };
  }, [src, reload, destroy]);

  // Fullscreen the container rather than the <video>, so the overlays
  // ("Tuning…", an error) are still visible in fullscreen.
  const fullscreen = useCallback(() => {
    if (document.fullscreenElement) document.exitFullscreen();
    else boxRef.current?.requestFullscreen?.();
  }, []);

  // Keyboard shortcuts, live only while the player has focus — arrow keys
  // change channel, and stealing those from the rest of the page would
  // make the channel list unusable.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const onKey = (e: KeyboardEvent) => {
      switch (e.key) {
        case " ":
        case "k":
          e.preventDefault();
          if (v.paused) v.play().catch(() => {});
          else v.pause();
          break;
        case "ArrowLeft":
          onPrev?.();
          break;
        case "ArrowRight":
          onNext?.();
          break;
        case "f":
          fullscreen();
          break;
        case "m":
          v.muted = !v.muted;
          break;
      }
    };
    v.addEventListener("keydown", onKey);
    return () => v.removeEventListener("keydown", onKey);
  }, [onPrev, onNext, fullscreen]);

  const shown = fatal ? "error" : status;

  return (
    // 16:9, but capped so the programme title and the record button stay
    // above the fold on a laptop. Past the cap the box is wider than the
    // picture and the video letterboxes inside it — invisibly, the bars
    // being the same black.
    <div
      ref={boxRef}
      className="relative aspect-video max-h-[70vh] overflow-hidden rounded-lg bg-black"
    >
      <video
        ref={videoRef}
        // No source means no timeline to scrub and no volume to set: with
        // `controls` always on, an idle player showed a native control bar
        // reading 0:00 under the Watch button.
        controls={Boolean(src)}
        playsInline
        muted
        tabIndex={0}
        className="h-full w-full"
      />

      {shown === "loading" && src && (
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-2">
          <div className="h-6 w-6 animate-spin rounded-full border border-white/25 border-t-white" />
          <span className="font-mono text-[11px] text-white/70">Tuning…</span>
        </div>
      )}

      {shown === "error" && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black/80 px-6 text-center">
          <div>
            <p className="text-sm font-medium text-rec">Cannot play this channel</p>
            <p className="mt-1 text-xs text-dim">{fatal ?? error ?? "The stream did not open."}</p>
          </div>
          {!fatal && (
            <button onClick={() => setReload((n) => n + 1)} className="btn">
              Retry
            </button>
          )}
        </div>
      )}
    </div>
  );
}

function describeHlsError(data: { type?: string; details?: string }): string {
  const t = data.type ?? "";
  const d = data.details ?? "";
  if (t === Hls.ErrorTypes.NETWORK_ERROR) return `Network error (${d})`;
  if (t === Hls.ErrorTypes.MEDIA_ERROR) return `Media error (${d})`;
  return `${t} ${d}`.trim();
}
