"use client";

import { useState } from "react";
import { useStatus } from "@/lib/api";

// Every address the live stream can be opened at, which is the same block
// the TUI puts under its channel list.
//
// It is here for the device that is not this browser: the URL you paste
// into VLC on the iPad, or hand to mpv. The daemon supplies the addresses
// because it is the only side that can see its own interfaces, and every
// one of them points at the same single playlist — a channel change does
// not invalidate the URL you wrote down.
export function WatchAddresses({ live }: { live: boolean }) {
  const { data } = useStatus();
  const [copied, setCopied] = useState<string | null>(null);
  const addresses = data?.addresses ?? [];
  const stream = data?.stream ?? "/stream.m3u8";

  if (!addresses.length) return null;

  const copy = async (url: string) => {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(url);
      setTimeout(() => setCopied((c) => (c === url ? null : c)), 1500);
    } catch {
      // A page served over plain http from a LAN address is not a secure
      // context in Chrome, so the clipboard API is simply absent. The URL
      // is right there to select by hand.
    }
  };

  return (
    <div className="flex flex-col gap-1">
      <span className="eyebrow">watch</span>
      {addresses.map((a) => {
        const url = a.base + stream;
        return (
          <button
            key={a.base}
            onClick={() => copy(url)}
            title="Copy"
            // Dimmed when nothing is tuned: these 404 until something is.
            // Still worth showing — this is where the address gets read off
            // the screen — but not dressed up as already playing.
            className={`flex cursor-pointer items-baseline gap-2 text-left font-mono text-[11px] ${
              live ? "text-dim" : "text-faint"
            } hover:text-fg`}
          >
            <span className="w-16 shrink-0 text-faint">{a.kind}</span>
            <span className="truncate">{url}</span>
            {copied === url && <span className="shrink-0 text-fg">copied</span>}
          </button>
        );
      })}
    </div>
  );
}
