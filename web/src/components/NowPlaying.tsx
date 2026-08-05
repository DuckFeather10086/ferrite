"use client";

import { useEffect, useState } from "react";
import {
  createSchedule,
  epgEnd,
  fmtTime,
  recordNow,
  stopRecording,
  useNextEvent,
  useNow,
  useRecordings,
  type EPGEvent,
} from "@/lib/api";

export type NowPlayingProps = {
  serviceId?: number;
  channelName: string | null;
  // Whether this page is watching. Recording is independent of it, so the
  // panel is useful either way — but there is nothing for Stop to stop.
  playing?: boolean;
  onStop: () => void;
};

// What is on this channel, and the three things you can do about it:
// record it right now, book the programme currently airing, or stop
// watching. The daemon's /api/now only refreshes every 60s, so the
// progress bar is ticked client-side from the event's own start and end.
export function NowPlaying({ serviceId, channelName, playing, onStop }: NowPlayingProps) {
  const { data: now } = useNow(serviceId);
  const { data: next } = useNextEvent(serviceId);
  const { data: recordings } = useRecordings();
  const [progress, setProgress] = useState(0);
  const [booking, setBooking] = useState(false);
  const [booked, setBooked] = useState(false);
  const [recBusy, setRecBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // A "Booked" label belongs to one programme; clear it when the
  // programme changes under us.
  useEffect(() => {
    setBooked(false);
    setErr(null);
  }, [now?.event_id]);

  useEffect(() => {
    if (!now) {
      setProgress(0);
      return;
    }
    const tick = () => {
      const start = new Date(now.start).getTime();
      const end = new Date(epgEnd(now)).getTime();
      const p = end > start ? (Date.now() - start) / (end - start) : 0;
      setProgress(Math.min(1, Math.max(0, p)));
    };
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [now]);

  if (!channelName) {
    return <p className="text-sm text-dim">Pick a channel to start watching.</p>;
  }

  // The ad-hoc recording of this channel, if one is running — its row id
  // is the handle to stop it.
  const running = recordings?.find((r) => r.channel === channelName && r.state === "recording");

  const book = async () => {
    if (!now || !serviceId) return;
    setBooking(true);
    setErr(null);
    try {
      await createSchedule({
        channel: channelName,
        service_id: serviceId,
        start: now.start,
        end: epgEnd(now),
      });
      setBooked(true);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBooking(false);
    }
  };

  const toggleRecord = async () => {
    setRecBusy(true);
    setErr(null);
    try {
      if (running) await stopRecording(running.id);
      else await recordNow(channelName);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setRecBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          {now ? (
            <>
              <h2 className="truncate text-[15px] font-medium leading-tight">{now.title}</h2>
              <p className="mt-0.5 font-mono text-[11px] text-dim tnum">
                {fmtTime(now.start)}–{fmtTime(epgEnd(now))}
              </p>
            </>
          ) : (
            // Normal for a data service or a subchannel the broadcaster
            // only publishes present/following for — not an error.
            <p className="text-sm text-dim">No guide data for this channel.</p>
          )}
        </div>

        <div className="flex shrink-0 items-center gap-1.5">
          <button
            onClick={toggleRecord}
            disabled={recBusy}
            className={`btn ${running ? "btn-danger" : ""}`}
            title={running ? "Finish this recording now" : "Start recording this channel"}
          >
            {running ? "■ Stop rec" : "● Record"}
          </button>
          {now && (
            <button onClick={book} disabled={booking || booked} className="btn" title="Schedule the programme now airing">
              {booked ? "✓ Booked" : booking ? "Booking…" : "Book"}
            </button>
          )}
          <button onClick={onStop} disabled={!playing} className="btn">
            Stop
          </button>
        </div>
      </div>

      {now && (
        <>
          <div className="h-px w-full bg-line">
            <div
              className="h-px bg-fg transition-[width] duration-1000 ease-linear"
              style={{ width: `${progress * 100}%` }}
            />
          </div>
          {now.synopsis && <p className="text-xs leading-relaxed text-dim">{now.synopsis}</p>}
        </>
      )}

      {running && (
        <p className="font-mono text-[11px] text-rec">
          ● Recording to row {running.id}
          {running.title ? ` · ${running.title}` : ""}
        </p>
      )}

      {err && <p className="text-xs text-rec">{err}</p>}

      <NextEvent next={next} />
    </div>
  );
}

function NextEvent({ next }: { next?: EPGEvent | null }) {
  if (!next) return null;
  return (
    <div className="flex items-baseline gap-2 border-t border-line pt-2 text-xs">
      <span className="eyebrow">next</span>
      <span className="font-mono text-dim tnum">{fmtTime(next.start)}</span>
      <span className="truncate text-dim">{next.title}</span>
    </div>
  );
}
