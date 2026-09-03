"use client";

import { useEffect, useRef, useState } from "react";
import { RecordingPlayer } from "@/components/RecordingPlayer";
import { StateBadge } from "@/components/StateBadge";
import {
  deleteRecording,
  fmtBytes,
  fmtDate,
  fmtTime,
  isPlayable,
  postprocessRecording,
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
  // The row being watched, by id rather than by value: the list re-fetches
  // under the player (every 5s while anything is converting) and holding
  // the object would pin a stale copy of the row.
  const [watchingId, setWatchingId] = useState<number | null>(null);
  const playerRef = useRef<HTMLDivElement>(null);

  const watching = recordings?.find((r) => r.id === watchingId) ?? null;
  // A recording deleted from another tab — or one whose files went with a
  // DELETE here — leaves the player pointing at nothing.
  useEffect(() => {
    if (watchingId !== null && recordings && !watching) setWatchingId(null);
  }, [watchingId, recordings, watching]);

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

  const watch = (r: Recording) => {
    setWatchingId(r.id);
    // The list is long enough that the row you clicked can be well below
    // the player it just loaded.
    requestAnimationFrame(() =>
      playerRef.current?.scrollIntoView({ behavior: "smooth", block: "start" }),
    );
  };

  return (
    <div className="flex flex-col gap-3">
      {/* Always mounted so `watch` has a ref to scroll to, but `empty:hidden`
          keeps it out of the flex flow — otherwise an idle page carries the
          column gap of a player that isn't there. */}
      <div ref={playerRef} className="empty:hidden">
        {watching && (
          <RecordingPlayer
            // Remount on a change of recording rather than swapping the src
            // under a live ASS.js instance and a loaded <track>.
            key={watching.id}
            rec={watching}
            channelLabel={index.label(watching.channel)}
            onClose={() => setWatchingId(null)}
          />
        )}
      </div>

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
              // The row the player is on, marked as the table's own hover
              // state does it — the player is above and can be off screen.
              <tr key={r.id} className={watchingId === r.id ? "bg-panel" : undefined}>
                <td className="whitespace-nowrap text-xs text-dim tnum">{fmtDate(r.start)}</td>
                {/* The row stores the canonical channel name; show the label. */}
                <td className="whitespace-nowrap">{index.label(r.channel)}</td>
                <td className="whitespace-nowrap font-mono text-xs tnum">
                  {fmtTime(r.start)} – {fmtTime(r.end)}
                </td>
                <td>
                  {r.title || <span className="text-faint">—</span>}
                  {r.error && <p className="mt-0.5 text-xs text-rec">{r.error}</p>}
                  {/* A recording that is fine but whose transcode is not: the
                      row is 'done' and only the Convert button knows why it
                      is still there, which is not where a person looks. */}
                  {r.post_state === "failed" && (
                    <p className="mt-0.5 text-xs text-warn">
                      Conversion failed{r.post_error ? `: ${r.post_error}` : ""}
                    </p>
                  )}
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
                        {(r.size_bytes ?? 0) > 0 ? (
                          <>
                            {/* What plays here is the post-pass's MP4, never
                                the .ts — that is MPEG-2 video with ARIB audio
                                and no browser will open it. So a row is
                                watchable only once post_state says 'done',
                                and until then the honest thing to show is
                                what it is waiting for. */}
                            {isPlayable(r) ? (
                              <button
                                onClick={() => watch(r)}
                                className={`btn ${watchingId === r.id ? "btn-primary" : ""}`}
                              >
                                ▶ Watch
                              </button>
                            ) : r.post_state === "pending" || r.post_state === "running" ? (
                              <span className="px-1 font-mono text-[11px] text-dim">
                                converting…
                              </span>
                            ) : r.state === "done" ? (
                              // 'skipped' — recorded before the post-pass
                              // existed — or 'failed'. This is the only route
                              // from such a row to a picture in a browser.
                              <button
                                onClick={() => void act(r.id, () => postprocessRecording(r.id))}
                                disabled={busyId === r.id}
                                className="btn"
                                title="Transcode to MP4 and write the subtitle sidecars"
                              >
                                Convert
                              </button>
                            ) : null}
                            {/* The recording's own file — the .ts as it came
                                off the air, or the MP4 that replaced it where
                                the box deletes the source after transcoding.
                                Either way it is what mpv, VLC and an archive
                                want (the endpoint honours Range). */}
                            <a href={recordingFileUrl(r.id)} download className="btn">
                              Download
                            </a>
                          </>
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
