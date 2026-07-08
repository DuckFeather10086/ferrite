"use client";

import { useState } from "react";
import { useChannels, useSchedules, createSchedule, cancelSchedule, fmtDate, fmtTime, type Schedule } from "@/lib/api";

export default function SchedulesPage() {
  const { data: channels } = useChannels();
  const { data: schedules, isLoading, mutate } = useSchedules();
  const [showForm, setShowForm] = useState(false);
  const [err, setErr] = useState("");

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3 flex-wrap">
        <button onClick={() => { setShowForm(!showForm); setErr(""); }} className="btn btn-accent text-xs">
          {showForm ? "Cancel" : "+ New Schedule"}
        </button>
        <button onClick={() => mutate()} className="btn text-xs">Refresh</button>
      </div>

      {err && (
        <div className="text-sm px-3 py-2 rounded-lg" style={{ background: "#450a0a", color: "var(--color-danger)" }}>
          {err}
        </div>
      )}

      {showForm && (
        <ScheduleForm
          channels={channels ?? []}
          onCreated={() => { setShowForm(false); setErr(""); }}
          onError={setErr}
        />
      )}

      {isLoading && <p className="text-sm" style={{ color: "var(--color-text-muted)" }}>Loading...</p>}

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
                <td className="whitespace-nowrap font-medium">{s.channel}</td>
                <td className="whitespace-nowrap text-xs">{fmtDate(s.start)}</td>
                <td className="whitespace-nowrap font-mono text-xs">
                  {fmtTime(s.start)} – {fmtTime(s.end)}
                </td>
                <td><StateBadge state={s.state} /></td>
                <td>
                  {s.state === "pending" && (
                    <button
                      onClick={async () => { await cancelSchedule(s.id); }}
                      className="btn btn-danger text-xs"
                    >
                      Cancel
                    </button>
                  )}
                </td>
              </tr>
            ))}
            {schedules?.length === 0 && (
              <tr><td colSpan={5} className="text-center py-4" style={{ color: "var(--color-text-muted)" }}>No schedules.</td></tr>
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
    canceled: "var(--color-text-muted)",
  };
  return (
    <span className="badge" style={{ background: colors[state] + "22", color: colors[state] ?? "var(--color-text-muted)" }}>
      {state}
    </span>
  );
}

function ScheduleForm({
  channels,
  onCreated,
  onError,
}: {
  channels: { name: string; service_id: number }[];
  onCreated: () => void;
  onError: (msg: string) => void;
}) {
  const [channel, setChannel] = useState(channels[0]?.name ?? "");
  const [date, setDate] = useState(() => new Date().toISOString().slice(0, 10));
  const [startT, setStartT] = useState("19:00");
  const [endT, setEndT] = useState("20:00");
  const [saving, setSaving] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    const svc = channels.find((c) => c.name === channel);
    if (!svc) { onError("Invalid channel"); setSaving(false); return; }
    const start = new Date(date + "T" + startT + ":00+09:00").toISOString();
    const end = new Date(date + "T" + endT + ":00+09:00").toISOString();
    if (new Date(end) <= new Date(start)) { onError("End must be after start"); setSaving(false); return; }
    try {
      await createSchedule({ channel, service_id: svc.service_id, start, end });
      onCreated();
    } catch (err: any) {
      onError(err.message ?? String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      className="grid grid-cols-1 sm:grid-cols-4 gap-3 p-4 rounded-xl border"
      style={{ background: "var(--color-surface)", borderColor: "var(--color-border)" }}
    >
      <label className="text-xs" style={{ color: "var(--color-text-muted)" }}>
        Channel
        <select value={channel} onChange={(e) => setChannel(e.target.value)}
          className="block w-full mt-1 px-2 py-1.5 rounded-lg border text-sm"
          style={{ background: "var(--color-bg)", borderColor: "var(--color-border)", color: "var(--color-text)" }}
        >
          {channels.map((c) => (
            <option key={c.name} value={c.name}>{c.name}</option>
          ))}
        </select>
      </label>
      <label className="text-xs" style={{ color: "var(--color-text-muted)" }}>
        Date
        <input type="date" value={date} onChange={(e) => setDate(e.target.value)}
          className="block w-full mt-1 px-2 py-1.5 rounded-lg border text-sm"
          style={{ background: "var(--color-bg)", borderColor: "var(--color-border)", color: "var(--color-text)" }}
        />
      </label>
      <label className="text-xs" style={{ color: "var(--color-text-muted)" }}>
        Start
        <input type="time" value={startT} onChange={(e) => setStartT(e.target.value)}
          className="block w-full mt-1 px-2 py-1.5 rounded-lg border text-sm"
          style={{ background: "var(--color-bg)", borderColor: "var(--color-border)", color: "var(--color-text)" }}
        />
      </label>
      <label className="text-xs" style={{ color: "var(--color-text-muted)" }}>
        End
        <input type="time" value={endT} onChange={(e) => setEndT(e.target.value)}
          className="block w-full mt-1 px-2 py-1.5 rounded-lg border text-sm"
          style={{ background: "var(--color-bg)", borderColor: "var(--color-border)", color: "var(--color-text)" }}
        />
      </label>
      <button type="submit" disabled={saving} className="btn btn-accent sm:col-span-4 justify-self-end">
        {saving ? "Saving..." : "Create Schedule"}
      </button>
    </form>
  );
}
