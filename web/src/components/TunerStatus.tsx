"use client";

import { useChannelIndex, useStatus } from "@/lib/api";

// The adapter strip, which is the same sentence the TUI's status bar
// writes: what each adapter is doing, and whether anything is recording.
//
// It reports occupancy per adapter rather than a "2/3 busy" count. On a
// one-tuner box the count says nothing you can act on — "busy" is only
// useful once you know *what* has it, because that decides whether your
// next click will succeed (an EPG pass yields to live; another
// recording does not).
export function TunerStatus() {
  const { data } = useStatus();
  const { label } = useChannelIndex();
  const adapters = data?.adapters ?? [];
  const recording = data?.recording ?? [];

  if (!adapters.length) {
    return <span className="font-mono text-[11px] text-faint">no tuners</span>;
  }

  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 font-mono text-[11px]">
      {adapters.map((a) => (
        <span key={a.adapter} className="flex items-center gap-1">
          <span className="text-faint">a{a.adapter}</span>
          {a.reserved ? (
            // Held without a fanout: an EPG pass has the adapter and no
            // channel to name. Reading this as idle is how the UI came to
            // claim a free tuner while the next tune waited on a lock.
            <span className="text-dim">EPG</span>
          ) : a.channel ? (
            <>
              <span className="text-fg">{label(a.channel)}</span>
              <span className="text-faint">
                ×{a.refs}
                {a.prio ? `/${a.prio}` : ""}
              </span>
            </>
          ) : (
            <span className="text-faint">idle</span>
          )}
        </span>
      ))}

      {recording.length > 0 && (
        <span className="text-rec">
          ● REC{recording.length > 1 ? ` ×${recording.length}` : ""}
        </span>
      )}
    </div>
  );
}
