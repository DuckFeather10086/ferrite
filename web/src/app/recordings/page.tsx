"use client";

import { useState } from "react";
import { StateBadge } from "@/components/StateBadge";
import {
  deleteRecording,
  fmtBytes,
  fmtDate,
  fmtTime,
  recordingFileUrl,
  stopRecording,
  useChannelIndex,
  useRecordings,
  type Recording,
} from "@/lib/api";

export default function RecordingsPage() {
  const index = useChannelIndex();
  const { data: recordings, isLoading, mutate } = useRecordings();
  const [error, setError] = useState<string | null>(null);
  // Which row has an action in flight, so its buttons stop responding
  // instead of firing a second DELETE.
  const [busyId, setBusyId] = useState<number | null>(null);

  const act = async (id: number, fn: () => Promise<unknown>) => {
    setBusyId(id);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusyId(null);
    }
  };

  const remove = (r: Recording) => {
    const what = r.title || `${index.label(r.channel)} ${fmtDate(r.start)}`;
    if (!confirm(`Delete "${what}" and its file? This cannot be undone.`)) return;
    void act(r.id, () => deleteRecording(r.id));
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <button onClick={() => mutate()} className="btn">
          Refresh
        </button>
        <span className="font-mono text-[11px] text-faint">
          {recordings?.length ?? 0} rows
        </span>
      </div>

      {isLoading && <p className="text-sm text-dim">Loading…</p>}
      {error && <p className="text-sm text-rec">{error}</p>}

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Date</th>
              <th>Channel</th>
              <th>Start – End</th>
              <th>Title</th>
              <th className="text-right">Size</th>
              <th>State</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {recordings?.map((r) => (
              <tr key={r.id}>
                <td className="whitespace-nowrap text-xs text-dim tnum">{fmtDate(r.start)}</td>
                {/* The row stores the canonical channel name; show the label. */}
                <td className="whitespace-nowrap">{index.label(r.channel)}</td>
                <td className="whitespace-nowrap font-mono text-xs tnum">
                  {fmtTime(r.start)} – {fmtTime(r.end)}
                </td>
                <td>
                  {r.title || <span className="text-faint">—</span>}
                  {r.error && <p className="mt-0.5 text-xs text-rec">{r.error}</p>}
                </td>
                <td className="whitespace-nowrap text-right font-mono text-xs tnum">
                  {fmtBytes(r.size_bytes)}
                </td>
                <td>
                  <StateBadge state={r.state} />
                </td>
                <td>
                  <div className="flex items-center justify-end gap-1.5">
                    {r.state === "recording" ? (
                      // The graceful finish: the row goes to 'done' with the
                      // bytes written. Without this the UI was a dead end —
                      // Delete refuses a running recording and told you to
                      // stop it first, with nothing here that could.
                      <button
                        onClick={() => void act(r.id, () => stopRecording(r.id))}
                        disabled={busyId === r.id}
                        className="btn btn-danger"
                        title="Finish this recording now"
                      >
                        ■ Stop
                      </button>
                    ) : (
                      <>
                        {/* Raw MPEG-2 TS with ARIB audio: no browser plays it
                            inline, so this is a download, and the same URL
                            opens in mpv or VLC (the endpoint honours Range). */}
                        {(r.size_bytes ?? 0) > 0 ? (
                          <a href={recordingFileUrl(r.id)} download className="btn">
                            Download
                          </a>
                        ) : (
                          <span className="px-1 text-faint">—</span>
                        )}
                        <button
                          onClick={() => remove(r)}
                          disabled={busyId === r.id}
                          className="btn btn-danger"
                          title="Delete the file and the row"
                        >
                          {busyId === r.id ? "Deleting…" : "Delete"}
                        </button>
                      </>
                    )}
                  </div>
                </td>
              </tr>
            ))}
            {recordings?.length === 0 && (
              <tr>
                <td colSpan={7} className="py-6 text-center text-dim">
                  No recordings yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
