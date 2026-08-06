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

// The viewer's caption choice, read off the media element rather than held
// as state, because the browser's own captions menu can set it too. Returns
// null when captions are off.
function subtitleChoice(video: HTMLVideoElement) {
  for (const t of captionTracks(video)) {
    if (t.mode === "showing") return { name: t.label, lang: t.language };
  }
  return null;
}

// The tracks a viewer would call subtitles. hls.js turns the manifest's WebVTT
// rendition into one of these (renderTextTracksNatively), so there is normally
// either one or none — none being an ordinary night on a channel that sends no
// captions.
function captionTracks(video: HTMLVideoElement) {
  return Array.from(video.textTracks).filter(
    (t) => t.kind === "subtitles" || t.kind === "captions",
  );
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
//
// Captions are a native TextTrack — hls.js turns the manifest's WebVTT
// rendition into one (renderTextTracksNatively, its default), which is what lets
// the browser draw them inside its own fullscreen video, and what an iPad gets
// from the manifest with nothing of ours running. Turning them *on*, though, is
// the button under the picture: Chrome's control bar has no captions button at
// all (checked against the accessibility tree — pause, fullscreen, mute, and an
// overflow ⋮), so the only browser-provided way in is two levels down that menu,
// which is where live captions went to die. The manifest still says DEFAULT=NO,
// so they start off as they do on a television, and a choice made in the
// browser's menu is picked up here rather than fought with.
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
  // What the viewer last chose, carried across channel changes. Detaching the
  // media resets the track selection, so without this every change of channel
  // would silently turn captions back off — and hls.js needs it as
  // `subtitlePreference` to load the new channel's rendition at all.
  const subsPrefRef = useRef<{ name: string; lang: string } | null>(null);
  // Captions as the control below the picture sees them: `on` is whether a
  // track is showing, `available` whether this stream carries one at all. Never
  // assumed — both are read back from the player by `syncSubs`, since the
  // browser's own captions menu sets the same thing behind us.
  const [subs, setSubs] = useState({ on: false, available: false });

  const destroy = useCallback(() => {
    const v = videoRef.current;
    // Read the caption choice while the player is still attached: hls.js
    // disables every text track as it detaches. Only when there is something
    // to read from — a destroy() on an already-dead player would otherwise
    // record "off" over a real preference.
    if (v && hlsRef.current) subsPrefRef.current = subtitleChoice(v);
    if (hlsRef.current) {
      hlsRef.current.destroy();
      hlsRef.current = null;
    }
    if (v) v.removeAttribute("src");
  }, []);

  // The caption control's state belongs to the player, not to us: hls.js adds
  // the track when it parses a manifest and disables it on detach, and the
  // browser's own captions menu sets the same modes.
  //
  // Whether captions are *available* is the manifest's answer and not the
  // element's, because a TextTrack cannot be removed once it has been added — so
  // after a channel change the last channel's track is still on the element, and
  // going by that would offer captions on a channel sending none. hls.js
  // republishes `subtitleTracks` per manifest, which is the truth; the element is
  // the fallback for iOS Safari, where hls.js does not run and the native player
  // owns the renditions.
  const syncSubs = useCallback(() => {
    const video = videoRef.current;
    if (!video) return;
    const tracks = captionTracks(video);
    const available = hlsRef.current
      ? hlsRef.current.subtitleTracks.length > 0
      : tracks.length > 0;
    setSubs({ on: tracks.some((t) => t.mode === "showing"), available });
  }, []);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const tracks = video.textTracks;
    syncSubs();
    tracks.addEventListener("addtrack", syncSubs);
    tracks.addEventListener("removetrack", syncSubs);
    tracks.addEventListener("change", syncSubs);
    return () => {
      tracks.removeEventListener("addtrack", syncSubs);
      tracks.removeEventListener("removetrack", syncSubs);
      tracks.removeEventListener("change", syncSubs);
    };
  }, [syncSubs]);

  // Setting the mode *is* turning captions on: the browser draws a showing
  // track, and hls.js reads the same change back to start loading the rendition
  // (it is how it maps its own menu selections). Nothing else is needed.
  const showSubs = useCallback((on: boolean) => {
    const video = videoRef.current;
    if (!video) return;
    for (const t of captionTracks(video)) t.mode = on ? "showing" : "disabled";
    // Remembered for the next channel, where hls.js re-selects the rendition by
    // name and language; without it a channel change turns captions back off.
    subsPrefRef.current = on ? subtitleChoice(video) : null;
    setSubs((s) => ({ ...s, on }));
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
      // subtitlePreference re-selects the rendition the viewer had on before
      // the channel changed; with none, the manifest's DEFAULT=NO leaves
      // captions off and the browser's captions menu is how they go on.
      const h = new Hls({ ...HLS_CONFIG, subtitlePreference: subsPrefRef.current ?? undefined });
      hlsRef.current = h;
      h.loadSource(src);
      h.attachMedia(video);
      h.on(Hls.Events.MANIFEST_PARSED, () => {
        playThenUnmute(video);
        // Whether this channel is sending captions is in the manifest just
        // parsed, and nothing on the element changes to say so.
        syncSubs();
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
  }, [src, reload, destroy, syncSubs]);

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
    <div className="flex flex-col gap-2">
      {/* 16:9, but capped so the programme title and the record button stay
          above the fold on a laptop. Past the cap the box is wider than the
          picture and the video letterboxes inside it — invisibly, the bars
          being the same black. */}
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
              <p className="mt-1 text-xs text-dim">
                {fatal ?? error ?? "The stream did not open."}
              </p>
            </div>
            {!fatal && (
              <button onClick={() => setReload((n) => n + 1)} className="btn">
                Retry
              </button>
            )}
          </div>
        )}
      </div>

      {/* Under the picture, not over it: an overlay control is on screen
          whether or not the native controls are, and this is the row the
          Recordings player puts the same choice in. `z-10` because the page
          lays its "▶ Watch" overlay across the whole player while idle. */}
      <div className="relative z-10 flex items-center gap-1.5">
        <span className="eyebrow">captions</span>
        <div className="flex overflow-hidden rounded-md border border-line">
          {[
            { on: true, label: "On" },
            { on: false, label: "Off" },
          ].map((choice) => (
            <button
              key={choice.label}
              onClick={() => showSubs(choice.on)}
              disabled={!subs.available}
              title={
                subs.available
                  ? "The broadcast's own subtitles, drawn by the browser"
                  : "This stream is carrying no subtitle track"
              }
              className={`cursor-pointer px-2 py-1 text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-30 ${
                subs.on === choice.on ? "bg-fg text-canvas" : "bg-panel text-dim hover:bg-raised"
              }`}
            >
              {choice.label}
            </button>
          ))}
        </div>
      </div>
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
