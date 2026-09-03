"use client";

import { useStatus } from "@/lib/api";

// The banner for a box that cannot do its job.
//
// It exists because of a specific failure. `cargo clean` removed the three
// programs the daemon spawns, and the daemon carried on: this UI drew, every
// page worked, the tuner status said "idle" — which is exactly what an idle,
// healthy box says — and the only symptom anywhere was that the guide stopped
// getting newer. The truth was in the journal, six hours apart, addressed to
// the EPG refresher. It took three days.
//
// So this is deliberately the one piece of chrome that cannot be missed and
// cannot be dismissed. There is no close button: the condition is a fact about
// the box, not a notification, and it goes away when the box is fixed. It sits
// above the page rather than inside one because it is true of every page —
// the Live player, the Guide and the Recordings list are all lying by omission
// while it is up.
export function Problems() {
  const { data: status } = useStatus();
  const problems = status?.problems ?? [];
  if (problems.length === 0) return null;

  // Red when the box is not a television at all, amber when it has lost a
  // feature — the same split the daemon makes, and the reason the daemon
  // sends `fatal` rather than leaving a client to guess from the setting
  // name which of the five programs matters most.
  const fatal = problems.some((p) => p.fatal);
  const tone = fatal ? "text-rec" : "text-warn";

  return (
    <div
      role="alert"
      className={`border-b border-line bg-panel px-4 py-3 ${fatal ? "border-l-2 border-l-rec" : "border-l-2 border-l-warn"}`}
    >
      <p className={`text-[13px] font-medium ${tone}`}>
        {fatal
          ? "This box cannot tune — a program the daemon needs is missing"
          : "Running with reduced function — a program the daemon needs is missing"}
      </p>

      <ul className="mt-2 space-y-1">
        {problems.map((p) => (
          <li key={p.setting} className="font-mono text-[11px] leading-relaxed">
            <span className={p.fatal ? "text-rec" : "text-warn"}>{p.setting}</span>
            <span className="text-faint"> = </span>
            {/* The path breaks anywhere: it is long, absolute, and on a
                phone it is the whole reason the message is actionable. */}
            <span className="break-all text-dim">{p.path}</span>
            <span className="text-faint"> — {p.error}</span>
            <span className="block pl-0 text-faint">breaks: {p.breaks}</span>
          </li>
        ))}
      </ul>

      <p className="mt-2 text-[11px] text-faint">
        Check the paths in the daemon&apos;s config, then restart it. Reinstalling
        the release (<span className="font-mono">scripts/install.sh</span>) puts
        every program back where the config expects it.
      </p>
    </div>
  );
}
