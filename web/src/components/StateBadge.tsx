"use client";

// The state of a schedule row or a recording row. Both tables show it and
// both used to carry their own copy of this map — which had already
// drifted: the schedules copy had no entry for 'canceled' and built its
// background as `colors[state] + "22"`, so an unrecognized state rendered
// with the literal colour "undefined22".
const STATES: Record<string, { text: string; dot?: boolean }> = {
  pending: { text: "text-warn" },
  recording: { text: "text-rec", dot: true },
  done: { text: "text-ok" },
  failed: { text: "text-rec" },
  canceled: { text: "text-faint" },
};

export function StateBadge({ state }: { state: string }) {
  const s = STATES[state] ?? { text: "text-dim" };
  return (
    <span className={`badge whitespace-nowrap font-mono ${s.text}`}>
      {s.dot ? "● " : ""}
      {state}
    </span>
  );
}
