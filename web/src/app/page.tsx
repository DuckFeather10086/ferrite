"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { ChannelList } from "@/components/ChannelList";
import { NowPlaying } from "@/components/NowPlaying";
import { VideoPlayer } from "@/components/VideoPlayer";
import { WatchAddresses } from "@/components/WatchAddresses";
import {
  CHANNEL_GROUP_ORDER,
  channelGroup,
  stopLive,
  switchLive,
  useChannels,
  useStatus,
} from "@/lib/api";

export default function LivePage() {
  const { data: channels } = useChannels();
  const { data: status } = useStatus();

  const [active, setActive] = useState<string | null>(null);
  const [playUrl, setPlayUrl] = useState<string | null>(null);
  const [tuning, setTuning] = useState(false);
  const [fatal, setFatal] = useState<string | null>(null);

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
      setPlayUrl(playlistFor(tuned));
    } else {
      setActive(ordered[0].name);
    }
  }, [active, ordered, tuned]);

  // One endpoint does the whole channel change: it closes any other
  // session, tunes, and answers only once the playlist is on disk. Doing
  // it by hand as stop-then-open is what this page used to do, and with a
  // single adapter the wrong order deadlocks — two live sessions have
  // equal priority and will not evict each other.
  const watch = useCallback(async (name: string) => {
    setActive(name);
    setFatal(null);
    setPlayUrl(null);
    setTuning(true);
    try {
      await switchLive(name);
      setPlayUrl(playlistFor(name));
    } catch (e) {
      // A recording holds the adapter, or the frontend never locked. The
      // reason is on the response, and hls.js would never see it.
      setFatal(e instanceof Error ? e.message : String(e));
    } finally {
      setTuning(false);
    }
  }, []);

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

// The multivariant playlist, not the media playlist: it is what names the
// caption rendition beside the video. Written when the session opens, so it
// exists for any session this build created.
function playlistFor(channel: string) {
  return `/api/live/${encodeURIComponent(channel)}/master.m3u8`;
}
