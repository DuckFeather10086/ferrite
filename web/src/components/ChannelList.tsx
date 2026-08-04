"use client";

import { useMemo, useState } from "react";
import {
  CHANNEL_GROUP_ORDER,
  channelGroup,
  displayName,
  useChannels,
  useNowByService,
  type Channel,
  type ChannelGroup,
} from "@/lib/api";

export type ChannelListProps = {
  // The channel this page is watching. Distinct from `tuned`: during a
  // switch the selection has moved and the adapter has not caught up.
  active: string | null;
  // What the daemon reports as tuned, if anything.
  tuned?: string;
  onSelect: (name: string) => void;
};

// Channel sidebar: grouped by ISDB service-id band, labelled with the
// daemon's display_name, with each row's now-playing programme underneath.
//
// One vertical list at every width. It used to lay the groups out as
// side-by-side columns below the `lg` breakpoint, which on a phone filled
// the whole viewport with a horizontally scrolling grid and pushed the
// player off the bottom of the page — you could not see the television.
export function ChannelList({ active, tuned, onSelect }: ChannelListProps) {
  const { data: channels } = useChannels();
  const nowByService = useNowByService();
  const [q, setQ] = useState("");

  const filtered = useMemo(() => {
    if (!channels) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return channels;
    // Aliases are searchable but never shown: for a legacy record the
    // alias is the mojibake, and matching on it is how you find a channel
    // whose label you can read but whose key you cannot.
    return channels.filter(
      (c) =>
        displayName(c).toLowerCase().includes(needle) ||
        c.name.toLowerCase().includes(needle) ||
        c.aliases?.some((a) => a.toLowerCase().includes(needle)),
    );
  }, [channels, q]);

  const grouped = useMemo(() => {
    const m = new Map<ChannelGroup, Channel[]>();
    for (const c of filtered) {
      const g = channelGroup(c);
      const arr = m.get(g) ?? [];
      arr.push(c);
      m.set(g, arr);
    }
    return m;
  }, [filtered]);

  if (!channels?.length) {
    return <p className="text-xs text-dim">No channels — is channels.json loaded?</p>;
  }

  return (
    <div className="flex h-full flex-col gap-2">
      <input
        type="search"
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="Filter channels"
        className="field w-full"
      />

      {/* Capped and scrollable on a phone so 39 channels do not become a
          page you scroll past to reach anything else; the desktop sidebar
          takes the column's full height instead. */}
      <div className="flex max-h-[50vh] flex-col gap-3 overflow-y-auto lg:max-h-none lg:flex-1">
        {CHANNEL_GROUP_ORDER.map((g) => {
          const items = grouped.get(g);
          if (!items?.length) return null;
          return (
            <section key={g} className="flex flex-col gap-px">
              <h3 className="eyebrow px-2 pb-1">{g}</h3>
              {items.map((c) => (
                <ChannelRow
                  key={c.name}
                  channel={c}
                  nowTitle={nowByService.get(c.service_id)?.title}
                  active={active === c.name}
                  tuned={tuned === c.name}
                  onSelect={onSelect}
                />
              ))}
            </section>
          );
        })}
        {filtered.length === 0 && <p className="px-2 py-4 text-xs text-dim">No match.</p>}
      </div>
    </div>
  );
}

type RowProps = {
  channel: Channel;
  nowTitle?: string;
  active: boolean;
  tuned: boolean;
  onSelect: (name: string) => void;
};

function ChannelRow({ channel, nowTitle, active, tuned, onSelect }: RowProps) {
  return (
    <button
      onClick={() => onSelect(channel.name)}
      // Selection is inversion, the same emphasis the TUI's cursor uses.
      className={`w-full cursor-pointer rounded-md px-2 py-1.5 text-left transition-colors ${
        active ? "bg-fg text-canvas" : "hover:bg-panel"
      }`}
    >
      <div className="flex items-baseline gap-1.5">
        {/* Only the tuned channel is marked. This used to give a red dot to
            every row *except* the selected one, so a 39-channel sidebar
            came up as a column of red. */}
        <span
          className={`w-2 shrink-0 text-center text-[10px] leading-none ${
            active ? "text-canvas" : "text-rec"
          }`}
          aria-hidden
        >
          {tuned ? "●" : ""}
        </span>
        <span className={`truncate text-[13px] ${active ? "font-medium" : ""}`}>
          {displayName(channel)}
        </span>
      </div>
      {nowTitle && (
        <p className={`truncate pl-3.5 text-[11px] ${active ? "text-canvas/60" : "text-faint"}`}>
          {nowTitle}
        </p>
      )}
    </button>
  );
}
