"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ChannelList } from "@/components/ChannelList";
import { NowPlaying } from "@/components/NowPlaying";
import { VideoPlayer } from "@/components/VideoPlayer";
import { WatchAddresses } from "@/components/WatchAddresses";
import {
  CHANNEL_GROUP_ORDER,
  channelGroup,
  isSuperseded,
  qualityQuery,
  stopLive,
  switchLive,
  useChannels,
  useStatus,
} from "@/lib/api";

// Where the viewer's quality choice is kept. Same shape as the player's
// caption preference and for the same reason: it is a property of this
// screen and this connection, not of the daemon, and having to re-pick it
// on every visit is what makes a setting feel broken.
const QUALITY_KEY = "ferrite.liveQuality";

export default function LivePage() {
  const { data: channels } = useChannels();
  const { data: status } = useStatus();

  const [active, setActive] = useState<string | null>(null);
  const [playUrl, setPlayUrl] = useState<string | null>(null);
  const [tuning, setTuning] = useState(false);
  const [fatal, setFatal] = useState<string | null>(null);
  // null until the daemon has said which tiers it has, so the first tune
  // asks for nothing and gets the default rather than a stale name.
  const [quality, setQuality] = useState<string | null>(null);

  const qualities = useMemo(() => status?.live_qualities ?? [], [status]);

  // Channel surfing is several channel changes in flight at once, and only the
  // last one is the viewer's answer. Two things keep the others from being
  // heard: the request is aborted, and — because an abort races a response
  // already on the wire — every one carries a sequence number and only the
  // current one may touch state. Without the guard the daemon's reply to a
  // press the viewer had already moved past would land afterwards and set
  // `fatal`, putting an error over a channel that was playing.
  const switchSeq = useRef(0);
  const switchAbort = useRef<AbortController | null>(null);

  // Adopt the remembered choice once, and only if the daemon still offers
  // it — the tier list is config, and a name that has been renamed or
  // removed must not pin the player to a tier that does not exist.
  useEffect(() => {
    if (quality || !qualities.length) return;
    const saved = typeof window === "undefined" ? null : localStorage.getItem(QUALITY_KEY);
    setQuality(saved && qualities.some((q) => q.name === saved) ? saved : qualities[0].name);
  }, [quality, qualities]);

  // Sidebar order (GR first), which is also the order the arrow keys walk.
  // channels.json's raw order starts with cable muxes this antenna cannot
  // receive, so the raw index would step through channels that are not
  // where they appear on screen.
  const ordered = useMemo(() => {
    if (!channels) return [];
    return CHANNEL_GROUP_ORDER.flatMap((g) => channels.filter((c) => channelGroup(c) === g));
  }, [channels]);

  // What the daemon has tuned for viewing, if anything. A reserved
  // adapter is an EPG pass and has no channel to adopt.
  const tuned = status?.adapters?.find((a) => a.channel && !a.reserved)?.channel;

  // Opening this page must not take the tuner. It adopts a session that is
  // already running — so a second browser, or a reload, joins what is on
  // rather than restarting it — but an idle box stays idle until someone
  // asks for a channel. The page used to tune on load, which meant merely
  // looking at the UI held the adapter for the session's whole idle
  // timeout.
  useEffect(() => {
    if (active || !ordered.length) return;
    if (tuned) {
      setActive(tuned);
      setPlayUrl(playlistFor(tuned, quality));
    } else {
      setActive(ordered[0].name);
    }
  }, [active, ordered, tuned, quality]);

  // One endpoint does the whole channel change: it closes any other
  // session, tunes, and answers only once the playlist is on disk. Doing
  // it by hand as stop-then-open is what this page used to do, and with a
  // single adapter the wrong order deadlocks — two live sessions have
  // equal priority and will not evict each other.
  const watch = useCallback(
    async (name: string, tier?: string | null) => {
    const q = tier === undefined ? quality : tier;
    switchAbort.current?.abort();
    const ac = new AbortController();
    switchAbort.current = ac;
    const seq = ++switchSeq.current;
    const current = () => seq === switchSeq.current;
    setActive(name);
    setFatal(null);
    setPlayUrl(null);
    setTuning(true);
    // Let the player actually go away before asking the daemon to change
    // channel. hls.js polls the live playlist it is on every couple of
    // seconds, and a GET on a channel's playlist *tunes* that channel — so a
    // poll landing between the daemon closing the old session and claiming the
    // adapter for the new one takes the tuner straight back, and the switch
    // fails with "no adapter available". Clearing src above destroys the hls.js
    // instance, but only once React has run the effect: one frame.
    await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    try {
      const out = await switchLive(name, q ?? undefined, ac.signal);
      if (!current()) return;
      // The daemon says which tier it actually started — an unknown name
      // gets the default rather than an error, and the control should show
      // what is playing rather than what was asked for.
      setQuality(out.quality ?? q ?? null);
      setPlayUrl(playlistFor(name, out.quality ?? q));
    } catch (e) {
      // Overtaken by a later press, here or at the daemon: not this viewer's
      // problem, and the press that overtook it owns the screen now.
      if (isSuperseded(e) || !current()) return;
      // A recording holds the adapter, or the frontend never locked. The
      // reason is on the response, and hls.js would never see it.
      setFatal(e instanceof Error ? e.message : String(e));
    } finally {
      if (current()) setTuning(false);
    }
    },
    [quality],
  );

  // Changing tier is a re-tune at the new quality: the daemon starts (or
  // joins) that encode, and the player reloads onto its playlist. There is
  // no in-manifest switch to make — one variant is on offer by design, so
  // that a tier nobody asked for is never being encoded.
  const chooseQuality = useCallback(
    (name: string) => {
      if (name === quality) return;
      setQuality(name);
      try {
        localStorage.setItem(QUALITY_KEY, name);
      } catch {
        /* private mode; the choice just does not outlive the page */
      }
      if (active && playUrl) void watch(active, name);
    },
    [quality, active, playUrl, watch],
  );

  const stop = useCallback(async () => {
    if (!active) return;
    setPlayUrl(null);
    setFatal(null);
    try {
      await stopLive(active);
    } catch {
      /* already gone — nothing left to release */
    }
  }, [active]);

  const step = useCallback(
    (delta: number) => {
      const i = ordered.findIndex((c) => c.name === active);
      const next = ordered[i + delta];
      if (i >= 0 && next) void watch(next.name);
    },
    [ordered, active, watch],
  );

  const serviceId = channels?.find((c) => c.name === active)?.service_id;

  return (
    <div className="flex flex-col gap-4 lg:flex-row">
      {/* Stacked on a phone, the player comes first — it is what the page is
          for. Side by side, the list goes back to being the left sidebar. */}
      <aside className="order-2 w-full shrink-0 lg:order-none lg:w-64">
        <ChannelList active={active} tuned={tuned} onSelect={(name) => void watch(name)} />
      </aside>

      <div className="order-1 flex min-w-0 flex-1 flex-col gap-3 lg:order-none">
        <div className="relative">
          <VideoPlayer
            src={playUrl}
            fatal={fatal}
            onPrev={() => step(-1)}
            onNext={() => step(1)}
            qualities={qualities}
            quality={quality}
            onQuality={chooseQuality}
          />

          {/* Idle: a channel is selected and nothing is playing. The
              player is black, so say what to press. */}
          {!playUrl && !tuning && !fatal && active && (
            <button
              onClick={() => void watch(active)}
              className="absolute inset-0 flex cursor-pointer items-center justify-center"
            >
              <span className="btn btn-primary px-4 py-2 text-sm">▶ Watch</span>
            </button>
          )}

          {tuning && (
            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-2">
              <div className="h-6 w-6 animate-spin rounded-full border border-white/25 border-t-white" />
              <span className="font-mono text-[11px] text-white/70">Tuning…</span>
            </div>
          )}
        </div>

        <NowPlaying
          serviceId={serviceId}
          channelName={active}
          playing={Boolean(playUrl)}
          onStop={() => void stop()}
        />
        <WatchAddresses live={Boolean(tuned)} />
      </div>
    </div>
  );
}

// The channel's playlist is the multivariant one: it names the caption
// rendition beside the video, and the daemon composes it per request so the
// same manifest works from here and from /stream.m3u8.
//
// ?q= names the tier. It carries one variant, not a ladder — see
// internal/hls/quality.go for why the choice is the viewer's and not the
// player's.
function playlistFor(channel: string, quality?: string | null) {
  return `/api/live/${encodeURIComponent(channel)}.m3u8${qualityQuery(quality)}`;
}
