"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import Hls from "hls.js";
import { useAribCaptions } from "./useAribCaptions";

export type PlayerStatus = "idle" | "loading" | "playing" | "error";

// How the captions are drawn, and the same three positions the Recordings
// player has, for the same reasons.
//
//   arib — the broadcast's own placement, colours, background boxes, ruby and
//          DRCS glyphs, drawn on a canvas over the picture from the structured
//          rendition (see useAribCaptions).
//   vtt  — the same words as plain lines at the bottom, drawn by the browser
//          from the WebVTT rendition in the manifest. Worth having because it
//          is a real TextTrack: it is what an iPad gets with nothing of ours
//          running, and the only thing visible inside a bare fullscreen video.
//   off  — as a television starts.
export type CaptionMode = "arib" | "vtt" | "off";

const captionModes: {
  key: CaptionMode;
  label: string;
  title: string;
  unavailable: string;
}[] = [
  {
    key: "arib",
    label: "ARIB",
    title: "Captions where the broadcast put them, in the colours it sent",
    // Not "unsupported": on iOS Safari hls.js does not run, the native player
    // owns the renditions, and there are no fragment events to hang this on.
    unavailable: "This player draws the broadcast's own captions only where hls.js runs",
  },
  {
    key: "vtt",
    label: "Text",
    title: "The same captions as plain lines, drawn by the browser",
    unavailable: "This stream is carrying no subtitle track",
  },
  { key: "off", label: "Off", title: "No captions", unavailable: "" },
];

// How long the controls stay up after the pointer stops moving. Long enough to
// cross the picture and click one, short enough that a black gradient is not
// sitting over the top of the programme.
const CONTROLS_IDLE_MS = 2_500;

// The same, for a finger. Longer, because there is no pointer sitting on the
// picture to keep the bar awake: a touch wakes it once and then the clock runs
// whatever the viewer is doing, so 2.5s is barely enough to read the row and
// not enough to decide.
const TOUCH_IDLE_MS = 5_000;

// A run of choices, one of them on. Everything the player itself can be set to
// lives in one of these and looks the same — they are the same kind of choice,
// and a viewer should not have to learn two shapes.
function OverlaySegmented({ children }: { children: React.ReactNode }) {
  return <div className="flex overflow-hidden rounded-md border border-white/25">{children}</div>;
}

// A button that opens something rather than setting something: the settings
// menu and fullscreen. Bordered on its own instead of butted against a
// neighbour, which is how it reads as a verb and not as one of three positions.
function OverlayChip({
  on,
  title,
  onClick,
  children,
}: {
  on?: boolean;
  title: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      title={title}
      className={`cursor-pointer rounded-md border border-white/25 px-2 py-1 text-[11px] transition-colors pointer-coarse:px-3 pointer-coarse:py-2 ${
        on ? "bg-white text-black" : "bg-black/40 text-white/80 hover:bg-white/20"
      }`}
    >
      {children}
    </button>
  );
}

// Light-on-dark rather than the page's own palette: this sits over whatever the
// broadcast is showing, so it cannot borrow a background from the theme.
function OverlayButton({
  on,
  disabled,
  title,
  disabledTitle,
  onClick,
  children,
}: {
  on: boolean;
  disabled?: boolean;
  title: string;
  disabledTitle?: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={disabled ? (disabledTitle ?? title) : title}
      // Roomier where the pointer is a finger: an 11px label with 4px of
      // padding is a comfortable mouse target and a coin-toss for a thumb, and
      // these sit next to each other in a row.
      className={`cursor-pointer px-2 py-1 text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-30 pointer-coarse:px-3 pointer-coarse:py-2 ${
        on ? "bg-white text-black" : "bg-black/40 text-white/80 hover:bg-white/20"
      }`}
    >
      {children}
    </button>
  );
}

