"use client";

import { useMemo, useState } from "react";
import {
  CHANNEL_GROUP_ORDER,
  channelGroup,
  displayName,
  useChannels,
  useNow,
  type Channel,
  type ChannelGroup,
} from "@/lib/api";

export type ChannelListProps = {
  active: string | null;
  onSelect: (name: string) => void;
  disabled?: boolean;
  disabledReason?: string;
};

// Channel sidebar. Groups by ISDB service-id band (GR/BS/CS/SKY),
// shows the human-readable name (aliases[0]), marks the live channel
// with a red dot, and lazy-loads each visible channel's now-playing
// subtitle. Includes a filter box. On mobile collapses to a horizontal
// scrollable chip row.
export function ChannelList({ active, onSelect, disabled, disabledReason }: ChannelListProps) {
  const { data: channels } = useChannels();
  const [q, setQ] = useState("");

  const filtered = useMemo(() => {
    if (!channels) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return channels;
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
    return (
      <p className="text-xs" style={{ color: "var(--color-text-muted)" }}>
        channels.json が読み込まれていません
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-2 h-full">
      <input
        type="search"
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="チャンネル検索…"
        className="w-full px-3 py-1.5 rounded-lg text-sm border outline-none"
        style={{
          background: "var(--color-surface)",
          borderColor: "var(--color-border)",
          color: "var(--color-text)",
        }}
      />

      <div className="flex lg:flex-col gap-3 overflow-x-auto lg:overflow-y-auto lg:flex-1 pb-1 lg:pb-0">
        {CHANNEL_GROUP_ORDER.map((g) => {
          const items = grouped.get(g);
          if (!items?.length) return null;
          return (
            <section key={g} className="flex flex-col gap-1 min-w-40 lg:min-w-0">
              <h3
                className="text-[10px] font-semibold uppercase tracking-wider px-2 pt-1 lg:pt-2"
                style={{ color: "var(--color-text-muted)" }}
              >
                {g}
              </h3>
              {items.map((c) => (
                <ChannelRow
                  key={c.name}
                  channel={c}
                  active={active === c.name}
                  onSelect={onSelect}
                  disabled={disabled}
                  disabledReason={disabledReason}
                />
              ))}
            </section>
          );
        })}
        {filtered.length === 0 && (
          <p className="text-xs px-2 py-4" style={{ color: "var(--color-text-muted)" }}>
            該当なし
          </p>
        )}
      </div>
    </div>
  );
}

type RowProps = {
  channel: Channel;
  active: boolean;
  onSelect: (name: string) => void;
  disabled?: boolean;
  disabledReason?: string;
};

function ChannelRow({ channel, active, onSelect, disabled, disabledReason }: RowProps) {
  const { data: now } = useNow(channel.service_id);
  const title = now?.title;
  return (
    <button
      onClick={() => !disabled && onSelect(channel.name)}
      disabled={disabled}
      title={disabled ? disabledReason : undefined}
      className={`w-full text-left px-3 py-2 rounded-lg transition-colors ${
        active ? "" : "hover:bg-white/5"
      } ${disabled ? "opacity-40 cursor-not-allowed" : "cursor-pointer"}`}
      style={{
        background: active ? "var(--color-accent-dim)" : "transparent",
        color: active ? "#042027" : "var(--color-text)",
        boxShadow: active ? "0 0 0 1px var(--color-accent), 0 0 12px rgba(34, 211, 238, 0.25)" : "none",
      }}
    >
      <div className="flex items-center gap-2">
        <span
          className={`inline-block w-2 h-2 rounded-full shrink-0 ${
            active ? "bg-[#042027]" : "bg-red-500"
          }`}
          aria-hidden
        />
        <span className={`text-sm truncate ${active ? "font-semibold" : "font-normal"}`}>
          {displayName(channel)}
        </span>
      </div>
      {title && (
        <p
          className="text-[11px] mt-0.5 truncate"
          style={{ color: active ? "rgba(4, 32, 39, 0.65)" : "var(--color-text-muted)" }}
        >
          {title}
        </p>
      )}
    </button>
  );
}
