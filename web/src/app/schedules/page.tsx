"use client";

import { useState } from "react";
import { StateBadge } from "@/components/StateBadge";
import {
  cancelSchedule,
  createSchedule,
  displayName,
  fmtDate,
  fmtTime,
  useChannelIndex,
  useSchedules,
  type Channel,
} from "@/lib/api";

export default function SchedulesPage() {
  const index = useChannelIndex();
  const { data: schedules, isLoading, mutate } = useSchedules();
  const [showForm, setShowForm] = useState(false);
  const [err, setErr] = useState("");

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <button
          onClick={() => {
            setShowForm(!showForm);
            setErr("");
          }}
          className={showForm ? "btn" : "btn btn-primary"}
        >
          {showForm ? "Close" : "+ New schedule"}
        </button>
        <button onClick={() => mutate()} className="btn">
          Refresh
        </button>
        <span className="font-mono text-[11px] text-faint">
          Or book a programme straight from the Guide.
        </span>
      </div>

      {err && <p className="text-sm text-rec">{err}</p>}

      {showForm && (
        <ScheduleForm
          channels={index.channels}
          onCreated={() => {
            setShowForm(false);
            setErr("");
          }}
          onError={setErr}
        />
      )}

      {isLoading && <p className="text-sm text-dim">Loading…</p>}

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Channel</th>
              <th>Date</th>
              <th>Start – End</th>
              <th>State</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {schedules?.map((s) => (
              <tr key={s.id}>
                {/* s.channel is the canonical key the recorder takes; what
                    a person reads is the daemon's label for it. */}
                <td className="whitespace-nowrap">{index.label(s.channel)}</td>
                <td className="whitespace-nowrap text-xs text-dim">{fmtDate(s.start)}</td>
                <td
                  className="whitespace-nowrap font-mono text-xs tnum"
                  title={`lead ${s.lead} · trail ${s.trail}`}
                >
                  {fmtTime(s.start)} – {fmtTime(s.end)}
                </td>
                <td>
                  <StateBadge state={s.state} />
                </td>
                <td className="text-right">
                  {s.state === "pending" && (
                    <button
                      onClick={() => void cancelSchedule(s.id)}
                      className="btn btn-danger"
                    >
                      Cancel
                    </button>
                  )}
                </td>
              </tr>
            ))}
            {schedules?.length === 0 && (
              <tr>
                <td colSpan={5} className="py-6 text-center text-dim">
                  No schedules.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function ScheduleForm({
  channels,
  onCreated,
  onError,
}: {
  channels: Channel[];
  onCreated: () => void;
  onError: (msg: string) => void;
}) {
  const [channel, setChannel] = useState("");
  const [date, setDate] = useState(() => new Date().toISOString().slice(0, 10));
  const [startT, setStartT] = useState("19:00");
  const [endT, setEndT] = useState("20:00");
  const [saving, setSaving] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const ch = channels.find((c) => c.name === (channel || channels[0]?.name));
    if (!ch) {
      onError("Pick a channel");
      return;
    }
    // Times are entered in JST, not in the browser's zone. Broadcast
    // schedules are published in JST and the guide displays them that way,
    // so a viewer reading "19:00" off the guide on a laptop set to another
    // zone must get that programme and not one nine hours away.
    const start = new Date(`${date}T${startT}:00+09:00`);
    const end = new Date(`${date}T${endT}:00+09:00`);
    if (end <= start) {
      onError("End must be after start");
      return;
    }
    setSaving(true);
    try {
      await createSchedule({
        channel: ch.name,
        service_id: ch.service_id,
        start: start.toISOString(),
        end: end.toISOString(),
      });
      onCreated();
    } catch (x) {
      onError(x instanceof Error ? x.message : String(x));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form onSubmit={submit} className="panel grid grid-cols-1 gap-3 p-3 sm:grid-cols-4">
      <label className="flex flex-col gap-1">
        <span className="eyebrow">channel</span>
        <select
          value={channel || channels[0]?.name || ""}
          onChange={(e) => setChannel(e.target.value)}
          className="field"
        >
          {channels.map((c) => (
            <option key={c.name} value={c.name}>
              {displayName(c)}
            </option>
          ))}
        </select>
      </label>
      <label className="flex flex-col gap-1">
        <span className="eyebrow">date (jst)</span>
        <input type="date" value={date} onChange={(e) => setDate(e.target.value)} className="field" />
      </label>
      <label className="flex flex-col gap-1">
        <span className="eyebrow">start (jst)</span>
        <input type="time" value={startT} onChange={(e) => setStartT(e.target.value)} className="field" />
      </label>
      <label className="flex flex-col gap-1">
        <span className="eyebrow">end (jst)</span>
        <input type="time" value={endT} onChange={(e) => setEndT(e.target.value)} className="field" />
      </label>
      <div className="sm:col-span-4 sm:justify-self-end">
        <button type="submit" disabled={saving} className="btn btn-primary">
          {saving ? "Saving…" : "Create schedule"}
        </button>
      </div>
    </form>
  );
}