export type VideoPlayerProps = {
  src: string | null;
  // Shown over a dead player instead of a spinner: on a one-tuner box a
  // channel change can legitimately fail (a recording holds the adapter),
  // and the reason comes from the switch call, not from hls.js.
  fatal?: string | null;
  onPrev?: () => void;
  onNext?: () => void;
  // The live quality tiers the daemon offers, the one in use, and how to
  // ask for another. Fewer than two means there is no choice to offer and
  // the control is not drawn.
  qualities?: { name: string; label: string }[];
  quality?: string | null;
  onQuality?: (name: string) => void;
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

// Which rendition the viewer has selected, in the form hls.js re-selects one
// by after a channel change. Any mode but `disabled` counts: in ARIB mode the
// text track is kept `hidden`, which loads the rendition without drawing it —
// hls.js reads a hidden track as the selected one (subtitle-track-controller's
// onTextTracksChanged), and it is what makes the fullscreen fallback below
// instant instead of a segment late.
function subtitleChoice(video: HTMLVideoElement) {
  for (const t of captionTracks(video)) {
    if (t.mode !== "disabled") return { name: t.label, lang: t.language };
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
  // Nothing today: lowLatencyMode only does anything for a playlist carrying
  // EXT-X-PART, and ffmpeg is not producing partial segments. Kept for the
  // day LL-HLS is worth its compatibility cost; what actually shortens the
  // live edge is the line below.
  lowLatencyMode: true,
  // How far back from the live edge to start playing, in *segments* — so it is
  // half the latency budget and the daemon's segment length is the other half.
  // hls.js defaults to 3, which at the daemon's 2s segments would be 6s of
  // buffer the viewer is watching from behind. Two is the least that still
  // absorbs one late segment, and 2 × 2s is where the live edge sits.
  liveSyncDurationCount: 2,
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
// Captions come in two forms and exactly one is drawn at a time. The text form
// is a native TextTrack — hls.js turns the manifest's WebVTT rendition into one
// (renderTextTracksNatively, its default), which is what lets the browser draw
// it inside its own fullscreen video, and what an iPad gets from the manifest
// with nothing of ours running. The ARIB form is the broadcast's own caption
// plane on a canvas over the picture, from the structured rendition beside the
// video segments; it keeps the colours, the background boxes, the ruby and the
// DRCS glyphs a `.vtt` can only spell 〓.
//
// Turning either *on* is the control under the picture: Chrome's control bar has
// no captions button at all (checked against the accessibility tree — pause,
// fullscreen, mute, and an overflow ⋮), so the only browser-provided way in is
// two levels down that menu, which is where live captions went to die. The
// manifest still says DEFAULT=NO, so they start off as they do on a television,
// and a choice made in the browser's menu is picked up here rather than fought
// with.
export function VideoPlayer({
  src,
  fatal,
  onPrev,
  onNext,
  qualities,
  quality,
  onQuality,
}: VideoPlayerProps) {
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
  // What the viewer picked, and whether this stream carries captions at all.
  // Availability is never assumed — it is read back from the player, since a
  // channel that sends none must not be offered a button that turns nothing on.
  const [mode, setMode] = useState<CaptionMode>("off");
  const [available, setAvailable] = useState(false);
  // The same choice, readable from the attach effect without making a change of
  // caption mode tear the player down and re-tune the channel.
  const modeRef = useRef(mode);
  useEffect(() => {
    modeRef.current = mode;
  }, [mode]);

  // True while the <video> *alone* is the fullscreen element, which is what the
  // player's own fullscreen button does. The canvas is a sibling of the video,
  // so in that state it stays on the page behind the fullscreen video and the
  // viewer sees no captions at all. The Recordings player learned this the hard
  // way and the findings port straight over: `controlsList="nofullscreen"` does
  // not remove the button (Chrome draws it either way), and re-targeting
  // fullscreen to the container from this handler is a permissions error,
  // because the UA's own request has already consumed the click's transient
  // activation.
  const [videoIsFullscreen, setVideoIsFullscreen] = useState(false);
  useEffect(() => {
    const onChange = () => setVideoIsFullscreen(document.fullscreenElement === videoRef.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);

  // What is actually drawn, as against what the viewer picked. The two differ in
  // exactly one case: an ARIB overlay cannot be shown over a bare fullscreen
  // video, so while it is in that state the text track stands in — a native
  // TextTrack is drawn by the browser *inside* the fullscreen video, which is
  // the one mechanism the platform offers here. The control keeps highlighting
  // the choice: this is a rendering fallback, not a change of mind, and it
  // reverses itself on the way out.
  const effective: CaptionMode = mode === "arib" && videoIsFullscreen ? "vtt" : mode;

  const arib = useAribCaptions(videoRef, effective === "arib");

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
    // Cue times are the old tune's broadcast PTS and the offset that put them
    // on the timeline was measured against its segments.
    arib.reset();
  }, [arib]);

  // Whether captions are available is the manifest's answer and not the
  // element's, because a TextTrack cannot be removed once it has been added — so
  // after a channel change the last channel's track is still on the element, and
  // going by that would offer captions on a channel sending none. hls.js
  // republishes `subtitleTracks` per manifest, which is the truth; the element is
  // the fallback for iOS Safari, where hls.js does not run and the native player
  // owns the renditions.
  const syncAvailable = useCallback(() => {
    const video = videoRef.current;
    if (!video) return;
    setAvailable(
      hlsRef.current ? hlsRef.current.subtitleTracks.length > 0 : captionTracks(video).length > 0,
    );
  }, []);

  // The chosen form, enforced on the element's tracks — and re-enforced when a
  // track arrives, since hls.js adds it a beat after the manifest is parsed.
  //
  // `hidden` rather than `disabled` in ARIB mode: it keeps hls.js loading the
  // WebVTT rendition without the browser drawing it, so the fallback above is a
  // mode flip rather than a wait for the next subtitle fragment.
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const apply = () => {
      // hls.js re-selects the rendition after every channel change and sets the
      // track `showing` when it does — which in ARIB mode would draw the same
      // words a second time, at the bottom, under our own. `subtitleDisplay`
      // is the knob that says "select it, do not display it"; without it the
      // assignment below is overwritten a moment later by the controller.
      if (hlsRef.current) hlsRef.current.subtitleDisplay = effective === "vtt";
      for (const t of captionTracks(video)) {
        t.mode = effective === "vtt" ? "showing" : effective === "arib" ? "hidden" : "disabled";
      }
      // Remembered for the next channel, where hls.js re-selects the rendition
      // by name and language; without it a channel change turns captions off.
      subsPrefRef.current = mode === "off" ? null : subtitleChoice(video);
      syncAvailable();
    };
    apply();
    video.textTracks.addEventListener("addtrack", apply);
    video.textTracks.addEventListener("removetrack", apply);
    return () => {
      video.textTracks.removeEventListener("addtrack", apply);
      video.textTracks.removeEventListener("removetrack", apply);
    };
  }, [effective, mode, syncAvailable]);

  // The browser's own captions menu sets the same thing behind us, and it can
  // only ever mean the text form. Picked up rather than fought with — the same
  // rule as before the ARIB form existed, since a viewer who found the menu is
  // asking for captions and should get them.
  //
  // Only in that direction. A track going `disabled` is what hls.js does to
  // every track as it detaches, so adopting *that* would throw the viewer's
  // choice away on each channel change — which is the bug `subtitlePreference`
  // exists to fix, reintroduced from the other end.
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const onChange = () => {
      if (effective === "vtt") return;
      if (captionTracks(video).some((t) => t.mode === "showing")) setMode("vtt");
    };
    video.textTracks.addEventListener("change", onChange);
    return () => video.textTracks.removeEventListener("change", onChange);
  }, [effective]);

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
      // Set before the first selection, not after: hls.js reads it the moment
      // it picks the rendition, and a frame of the browser's own captions
      // underneath the ARIB overlay is exactly what this stops.
      h.subtitleDisplay = modeRef.current !== "arib";
      h.loadSource(src);
      h.attachMedia(video);
      h.on(Hls.Events.MANIFEST_PARSED, () => {
        playThenUnmute(video);
        // Whether this channel is sending captions is in the manifest just
        // parsed, and nothing on the element changes to say so.
        syncAvailable();
      });
      // The ARIB overlay asks for `sub{frag.sn}.json` beside each video
      // fragment, and takes its PTS→timeline offset from the two together.
      h.on(Hls.Events.FRAG_LOADED, (_evt, data) => arib.onFragment(data.frag));
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
  }, [src, reload, destroy, syncAvailable, arib]);

  // Fullscreen the container rather than the <video>, so everything inside it
  // comes too: the "Tuning…" and error overlays, the controls above, and the
  // ARIB caption canvas — which a fullscreen <video> leaves behind on the page,
  // being a sibling and not a child of it.
  const fullscreen = useCallback(() => {
    if (document.fullscreenElement) document.exitFullscreen();
    else boxRef.current?.requestFullscreen?.();
  }, []);

  // The controls fade out with the pointer, the way the browser's own do — they
  // sit over the picture now, so leaving them up would put a black gradient
  // across the top of every programme.
  //
  // A finger is not a pointer, and taking that literally is what made every
  // setting the player has unreachable on a phone. A tap emits no `pointermove`
  // at all, so nothing ever woke the bar; and a touch pointer ceases to exist
  // when the finger lifts, which fires `pointerleave` — so on the rare tap that
  // did drift enough to count as a move, the bar was hidden again by the end of
  // the same tap. Both handlers are therefore mouse-only, and touch gets the
  // gesture it expects instead: tap the picture to show the controls, tap again
  // to dismiss them.
  const [controlsAwake, setControlsAwake] = useState(false);
  const sleepRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const wake = useCallback((ms: number = CONTROLS_IDLE_MS) => {
    setControlsAwake(true);
    if (sleepRef.current) clearTimeout(sleepRef.current);
    sleepRef.current = setTimeout(() => setControlsAwake(false), ms);
  }, []);
  const sleep = useCallback(() => {
    if (sleepRef.current) clearTimeout(sleepRef.current);
    setControlsAwake(false);
  }, []);
  const touchToggle = useCallback(() => {
    if (controlsAwake) sleep();
    else wake(TOUCH_IDLE_MS);
  }, [controlsAwake, sleep, wake]);
  useEffect(() => () => void (sleepRef.current && clearTimeout(sleepRef.current)), []);

  // The settings menu, and the one thing it needs of the bar: an open menu
  // holds the bar up. The idle clock is still running underneath — it is what
  // makes the bar go away again once the menu is dismissed — but a panel that
  // vanished 2.5s into reading it would make every setting a race.
  const [menuOpen, setMenuOpen] = useState(false);
  const barAwake = controlsAwake || menuOpen;

  // Escape closes it, wherever the focus is. The player's own key handler is
  // on the <video>, which is not what has focus once the ⚙ has been clicked.
  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenuOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [menuOpen]);

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

  // Everything the player offers is inside the picture now, so the player *is*
  // the picture — no row underneath, and the page's own idle overlay lands on
  // the box rather than on the box plus a strip of controls.
  return (
    <>
      {/* 16:9, but capped so the programme title and the record button stay
          above the fold on a laptop. Past the cap the box is wider than the
          picture and the video letterboxes inside it — invisibly, the bars
          being the same black. The cap has to go when this element *is* the
          screen, or fullscreen shows a 70%-tall video on a black field. */}
      <div
        ref={boxRef}
        onPointerMove={(e) => e.pointerType === "mouse" && wake()}
        onPointerLeave={(e) => e.pointerType === "mouse" && sleep()}
        // A press on the picture dismisses the menu, whatever the pointer is.
        // The bar stops propagation on `pointerdown`, so this only ever sees
        // presses *outside* it — which is what dismissal means here, and is
        // why there is no document-level listener.
        onPointerDown={(e) => {
          setMenuOpen(false);
          if (e.pointerType !== "mouse") touchToggle();
        }}
        className="relative aspect-video max-h-[70vh] overflow-hidden rounded-lg bg-black
                   [&:fullscreen]:aspect-auto [&:fullscreen]:h-full [&:fullscreen]:max-h-none [&:fullscreen]:rounded-none"
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

        {/* The ARIB caption plane. Sized and placed onto the *picture* by the
            hook, which is not the same rectangle as this box — the height cap
            above letterboxes the video inside it. Never takes a click: the
            native controls are underneath. */}
        <canvas ref={arib.canvasRef} className="pointer-events-none absolute left-0 top-0" />

        {/* The controls that are ours, over the picture rather than under it.
            At the *top*, because the bottom belongs to the browser's own bar and
            to the captions — and inside this box rather than below it, because
            this box is what our fullscreen button takes, and a control outside
            it is unreachable for as long as the viewer is watching fullscreen.
            They fade with the pointer the way the native ones do. */}
        {src && (
          <div
            // A touch landing *on* the bar must not reach the toggle above it:
            // the bar would be dismissed on `pointerdown`, and a button that has
            // gone `pointer-events-none` never receives the click that follows,
            // so every setting would be one tap from working and never work.
            // Restart the idle clock instead — a viewer partway through the row
            // is not idle.
            onPointerDown={(e) => {
              e.stopPropagation();
              wake(e.pointerType === "mouse" ? CONTROLS_IDLE_MS : TOUCH_IDLE_MS);
            }}
            className={`absolute inset-x-0 top-0 z-20 flex items-start gap-2 bg-gradient-to-b from-black/70 to-transparent px-2 py-1.5 transition-opacity duration-200 ${
              barAwake ? "opacity-100" : "pointer-events-none opacity-0"
            }`}
          >
            {/* Every setting behind one button, and nothing over the picture
                until it is asked for. The row these used to sit in was the
                whole width of the frame and up whenever the pointer moved,
                which is a lot of furniture across the top of a programme for
                two settings a viewer changes once. */}
            <div className="relative">
              <OverlayChip
                on={menuOpen}
                title="Captions and picture quality"
                onClick={() => setMenuOpen((open) => !open)}
              >
                ⚙
              </OverlayChip>

              {menuOpen && (
                // Anchored under the button and inside the box, so it comes
                // along into our own fullscreen. A grid rather than two rows,
                // to line the labels up with each other.
                //
                // `w-max` is load-bearing, and without it the menu is empty.
                // This is absolutely positioned, so its shrink-to-fit width is
                // resolved against its containing block — which is the ⚙ chip's
                // own wrapper, 27px wide — and collapses to min-content. Each
                // row of choices is `overflow-hidden`, which makes it a scroll
                // container, whose min-content contribution is *zero*: the two
                // segmented controls came out 2px wide (their borders, nothing
                // else) and clipped every button away, so the menu opened
                // showing two labels and no choices, with `elementFromPoint`
                // over the buttons answering the <video> underneath. Sizing to
                // max-content takes the containing block out of it.
                <div className="absolute left-0 top-full mt-1.5 grid w-max grid-cols-[auto_auto] items-center gap-x-3 gap-y-2 rounded-md border border-white/20 bg-black/85 px-2.5 py-2 shadow-lg backdrop-blur-sm">
                  {/* One control, three positions — the same one the Recordings
                      player carries, because it is the same choice. Not the
                      browser's own captions menu: in ARIB mode there is no
                      showing TextTrack for it to list, and in Chrome there is
                      no captions button to reach it with anyway. */}
                  <span className="eyebrow text-white/60">captions</span>
                  <OverlaySegmented>
                    {captionModes.map((choice) => (
                      <OverlayButton
                        key={choice.key}
                        on={mode === choice.key}
                        disabled={
                          choice.key !== "off" &&
                          !(available && (choice.key !== "arib" || Hls.isSupported()))
                        }
                        title={choice.title}
                        disabledTitle={choice.unavailable}
                        onClick={() => {
                          setMode(choice.key);
                          setMenuOpen(false);
                        }}
                      >
                        {choice.label}
                      </OverlayButton>
                    ))}
                  </OverlaySegmented>

                  {/* Quality, when there is more than one to pick from.
                      Choosing one starts an encode on the server and reloads
                      the player at it — this is not an ABR switch inside the
                      manifest, and it is not free, which is why it is a
                      deliberate click and not something the player does on its
                      own. */}
                  {qualities && qualities.length > 1 && (
                    <>
                      <span className="eyebrow text-white/60">quality</span>
                      <OverlaySegmented>
                        {qualities.map((q) => (
                          <OverlayButton
                            key={q.name}
                            on={quality === q.name}
                            title={`Re-encode this channel at ${q.label}`}
                            onClick={() => {
                              onQuality?.(q.name);
                              setMenuOpen(false);
                            }}
                          >
                            {q.label}
                          </OverlayButton>
                        ))}
                      </OverlaySegmented>
                    </>
                  )}
                </div>
              )}
            </div>

            <div className="ml-auto">
              {/* Ours takes the whole box, so the ARIB overlay inside it comes
                  too. The player's own fullscreen button takes the bare video
                  and the overlay is left behind on the page — which is why
                  `effective` falls back to the text track there, and why this
                  button is the only way to watch fullscreen with the captions
                  where the broadcast put them. */}
              <OverlayChip
                title="Fullscreen with the ARIB captions — the player's own button cannot show them"
                onClick={fullscreen}
              >
                ⛶ Fullscreen
              </OverlayChip>
            </div>
          </div>
        )}

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
    </>
  );
}

function describeHlsError(data: { type?: string; details?: string }): string {
  const t = data.type ?? "";
  const d = data.details ?? "";
  if (t === Hls.ErrorTypes.NETWORK_ERROR) return `Network error (${d})`;
  if (t === Hls.ErrorTypes.MEDIA_ERROR) return `Media error (${d})`;
  return `${t} ${d}`.trim();
}
