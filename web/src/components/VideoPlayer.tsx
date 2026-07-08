"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import Hls from "hls.js";

export type PlayerStatus = "idle" | "loading" | "playing" | "error";

export type VideoPlayerProps = {
  src: string | null;
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
// so the parent can render an overlay (spinner / error + retry). Fatal
// hls.js errors get up to 3 automatic recoveries (recoverMediaError for
// media errors, startLoad for network errors); after that the overlay
// shows a manual retry button.
//
// Autoplay policy: start muted (browser-compliant), unmute after the
// first play() resolves. enableWorker=true offloads remuxing from the
// main thread (the previous `false` caused stutter on high-bitrate CS).
export function VideoPlayer({ src, onPrev, onNext }: VideoPlayerProps) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const hlsRef = useRef<Hls | null>(null);
  const recoveriesRef = useRef(0);
  const [status, setStatus] = useState<PlayerStatus>("idle");
  const [error, setError] = useState<string | null>(null);

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
      h.on(Hls.Events.MANIFEST_PARSED, () => {
        playThenUnmute(video);
      });
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
  }, [src, destroy]);

  const retry = useCallback(() => {
    // Force the effect to re-run by toggling status; the src prop is
    // unchanged, so we re-trigger via a state-driven remount pattern.
    setStatus("loading");
    setError(null);
    recoveriesRef.current = 0;
    const v = videoRef.current;
    if (!v || !src) return;
    if (hlsRef.current) {
      hlsRef.current.destroy();
      hlsRef.current = null;
    }
    if (!Hls.isSupported()) {
      v.src = src;
      playThenUnmute(v);
    } else {
      const h = new Hls(HLS_CONFIG);
      hlsRef.current = h;
      h.loadSource(src);
      h.attachMedia(v);
      h.on(Hls.Events.MANIFEST_PARSED, () => playThenUnmute(v));
      h.on(Hls.Events.ERROR, (_e, data) => {
        if (!data.fatal) return;
        setStatus("error");
        setError(describeHlsError(data));
      });
    }
  }, [src]);

  // Keyboard shortcuts (only when this player region is focused/hovered).
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
          if (onPrev) onPrev();
          break;
        case "ArrowRight":
          if (onNext) onNext();
          break;
        case "f":
          if (document.fullscreenElement) document.exitFullscreen();
          else v.requestFullscreen?.();
          break;
        case "m":
          v.muted = !v.muted;
          break;
      }
    };
    v.addEventListener("keydown", onKey);
    return () => v.removeEventListener("keydown", onKey);
  }, [onPrev, onNext]);

  const fullscreen = useCallback(() => {
    const v = videoRef.current;
    if (!v) return;
    if (document.fullscreenElement) document.exitFullscreen();
    else v.requestFullscreen?.();
  }, []);

  return (
    <div className="relative bg-black rounded-xl overflow-hidden aspect-video group">
      <video
        ref={videoRef}
        controls
        playsInline
        muted
        tabIndex={0}
        className="w-full h-full"
        poster="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 9'><rect fill='%23111827' width='16' height='9'/></svg>"
      />

      {/* Loading overlay */}
      {status === "loading" && src && (
        <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
          <div className="flex flex-col items-center gap-2" style={{ color: "var(--color-text-muted)" }}>
            <div className="w-8 h-8 border-2 border-white/30 border-t-white rounded-full animate-spin" />
            <span className="text-xs">チューニング中…</span>
          </div>
        </div>
      )}

      {/* Error overlay */}
      {status === "error" && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black/70">
          <div className="text-center px-4">
            <div className="text-sm font-medium mb-1" style={{ color: "var(--color-danger)" }}>
              再生エラー
            </div>
            <div className="text-xs" style={{ color: "var(--color-text-muted)" }}>
              {error ?? "ストリームを開けませんでした"}
            </div>
          </div>
          <button onClick={retry} className="btn btn-accent text-xs">
            再試行
          </button>
        </div>
      )}

      {/* Fullscreen button (top-right, fades in on hover) */}
      {src && status !== "error" && (
        <button
          onClick={fullscreen}
          className="absolute top-2 right-2 opacity-0 group-hover:opacity-100 transition-opacity px-2 py-1 rounded-md text-xs"
          style={{ background: "rgba(0,0,0,0.6)", color: "#fff" }}
          title="全画面 (F)"
        >
          ⛶
        </button>
      )}
    </div>
  );
}

function describeHlsError(data: { type?: string; details?: string }): string {
  const t = data.type ?? "";
  const d = data.details ?? "";
  if (t === Hls.ErrorTypes.NETWORK_ERROR) return `ネットワークエラー (${d})`;
  if (t === Hls.ErrorTypes.MEDIA_ERROR) return `メディアエラー (${d})`;
  return `${t} ${d}`.trim();
}
