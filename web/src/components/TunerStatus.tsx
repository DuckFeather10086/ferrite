"use client";

import { useStatus } from "@/lib/api";

// Header tuner-occupancy badge. Uses `refs > 0` (not a non-existent
// `busy` field) to decide whether an adapter is in use. Turns red and
// pulses when every adapter is busy — the Live page also gates channel
// switching on this so the user gets a clear "no free tuner" signal.
export function TunerStatus() {
  const { data } = useStatus();
  const adapters = data?.adapters ?? [];
  const total = adapters.length;
  const busy = adapters.filter((a) => a.refs > 0).length;
  const saturated = total > 0 && busy >= total;

  if (total === 0) {
    return (
      <span
        className="text-xs px-2 py-0.5 rounded-full"
        style={{ background: "var(--color-surface)", color: "var(--color-text-muted)" }}
        title="No DVB adapters configured"
      >
        no tuners
      </span>
    );
  }

  return (
    <span
      className={`text-xs px-2 py-0.5 rounded-full ${saturated ? "animate-pulse" : ""}`}
      style={{
        background: "var(--color-surface)",
        color: saturated ? "var(--color-danger)" : "var(--color-text-muted)",
        border: saturated ? "1px solid var(--color-danger)" : "1px solid var(--color-border)",
      }}
      title={saturated ? "All tuners busy — stop a stream or recording to free one" : `${busy} of ${total} tuners in use`}
    >
      tuners {busy}/{total}
    </span>
  );
}

// Non-hook helper exported for the Live page to decide whether channel
// switching should be gated. Returns the live AdapterStatus snapshot.
export function useTunersFree(): { total: number; busy: number; free: number } {
  const { data } = useStatus();
  const adapters = data?.adapters ?? [];
  const total = adapters.length;
  const busy = adapters.filter((a) => a.refs > 0).length;
  return { total, busy, free: Math.max(0, total - busy) };
}
