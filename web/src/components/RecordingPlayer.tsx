"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type ASS from "assjs";
import type HlsType from "hls.js";
import {
  fmtDate,
  fmtTime,
  recordingMp4Url,
  recordingPlaylistUrl,
  recordingSubsUrl,
  SOURCE_QUALITY,
  useStatus,
  type Recording,
} from "@/lib/api";

// Where the viewer's picture-quality choice is kept, the same way the Live
// page keeps its own: a property of this screen and this connection (a phone
// on mobile data wants 480p every time), not of the recording or the daemon.
const QUALITY_KEY = "ferrite.recordingQuality";

// A position in the programme, as a viewer reads it off the seek bar.
function clock(seconds: number) {
  const s = Math.max(0, Math.floor(seconds));
  const mm = String(Math.floor(s / 60) % 60).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  const h = Math.floor(s / 3600);
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}

// The font family an ASS script's default style names, which is the one ASS.js
// will measure. Ours always writes the same one, but reading it out of the file
// keeps that a fact about the file rather than an assumption here.
function assFontFamily(script: string) {
  const style = script.match(/^Style:\s*[^,]*,\s*([^,]+)/m);
  return style?.[1]?.trim() || "sans-serif";
}

// How the captions are drawn. ARIB broadcasts place a caption on the
// screen — over the shot it belongs to, clear of the face that is talking —
// and the .ass keeps that placement, which is the reason the post-pass
// writes one. The .vtt is the same words as plain lines at the bottom:
// worth having because it is a real <track>, so the browser draws it itself,
// which is the only thing that works inside a bare fullscreen video.
export type CaptionMode = "ass" | "vtt" | "off";

type Props = {
  rec: Recording;
  // Already resolved through useChannelIndex — a row carries the canonical
  // channel name, which for a legacy record is mojibake.
  channelLabel: string;
  onClose: () => void;
};

