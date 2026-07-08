"use client";

import { useEffect, useState } from "react";
import { ChannelList } from "@/components/ChannelList";
import { NowPlaying } from "@/components/NowPlaying";
import { VideoPlayer } from "@/components/VideoPlayer";
import {
  CHANNEL_GROUP_ORDER,
  channelGroup,
  stopLive,
  useChannels,
} from "@/lib/api";

export default function LivePage() {
  const { data: channels } = useChannels();
  const [active, setActive] = useState<string | null>(null);

  // Auto-select the first channel in sidebar display order (GR first)
  // so the page isn't empty on first load. channels.json's raw order
  // starts with cable muxes this antenna can't receive — auto-tuning a
  // dead channel wedges the single tuner for the whole lock timeout.
  useEffect(() => {
    if (!active && channels?.length) {
      const first =
        CHANNEL_GROUP_ORDER.flatMap((g) =>
          channels.filter((c) => channelGroup(c) === g),
        )[0] ?? channels[0];
      setActive(first.name);
    }
  }, [channels, active]);

  const serviceId = channels?.find((c) => c.name === active)?.service_id;
  const playUrl = active ? `/api/live/${encodeURIComponent(active)}.m3u8` : null;

  // Switching releases the current channel's tuner lease first — on a
  // single-adapter box the new tune could never acquire otherwise (the
  // old session would hold the adapter until the 60s idle janitor).
  // The stop endpoint tears the session down synchronously, so once it
  // returns the adapter is free for the new channel.
  const switchTo = async (name: string) => {
    if (name === active) return;
    if (active) {
      try {
        await stopLive(active);
      } catch {
        /* stale session — the new tune will surface any real error */
      }
    }
    setActive(name);
  };

  const handleStop = () => {
    if (active) {
      stopLive(active);
      setActive(null);
    }
  };

  const activeIdx = channels?.findIndex((c) => c.name === active) ?? -1;
  const onPrev = () => {
    if (!channels || activeIdx <= 0) return;
    void switchTo(channels[activeIdx - 1].name);
  };
  const onNext = () => {
    if (!channels || activeIdx < 0 || activeIdx >= channels.length - 1) return;
    void switchTo(channels[activeIdx + 1].name);
  };

  return (
    <div className="flex flex-col lg:flex-row gap-4">
      {/* Channel sidebar */}
      <aside className="w-full lg:w-60 shrink-0">
        <ChannelList active={active} onSelect={(name) => void switchTo(name)} />
      </aside>

      {/* Player + now playing */}
      <div className="flex-1 flex flex-col gap-3 min-w-0">
        <VideoPlayer src={playUrl} onPrev={onPrev} onNext={onNext} />
        <NowPlaying serviceId={serviceId} channelName={active} onStop={handleStop} />
      </div>
    </div>
  );
}
