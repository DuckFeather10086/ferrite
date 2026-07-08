"use client";

import { useEffect, useState } from "react";
import {
  createSchedule,
  epgEnd,
  fmtTime,
  useNextEvent,
  useNow,
  type EPGEvent,
} from "@/lib/api";

export type NowPlayingProps = {
  serviceId?: number;
  channelName: string | null;
  onStop: () => void;
};

// Now-playing card: title + synopsis + live progress bar (where the
// current programme sits in its start→end window) + "次の番組" preview
// + a one-click "预约録画" that creates a schedule from the current
// event's start/end. The progress bar ticks every second client-side
// (the daemon's /api/now refreshes every 60s, which is too coarse for
// a smooth indicator).
export function NowPlaying({ serviceId, channelName, onStop }: NowPlayingProps) {
  const { data: now } = useNow(serviceId);
  const { data: next } = useNextEvent(serviceId);
  const [progress, setProgress] = useState(0);
  const [booking, setBooking] = useState(false);
  const [booked, setBooked] = useState(false);
  const [bookErr, setBookErr] = useState<string | null>(null);

  // Reset the "booked" toast when the programme changes.
  useEffect(() => {
    setBooked(false);
    setBookErr(null);
  }, [now?.event_id]);

  useEffect(() => {
    if (!now) {
      setProgress(0);
      return;
    }
    const tick = () => {
      const start = new Date(now.start).getTime();
      const end = new Date(epgEnd(now)).getTime();
      const n = Date.now();
      const p = end > start ? Math.min(1, Math.max(0, (n - start) / (end - start))) : 0;
      setProgress(p);
    };
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [now]);

  if (!channelName) {
    return (
      <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>
        左のリストからチャンネルを選んでください。
      </p>
    );
  }

  const book = async () => {
    if (!now || !serviceId || !channelName) return;
    setBooking(true);
    setBookErr(null);
    try {
      await createSchedule({
        channel: channelName,
        service_id: serviceId,
        start: now.start,
        end: epgEnd(now),
      });
      setBooked(true);
    } catch (e) {
      setBookErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBooking(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-2">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            {now ? (
              <>
                <h2 className="text-base font-semibold leading-tight truncate">{now.title}</h2>
                <p className="text-xs mt-0.5" style={{ color: "var(--color-text-muted)" }}>
                  {fmtTime(now.start)} – {fmtTime(epgEnd(now))}
                </p>
              </>
            ) : (
              <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>
                番組情報がありません
              </p>
            )}
          </div>
          <div className="flex items-center gap-2 shrink-0">
            {now && (
              <button
                onClick={book}
                disabled={booking || booked}
                className={`btn text-xs ${booked ? "btn-accent" : ""}`}
                title="現在の番組を予約録画"
              >
                {booked ? "✓ 予約済み" : booking ? "予約中…" : "● 予約"}
              </button>
            )}
            <button onClick={onStop} className="btn btn-danger text-xs">
              停止
            </button>
          </div>
        </div>

        {now && (
          <>
            {/* Progress bar */}
            <div
              className="w-full h-1.5 rounded-full overflow-hidden"
              style={{ background: "var(--color-surface)" }}
            >
              <div
                className="h-full transition-[width] duration-1000 ease-linear"
                style={{ width: `${progress * 100}%`, background: "var(--color-accent)" }}
              />
            </div>

            {now.synopsis && (
              <p
                className="text-xs leading-relaxed mt-1"
                style={{ color: "var(--color-text-muted)" }}
              >
                {now.synopsis}
              </p>
            )}
          </>
        )}

        {bookErr && (
          <p className="text-xs" style={{ color: "var(--color-danger)" }}>
            予約失敗: {bookErr}
          </p>
        )}
      </div>

      <NextEvent next={next} />
    </div>
  );
}

function NextEvent({ next }: { next?: EPGEvent | null }) {
  if (!next) return null;
  return (
    <div
      className="flex items-center gap-2 px-3 py-2 rounded-lg text-xs"
      style={{ background: "var(--color-surface)", border: "1px solid var(--color-border)" }}
    >
      <span
        className="text-[10px] font-semibold uppercase tracking-wider"
        style={{ color: "var(--color-text-muted)" }}
      >
        次の番組
      </span>
      <span className="font-mono" style={{ color: "var(--color-text-muted)" }}>
        {fmtTime(next.start)}
      </span>
      <span className="truncate">{next.title}</span>
    </div>
  );
}