// Plays the MP4 the post-pass made, with the captions it made beside it.
//
// The .ass is rendered by ASS.js into a box over the video rather than by the
// browser, since no browser reads ASS. That is a DOM overlay, so it only
// covers *this* element — which is what the fullscreen handling below is
// about, and why the .vtt is not merely a lesser alternative.
export function RecordingPlayer({ rec, channelLabel, onClose }: Props) {
  const boxRef = useRef<HTMLDivElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const assRef = useRef<ASS | null>(null);
  // The .ass itself, since ASS.js takes the content and not a URL. It is
  // also the availability answer for the ARIB button: null means this
  // recording has no .ass, which is ordinary — plenty of programmes carry no
  // captions at all — and not an error to report.
  const [assText, setAssText] = useState<string | null>(null);
  const [have, setHave] = useState<{ ass: boolean; vtt: boolean } | null>(null);
  const [mode, setMode] = useState<CaptionMode>("ass");
  const [error, setError] = useState<string | null>(null);

  // Which picture is being played: the recording's own MP4, or one of the
  // tiers the daemon re-encodes it to on demand. A 1080p 6 Mbit/s file is the
  // right thing on the LAN and unwatchable on a phone over Tailscale, and it
  // is the same choice — and the same encode — the Live page offers.
  const { data: status } = useStatus();
  const tiers = useMemo(() => status?.live_qualities ?? [], [status]);
  const [quality, setQuality] = useState<string>(SOURCE_QUALITY);
  // Where to pick the programme up after a switch. Read off the element at
  // the moment of the click, because the element is about to be reloaded.
  const resumeRef = useRef(0);
  const hlsRef = useRef<HlsType | null>(null);
  // Whether the *current* source has told us its size yet. The ASS overlay
  // below is built from videoWidth/videoHeight and reads them once, so it
  // must not be built against the previous source's numbers — which is what
  // a switch would otherwise hand it, the old media still being loaded for
  // the moment it takes the new one to start.
  const [ready, setReady] = useState(false);
  // The position a tier switch is waiting for the transcode to reach, in
  // seconds; null when nothing is waiting.
  const [catchUp, setCatchUp] = useState<number | null>(null);
  // Bumped to re-attach the same tier from scratch. The one thing that needs
  // it: an encode reaped for being idle — which is what a pause longer than
  // the daemon's timeout looks like from here — leaves the segments the
  // player is about to ask for gone, and no amount of retrying brings them
  // back. Starting the encode again does, and the position is restored by
  // the same catch-up path a tier switch uses.
  const [reload, setReload] = useState(0);
  const restartedAtRef = useRef(0);

  // Adopt the remembered choice once the daemon has said what it offers, and
  // only if it still offers it — a tier that has been renamed or removed from
  // the config must not pin the player to something that 404s.
  useEffect(() => {
    if (typeof window === "undefined" || !tiers.length) return;
    const saved = localStorage.getItem(QUALITY_KEY);
    if (saved && saved !== SOURCE_QUALITY && tiers.some((t) => t.name === saved)) {
      setQuality(saved);
    }
    // Once, on the first status that carries tiers: after that the viewer's
    // clicks own this.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tiers.length]);

  const chooseQuality = useCallback((name: string) => {
    const v = videoRef.current;
    resumeRef.current = v?.currentTime ?? 0;
    setQuality(name);
    try {
      localStorage.setItem(QUALITY_KEY, name);
    } catch {
      /* private mode; the choice just does not outlive the page */
    }
  }, []);

  // True while the <video> *alone* is the fullscreen element, which is what
  // the player's own fullscreen button does. The ASS overlay is a sibling div,
  // so in that state it stays on the page behind the fullscreen video and the
  // viewer sees no captions at all.
  //
  // Two things were tried before settling for this. `controlsList="nofullscreen"`
  // to remove the button: Chrome draws it regardless (checked against the
  // accessibility tree, with and without the attribute). Then re-targeting
  // fullscreen to the container from this handler: rejected, because the UA's
  // own fullscreen request consumes the click's transient activation, so by the
  // time `fullscreenchange` fires `navigator.userActivation.isActive` is false
  // and `requestFullscreen` is a permissions error.
  const [videoIsFullscreen, setVideoIsFullscreen] = useState(false);
  useEffect(() => {
    const onChange = () => setVideoIsFullscreen(document.fullscreenElement === videoRef.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);

  // What is actually drawn, as against what the viewer picked. The two differ
  // in exactly one case: ARIB captions cannot be shown over a bare fullscreen
  // video, so while it is in that state the .vtt stands in — a native
  // TextTrack is drawn by the browser *inside* the fullscreen video, which is
  // the one mechanism the platform offers here. The toolbar keeps highlighting
  // the choice: this is a rendering fallback, not a change of mind, and it
  // reverses itself on the way out of fullscreen.
  const effective: CaptionMode =
    mode === "ass" && videoIsFullscreen && have?.vtt ? "vtt" : mode;

  const assUrl = recordingSubsUrl(rec.id, "ass");
  const vttUrl = recordingSubsUrl(rec.id, "vtt");

  // Ask what exists before offering to show it: a caption button that turns
  // nothing on is worse than one that isn't there. The .ass is fetched whole
  // because it is needed whole; the .vtt only has to be there, and the
  // browser will fetch it itself when the <track> mounts.
  useEffect(() => {
    let live = true;
    void Promise.all([
      fetch(assUrl).then(
        (r) => (r.ok ? r.text() : null),
        () => null,
      ),
      fetch(vttUrl, { method: "HEAD" }).then(
        (r) => r.ok,
        () => false,
      ),
    ]).then(([ass, vtt]) => {
      if (!live) return;
      setAssText(ass);
      setHave({ ass: ass !== null, vtt });
      setMode(ass !== null ? "ass" : vtt ? "vtt" : "off");
    });
    return () => {
      live = false;
    };
  }, [assUrl, vttUrl]);

  // Attaching the media, which is one of two things depending on the tier.
  //
  // Source is the MP4 itself: a plain src, served with Range, seekable
  // anywhere, and no encode running on the box. A tier is HLS, because that
  // is what an on-demand transcode can serve while it is still running — the
  // playlist is EXT-X-PLAYLIST-TYPE:EVENT and grows as ffmpeg gets through
  // the recording. It runs several times faster than the picture is watched
  // (8.2x realtime measured here for 480p), so it stays ahead; the one thing
  // a viewer can ask for and have to wait for is a seek into the part of the
  // programme it has not reached yet.
  //
  // hls.js is imported here rather than at the top of the file for the same
  // reason assjs is: it is a few hundred KB that a viewer who never leaves
  // Source should not have to fetch.
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    let live = true;
    setReady(false);
    setError(null);
    setCatchUp(null);
    // Where the viewer was when they picked another tier. Consumed here, so
    // a later reload of the same tier starts where the media says.
    const at = resumeRef.current;
    resumeRef.current = 0;

    const onMeta = () => {
      if (!live) return;
      // Only for the MP4, which has the whole programme the moment it has
      // metadata. On a tier the position is `reached`'s to restore, because
      // the encode may not have got there yet.
      if (at > 0 && quality === SOURCE_QUALITY) video.currentTime = at;
      setReady(true);
    };
    video.addEventListener("loadedmetadata", onMeta);

    if (quality === SOURCE_QUALITY) {
      video.src = recordingMp4Url(rec.id);
    } else {
      const url = recordingPlaylistUrl(rec.id, quality);
      void import("hls.js").then(({ default: Hls }) => {
        if (!live) return;
        if (!Hls.isSupported()) {
          // No MSE (iOS Safari): it plays HLS natively, and the position is
          // an ordinary seek once metadata arrives.
          video.src = url;
          return;
        }
        // Where the viewer was, once the encode covers it. Until then this
        // stays true and playback is held — see `reached` below.
        let waiting = at > 0;
        const h = new Hls({
          // Not the live edge, which is what hls.js would choose on its own:
          // the playlist grows and carries no ENDLIST until the encode
          // finishes, so it reads as live, and playback would start wherever
          // ffmpeg had got to — minutes into the programme. The viewer's own
          // position is restored by `reached`, not from here, because it may
          // be somewhere this encode has not written yet.
          startPosition: 0,
          // The first request is held open while ffmpeg writes its first
          // segment, which is a second or two rather than the tens of
          // milliseconds a playlist normally takes.
          manifestLoadPolicy: {
            default: {
              maxTimeToFirstByteMs: 60_000,
              maxLoadTimeMs: 90_000,
              timeoutRetry: { maxNumRetry: 2, retryDelayMs: 1_000, maxRetryDelayMs: 4_000 },
              errorRetry: { maxNumRetry: 4, retryDelayMs: 2_000, maxRetryDelayMs: 8_000 },
            },
          },
        });
        hlsRef.current = h;
        h.loadSource(url);
        h.attachMedia(video);
        h.on(Hls.Events.MANIFEST_PARSED, () => {
          if (!waiting) void video.play().catch(() => {});
        });

        // Switching tier partway through a programme asks a transcode that
        // has just started for a position it has not encoded yet, and a
        // player cannot seek past what it has been given: left alone, the
        // viewer lands back near the opening titles with no explanation. So
        // hold there, say so, and go to the position the moment the encode
        // covers it — which is a minute of programme every seven seconds or
        // so, with the playlist growing under us as it happens.
        //
        // The question is "how much has been *encoded*", which is the
        // playlist's own duration, and not `video.seekable` — that is what
        // has been *buffered*, and it is empty until the first fragment
        // arrives. Going by it would hold an encode that had already finished
        // for a position it already had, and with no further playlist
        // reloads coming (ENDLIST stops them) nothing would ever release it.
        if (at > 0) {
          const reached = (_evt: unknown, data: { details?: { totalduration?: number } }) => {
            if ((data.details?.totalduration ?? 0) + 0.5 < at) {
              // Paused rather than playing the opening at them: the viewer
              // asked to change the picture, not the scene.
              video.pause();
              setCatchUp(at);
              return;
            }
            h.off(Hls.Events.LEVEL_UPDATED, reached);
            waiting = false;
            setCatchUp(null);
            video.currentTime = at;
            void video.play().catch(() => {});
          };
          h.on(Hls.Events.LEVEL_UPDATED, reached);
        }
        h.on(Hls.Events.ERROR, (_evt, data) => {
          if (!data.fatal || !live) return;
          // Segments that are not there any more, which on this endpoint
          // means the encode was reaped rather than that anything is wrong:
          // ask for it again from the position the viewer is at. Once every
          // 30s at most, so a genuinely broken stream reports itself instead
          // of restarting an ffmpeg in a loop.
          const now = Date.now();
          if (data.type === Hls.ErrorTypes.NETWORK_ERROR && now - restartedAtRef.current > 30_000) {
            restartedAtRef.current = now;
            resumeRef.current = video.currentTime;
            setReload((n) => n + 1);
            return;
          }
          setError(`The ${quality} transcode stopped (${data.details}).`);
        });
      });
    }

    return () => {
      live = false;
      video.removeEventListener("loadedmetadata", onMeta);
      hlsRef.current?.destroy();
      hlsRef.current = null;
    };
  }, [rec.id, quality, reload]);

  // ASS.js, loaded only when it is about to draw something. The import has
  // to stay dynamic: the module calls document.createElement at load time,
  // and this page is prerendered at build time by the static export, where
  // there is no document. It also keeps ~40 KB out of the bundle every
  // other page pays for.
  //
  // Two things have to be true before it is constructed, and both are things
  // it reads *once*:
  //
  // `ready` is the first of them and now gates the whole effect: it goes
  // false the moment a source is attached and true when that source has
  // reported its size, so a tier change cannot build the overlay against the
  // dimensions of the picture being replaced.
  //
  //   - The video's intrinsic size. ASS.js takes the caption plane the script
  //     declares (960×540) and fits it to the picture, which it works out from
  //     `videoWidth`/`videoHeight` — and falls back to the element's own box
  //     when they are still 0. That fallback is silently wrong for us: the
  //     player's box is 16:9 but wider than the picture inside it, so the whole
  //     plane was being stretched across the letterbox bars, every caption
  //     drawn ~120px left of where the broadcast put it and a quarter too wide.
  //     Nothing recovers from it either — the resolution it derives is fixed at
  //     construction and its ResizeObserver only watches the element, which
  //     never changes size when metadata arrives.
  //   - The caption font. ASS.js measures the family the script names to
  //     convert an ASS size (a line box) into a font size, so that family has
  //     to be resolvable *before* it measures — it caches the result per name.
  //     See the @font-face in globals.css, which aliases it to a real one.
  useEffect(() => {
    if (!ready || effective !== "ass" || assText === null) return;
    const video = videoRef.current;
    const box = boxRef.current;
    if (!video || !box) return;

    let live = true;
    void Promise.all([
      import("assjs"),
      // Never a reason to fail on: with no matching local font the caption
      // still draws, in whatever the browser substitutes.
      document.fonts.load(`36px "${assFontFamily(assText)}"`).catch(() => []),
    ])
      .then(([mod]) => {
        if (!live) return;
        assRef.current = new mod.default(assText, video, { container: box });
      })
      .catch((e) => {
        if (live) setError(e instanceof Error ? e.message : String(e));
      });

    return () => {
      live = false;
      assRef.current?.destroy();
      assRef.current = null;
    };
  }, [effective, assText, ready]);

  // A <track> added after the element exists does not honour `default`, so
  // turn it on by hand — and again when the track element arrives, since
  // React mounts it a beat after this effect runs.
  useEffect(() => {
    const video = videoRef.current;
    if (!video || effective !== "vtt") return;
    const show = () => {
      for (const t of Array.from(video.textTracks)) {
        if (t.kind === "captions" || t.kind === "subtitles") t.mode = "showing";
      }
    };
    show();
    video.textTracks.addEventListener("addtrack", show);
    return () => video.textTracks.removeEventListener("addtrack", show);
  }, [effective]);

  // Ours takes the box, so the ASS overlay inside it comes too — the only way
  // to watch fullscreen with the captions where the broadcast put them.
  const fullscreen = useCallback(() => {
    if (document.fullscreenElement) void document.exitFullscreen();
    else void boxRef.current?.requestFullscreen?.();
  }, []);

  const modes: { key: CaptionMode; label: string; enabled: boolean; title: string }[] = [
    {
      key: "ass",
      label: "ARIB",
      enabled: have?.ass ?? false,
      title: "Captions where the broadcast put them",
    },
    {
      key: "vtt",
      label: "Text",
      enabled: have?.vtt ?? false,
      title: "The same captions as plain lines, drawn by the browser",
    },
    { key: "off", label: "Off", enabled: true, title: "No captions" },
  ];

  return (
    <div className="panel flex flex-col gap-2 p-2">
      {/* 16:9, capped so the table stays in view — but the cap has to go when
          this element *is* the screen, or fullscreen shows a 70%-tall video
          on a black field. */}
      <div
        ref={boxRef}
        className="relative aspect-video max-h-[70vh] overflow-hidden rounded bg-black
                   [&:fullscreen]:aspect-auto [&:fullscreen]:h-full [&:fullscreen]:max-h-none [&:fullscreen]:rounded-none"
      >
        {/* The source is attached by the effect above, not written here:
            which one it is depends on the tier, and switching tiers has to
            keep the position. */}
        <video
          ref={videoRef}
          controls
          playsInline
          autoPlay
          className="h-full w-full"
          onError={() =>
            setError(
              quality === SOURCE_QUALITY
                ? "The MP4 would not play — it may have been deleted since the page loaded."
                : `The ${quality} transcode would not play.`,
            )
          }
        >
          {/* Mounted only when the browser is the one drawing captions:
              leaving it in place would put a second, redundant entry in the
              browser's captions menu that draws the same words underneath
              ASS.js's. */}
          {effective === "vtt" && have?.vtt && (
            <track kind="captions" srcLang="ja" label="日本語" src={vttUrl} default />
          )}
        </video>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm">{rec.title || channelLabel}</p>
          <p className="font-mono text-[11px] text-faint tnum">
            {channelLabel} · {fmtDate(rec.start)} {fmtTime(rec.start)}
            {rec.end ? `–${fmtTime(rec.end)}` : ""}
          </p>
        </div>

        <div className="flex items-center gap-1.5">
          <span className="eyebrow">captions</span>
          {/* One control, three positions — not the browser's own captions
              menu, because in ARIB mode there is no TextTrack for it to list. */}
          <div className="flex overflow-hidden rounded-md border border-line">
            {modes.map((m) => (
              <button
                key={m.key}
                onClick={() => setMode(m.key)}
                disabled={!m.enabled}
                title={m.enabled ? m.title : `This recording has no ${m.label} captions`}
                // Same segmented control as the live player's, down to the
                // roomier hit box on a touch screen.
                className={`cursor-pointer px-2 py-1 text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-30 pointer-coarse:px-3 pointer-coarse:py-2 ${
                  mode === m.key ? "bg-fg text-canvas" : "bg-panel text-dim hover:bg-raised"
                }`}
              >
                {m.label}
              </button>
            ))}
          </div>
        </div>

        {/* Picture quality, when the daemon has tiers configured. Source is
            the recording's own file and costs nothing; a tier starts an
            encode on the box, which is why it is a deliberate click and not
            something the player decides on its own — the same rule the live
            tiers follow, and the same tier table. */}
        {tiers.length > 0 && (
          <div className="flex items-center gap-1.5">
            <span className="eyebrow">quality</span>
            <div className="flex overflow-hidden rounded-md border border-line">
              {[{ name: SOURCE_QUALITY, label: "Source" }, ...tiers].map((q) => (
                <button
                  key={q.name}
                  onClick={() => chooseQuality(q.name)}
                  title={
                    q.name === SOURCE_QUALITY
                      ? "The recording as it was transcoded — no re-encode, seeks anywhere"
                      : `Re-encode this recording at ${q.label} while you watch it`
                  }
                  className={`cursor-pointer px-2 py-1 text-[11px] transition-colors pointer-coarse:px-3 pointer-coarse:py-2 ${
                    quality === q.name ? "bg-fg text-canvas" : "bg-panel text-dim hover:bg-raised"
                  }`}
                >
                  {q.label}
                </button>
              ))}
            </div>
          </div>
        )}

        <button
          onClick={fullscreen}
          className="btn"
          title="Fullscreen with the ARIB captions — the player's own button cannot show them"
        >
          ⛶ Fullscreen
        </button>
        <button onClick={onClose} className="btn">
          Close
        </button>
      </div>

      {catchUp !== null && (
        <p className="text-xs text-dim">
          Transcoding — playback resumes at {clock(catchUp)} when the encode reaches it.
        </p>
      )}
      {error && <p className="text-xs text-rec">{error}</p>}
      {have && !have.ass && !have.vtt && (
        <p className="text-xs text-faint">No captions were found in this recording.</p>
      )}
    </div>
  );
}
