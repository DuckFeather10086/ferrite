"use client";

import { useMemo, useState } from "react";
import { ScanChannels } from "@/components/ScanChannels";
import {
  createSchedule,
  displayName,
  epgEnd,
  fmtTime,
  isAiring,
  useChannelIndex,
  useEPG,
  useMinuteTick,
  type EPGEvent,
} from "@/lib/api";

export default function GuidePage() {
  const index = useChannelIndex();
  const [sel, setSel] = useState("");
  const selected = sel ? index.byName.get(sel) : undefined;
  const { data: events, isLoading, mutate } = useEPG(selected?.service_id);
  const now = useMinuteTick();

  // The window spans midnight more often than not, and a bare "19:00" with
  // no day is how you book tomorrow's programme by accident.
  const days = useMemo(() => groupByDay(events ?? []), [events]);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <select value={sel} onChange={(e) => setSel(e.target.value)} className="field">
          <option value="">All channels</option>
          {index.channels.map((c) => (
            // Label from display_name, request by name — this select used
            // to show the raw key, which for a legacy record is mojibake.
            <option key={c.name} value={c.name}>
              {displayName(c)}
            </option>
          ))}
        </select>
        <button onClick={() => mutate()} className="btn">
          Refresh
        </button>
        <span className="font-mono text-[11px] text-faint">
          {events?.length ?? 0} events · next 12h
        </span>
        {/* Pushed right: rebuilding the channel list is the rarest thing
            on this page and the most disruptive, so it sits away from the
            controls used every visit. */}
        <div className="ml-auto">
          <ScanChannels />
        </div>
      </div>

      {isLoading && <p className="text-sm text-dim">Loading guide…</p>}

      {!isLoading && !days.length && (
        <p className="text-sm text-dim">
          Nothing in the guide for this window. EPG is ingested per mux in the
          background, so a channel can stay empty until its transport stream has
          been scanned.
        </p>
      )}

      {days.map(([day, rows]) => (
        <section key={day} className="flex flex-col">
          <h2 className="eyebrow sticky top-0 bg-canvas py-1.5">{day}</h2>
          <div className="flex flex-col divide-y divide-line border-y border-line">
            {rows.map((e) => (
              <GuideRow
                key={`${e.service_id}-${e.event_id}-${e.start}`}
                event={e}
                channelName={index.byServiceId.get(e.service_id)?.name}
                channelLabel={index.labelForServiceId(e.service_id)}
                showChannel={!selected}
                airing={isAiring(e, now)}
              />
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

function GuideRow({
  event: e,
  channelName,
  channelLabel,
  showChannel,
  airing,
}: {
  event: EPGEvent;
  channelName?: string;
  channelLabel: string;
  showChannel: boolean;
  airing: boolean;
}) {
  const [state, setState] = useState<"idle" | "saving" | "booked">("idle");
  const [err, setErr] = useState<string | null>(null);

  const book = async () => {
    if (!channelName) return;
    setState("saving");
    setErr(null);
    try {
      await createSchedule({
        channel: channelName,
        service_id: e.service_id,
        start: e.start,
        end: epgEnd(e),
      });
      setState("booked");
    } catch (x) {
      setErr(x instanceof Error ? x.message : String(x));
      setState("idle");
    }
  };

  return (
    <div className={`group flex items-baseline gap-3 px-2 py-1.5 ${airing ? "bg-panel" : ""}`}>
      <span className="w-2 shrink-0 text-[10px] leading-none text-rec" aria-hidden>
        {airing ? "●" : ""}
      </span>
      <span className="shrink-0 font-mono text-[11px] text-dim tnum">
        {fmtTime(e.start)}–{fmtTime(epgEnd(e))}
      </span>
      {showChannel && (
        <span className="w-28 shrink-0 truncate text-[11px] text-faint">{channelLabel}</span>
      )}
      <div className="min-w-0 flex-1">
        <p className={`truncate text-[13px] ${airing ? "text-fg" : ""}`}>{e.title}</p>
        {e.synopsis && <p className="truncate text-[11px] text-faint">{e.synopsis}</p>}
        {err && <p className="text-[11px] text-rec">{err}</p>}
      </div>
      {/* A guide row already knows the channel, the start and the end — the
          three things the schedule form otherwise asks you to retype. Not
          offered for a service that is not in channels.json: the recorder
          takes a channel name, and there is none to send. */}
      <button
        onClick={book}
        disabled={!channelName || state !== "idle"}
        title={channelName ? "Schedule this programme" : "Channel not in channels.json"}
        // A booked row keeps its confirmation visible; an unbooked one is
        // revealed by hovering the row (see .row-action).
        className={`btn shrink-0 ${state === "booked" ? "" : "row-action"}`}
      >
        {state === "booked" ? "✓ Booked" : state === "saving" ? "…" : "Book"}
      </button>
    </div>
  );
}

// Events bucketed by local calendar day, in chronological order. The API
// returns them sorted by start; a service filter does not change that.
function groupByDay(events: EPGEvent[]): [string, EPGEvent[]][] {
  const days = new Map<string, EPGEvent[]>();
  for (const e of events) {
    const d = new Date(e.start);
    const key = d.toLocaleDateString("ja-JP", {
      month: "short",
      day: "numeric",
      weekday: "short",
    });
    const arr = days.get(key) ?? [];
    arr.push(e);
    days.set(key, arr);
  }
  return [...days.entries()];
}
