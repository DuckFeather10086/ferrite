"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { scanChannels, useScanStatus, type ScanProgress } from "@/lib/api";

// Sweep the terrestrial band and rebuild channels.json.
//
// This is the control that makes the daemon installable by someone who is
// not its author: the channel list in the repo is a hand-kept frequency
// table for one aerial in Kantō, and without this the first thing a new
// install asks of you is the one thing you cannot supply.
//
// It is deliberately not a one-click affair. A sweep is fifty transports
// at up to twenty seconds each and it owns the tuner throughout — so the
// button confirms, the progress is spelled out while it runs, and the
// copy says what it will cost. Live playback and recordings outrank it
// and will take the adapter back, which ends the sweep where it stands;
// what it had already found is on disk.
export function ScanChannels() {
  const { data: status, mutate: refreshStatus } = useScanStatus();
  const [progress, setProgress] = useState<ScanProgress | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const abortRef = useRef<AbortController | null>(null);

  // Detach the reader on unmount. The scan itself carries on — the daemon
  // does not tie it to the request — so this only stops us reading.
  useEffect(() => () => abortRef.current?.abort(), []);

  const start = useCallback(async () => {
    setConfirming(false);
    setRunning(true);
    setError(null);
    setProgress(null);
    const ac = new AbortController();
    abortRef.current = ac;
    try {
      await scanChannels(setProgress, ac.signal);
    } catch (e) {
      if (!ac.signal.aborted) setError(e instanceof Error ? e.message : String(e));
    } finally {
      setRunning(false);
      abortRef.current = null;
      void refreshStatus();
    }
  }, [refreshStatus]);

  if (status && !status.available) return null;

  // A scan started from another tab (or before this page was opened) is
  // still a scan; say so rather than offering to start a second one, which
  // the daemon would refuse.
  const busyElsewhere = !running && Boolean(status?.running);
  const pct = progress?.total ? Math.round((progress.done / progress.total) * 100) : 0;

  return (
    <div className="flex flex-wrap items-center gap-2">
      {!running && !busyElsewhere && !confirming && (
        <button onClick={() => setConfirming(true)} className="btn" title="Rebuild channels.json from the air">
          Scan channels
        </button>
      )}

      {confirming && (
        <>
          <span className="text-[11px] text-dim">
            Sweeps UHF 13–62. Takes several minutes and holds the tuner — live
            playback or a recording will interrupt it. Names you have edited are
            kept.
          </span>
          <button onClick={start} className="btn">
            Start
          </button>
          <button onClick={() => setConfirming(false)} className="btn">
            Cancel
          </button>
        </>
      )}

      {(running || busyElsewhere) && (
        <>
          <div className="h-1 w-32 overflow-hidden rounded-full bg-panel">
            <div className="h-full bg-fg transition-[width]" style={{ width: `${pct}%` }} />
          </div>
          <span className="font-mono text-[11px] text-dim tnum">
            {progress
              ? `ch ${progress.physical} · ${progress.done}/${progress.total} · ${progress.services} services`
              : busyElsewhere
                ? "scanning (started elsewhere)…"
                : "starting…"}
          </span>
        </>
      )}

      {progress?.finished && !running && (
        <span className="font-mono text-[11px] text-dim tnum">
          {progress.error
            ? `stopped after ${progress.done}/${progress.total} — ${progress.error}`
            : `found ${progress.services} services`}
        </span>
      )}

      {error && <span className="text-[11px] text-rec">{error}</span>}
    </div>
  );
}
