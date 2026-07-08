"use client";

import { useMemo, useState } from "react";
import { useChannels, useEPG, fmtTime, epgEnd, type EPGEvent } from "@/lib/api";

export default function GuidePage() {
  const { data: channels } = useChannels();
  const [sel, setSel] = useState("");
  const selectedId = channels?.find((c) => c.name === sel)?.service_id;
  const { data: events, isLoading, mutate } = useEPG(selectedId);

  // Build service_id -> channel name map for DB-backed EPG events.
  const sidToName = useMemo(() => {
    const m: Record<number, string> = {};
    channels?.forEach((c) => { m[c.service_id] = c.name; });
    return m;
  }, [channels]);

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3 flex-wrap">
        <label className="text-sm" style={{ color: "var(--color-text-muted)" }}>
          Channel{" "}
          <select
            value={sel}
            onChange={(e) => setSel(e.target.value)}
            className="ml-1 px-2 py-1 rounded-lg text-sm border"
            style={{ background: "var(--color-surface)", borderColor: "var(--color-border)", color: "var(--color-text)" }}
          >
            <option value="">All</option>
            {channels?.map((c) => (
              <option key={c.name} value={c.name}>{c.name}</option>
            ))}
          </select>
        </label>
        <button onClick={() => mutate()} className="btn text-xs">Refresh</button>
      </div>

      {isLoading && <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>Loading EPG...</p>}

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Channel</th>
              <th>Title</th>
            </tr>
          </thead>
          <tbody>
            {events?.map((e) => (
              <tr key={`${e.service_id}-${e.event_id}`} className="hover:brightness-110" style={{ background: isNow(e) ? "#1e293b" : "transparent" }}>
                <td className="whitespace-nowrap font-mono text-xs">
                  {fmtTime(e.start)}
                </td>
                <td className="whitespace-nowrap text-xs" style={{ color: "var(--color-text-muted)" }}>
                  {sidToName[e.service_id] || `SID ${e.service_id}`}
                </td>
                <td>
                  <GuideTitle event={e} />
                </td>
              </tr>
            ))}
            {events?.length === 0 && (
              <tr><td colSpan={3} className="text-center py-4" style={{ color: "var(--color-text-muted)" }}>No events in this window.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function isNow(e: EPGEvent) {
  const n = Date.now();
  return new Date(e.start).getTime() <= n && new Date(epgEnd(e)).getTime() > n;
}

function GuideTitle({ event: e }: { event: EPGEvent }) {
  return (
    <div>
      <span className="font-medium">{e.title}</span>
      {e.synopsis && (
        <p className="text-xs mt-0.5" style={{ color: "var(--color-text-muted)", maxWidth: "42rem", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
          {e.synopsis}
        </p>
      )}
    </div>
  );
}
