"use client";

import { useState } from "react";

import {
  useRecordings,
  deleteRecording,
  recordingFileUrl,
  fmtDate,
  fmtTime,
  fmtBytes,
  type Recording,
} from "@/lib/api";

export default function RecordingsPage() {
  const { data: recordings, isLoading, mutate } = useRecordings();
  const [error, setError] = useState<string | null>(null);
  // Which row is mid-delete, so its buttons stop responding instead of
  // firing a second DELETE.
  const [busyId, setBusyId] = useState<number | null>(null);

  async function remove(r: Recording) {
    const what = r.title || `${r.channel} ${fmtDate(r.start)}`;
    if (!confirm(`Delete "${what}" and its file? This cannot be undone.`)) return;
    setBusyId(r.id);
    setError(null);
    try {
      await deleteRecording(r.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusyId(null);
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <button onClick={() => mutate()} className="btn text-xs">Refresh</button>
      </div>

      {isLoading && <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>Loading...</p>}
      {error && <p className="text-sm" style={{ color: "var(--color-danger)" }}>{error}</p>}

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
              <th></th>
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
                <td className="whitespace-nowrap text-xs">
                  <div className="flex items-center gap-2 justify-end">
                    {/* The TS is raw MPEG-2 with ARIB audio, which no browser
                        plays natively — hence a download rather than an
                        inline player. mpv/VLC open the same URL directly. */}
                    {hasBytes(r) ? (
                      <a href={recordingFileUrl(r.id)} download className="btn text-xs">Download</a>
                    ) : (
                      <span style={{ color: "var(--color-text-muted)" }}>—</span>
                    )}
                    <button
                      onClick={() => remove(r)}
                      className="btn text-xs"
                      // The daemon refuses to delete a running recording; don't
                      // offer it here either.
                      disabled={busyId === r.id || r.state === "recording"}
                      title={r.state === "recording" ? "Stop the recording first" : "Delete file and row"}
                      style={{ color: "var(--color-danger)" }}
                    >
                      {busyId === r.id ? "Deleting…" : "Delete"}
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {recordings?.length === 0 && (
              <tr><td colSpan={7} className="text-center py-4" style={{ color: "var(--color-text-muted)" }}>No recordings yet.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// size_bytes is only written when the row is finalized, so an in-progress
// recording has none — and a failed job may have produced nothing at all.
function hasBytes(r: Recording): boolean {
  return r.state === "recording" || (r.size_bytes ?? 0) > 0;
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
