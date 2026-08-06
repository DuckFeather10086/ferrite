"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import type ASS from "assjs";
import {
  fmtDate,
  fmtTime,
  recordingMp4Url,
  recordingSubsUrl,
  type Recording,
} from "@/lib/api";

// `HTMLMediaElement.HAVE_METADATA`, spelled out because the constant is only on
// the element and this is about a video that may not exist yet.
const HAVE_METADATA = 1;

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

  // ASS.js, loaded only when it is about to draw something. The import has
  // to stay dynamic: the module calls document.createElement at load time,
  // and this page is prerendered at build time by the static export, where
  // there is no document. It also keeps ~40 KB out of the bundle every
  // other page pays for.
  //
  // Two things have to be true before it is constructed, and both are things
  // it reads *once*:
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
    if (effective !== "ass" || assText === null) return;
    const video = videoRef.current;
    const box = boxRef.current;
    if (!video || !box) return;

    let live = true;
    const start = () => {
      if (!live) return;
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
    };

    if (video.readyState >= HAVE_METADATA) start();
    else video.addEventListener("loadedmetadata", start, { once: true });

    return () => {
      live = false;
      video.removeEventListener("loadedmetadata", start);
      assRef.current?.destroy();
      assRef.current = null;
    };
  }, [effective, assText]);

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
        <video
          ref={videoRef}
          src={recordingMp4Url(rec.id)}
          controls
          playsInline
          autoPlay
          className="h-full w-full"
          onError={() =>
            setError("The MP4 would not play — it may have been deleted since the page loaded.")
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
                className={`cursor-pointer px-2 py-1 text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-30 ${
                  mode === m.key ? "bg-fg text-canvas" : "bg-panel text-dim hover:bg-raised"
                }`}
              >
                {m.label}
              </button>
            ))}
          </div>
        </div>

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

      {error && <p className="text-xs text-rec">{error}</p>}
      {have && !have.ass && !have.vtt && (
        <p className="text-xs text-faint">No captions were found in this recording.</p>
      )}
    </div>
  );
}
