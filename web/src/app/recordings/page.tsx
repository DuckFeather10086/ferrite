"use client";

import { useRecordings, fmtDate, fmtTime, fmtBytes, type Recording } from "@/lib/api";

export default function RecordingsPage() {
  const { data: recordings, isLoading, mutate } = useRecordings();

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <button onClick={() => mutate()} className="btn text-xs">Refresh</button>
      </div>

      {isLoading && <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>Loading...</p>}

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Date</th>
              <th>Channel</th>
              <th>Time</th>
              <th>Title</th>
              <th>Size</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>
            {recordings?.map((r) => (
              <tr key={r.id}>
                <td className="whitespace-nowrap text-xs">{fmtDate(r.start)}</td>
                <td className="whitespace-nowrap font-medium">{r.channel}</td>
                <td className="whitespace-nowrap font-mono text-xs">
                  {fmtTime(r.start)} – {fmtTime(r.end)}
                </td>
                <td>
                  <span>{r.title || "—"}</span>
                  {r.error && (
                    <p className="text-xs mt-0.5" style={{ color: "var(--color-danger)" }}>{r.error}</p>
                  )}
                </td>
                <td className="whitespace-nowrap font-mono text-xs">{fmtBytes(r.size_bytes)}</td>
                <td><StateBadge state={r.state} /></td>
              </tr>
            ))}
            {recordings?.length === 0 && (
              <tr><td colSpan={6} className="text-center py-4" style={{ color: "var(--color-text-muted)" }}>No recordings yet.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function StateBadge({ state }: { state: string }) {
  const colors: Record<string, string> = {
    pending: "var(--color-warn)",
    recording: "var(--color-accent)",
    done: "var(--color-success)",
    failed: "var(--color-danger)",
  };
  return (
    <span className="badge" style={{ background: (colors[state] ?? "var(--color-text-muted)") + "22", color: colors[state] ?? "var(--color-text-muted)" }}>
      {state}
    </span>
  );
}
