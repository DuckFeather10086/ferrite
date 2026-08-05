"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { TunerStatus } from "@/components/TunerStatus";
import { useStatus } from "@/lib/api";

// Same tagline the TUI's banner carries. Two front ends onto one daemon
// should introduce themselves the same way.
const TAGLINE = "ISDB-T live TV · guide · recordings";

const TABS = [
  { href: "/", label: "Live" },
  { href: "/guide", label: "Guide" },
  { href: "/schedules", label: "Schedules" },
  { href: "/recordings", label: "Recordings" },
];

export function Header() {
  const pathname = usePathname();
  const { data: status } = useStatus();

  // Version and uptime, but not the host: the TUI prints the host because
  // it is a remote that could be pointed anywhere, whereas here the
  // address bar above this header already says it.
  const daemon = [status?.version, status?.uptime && `up ${status.uptime}`]
    .filter(Boolean)
    .join(" · ");

  return (
    <header className="border-b border-line">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 px-4 pt-3 pb-2">
        <span className="font-mono text-[15px] font-bold tracking-tight">ferrite</span>
        <div className="min-w-0 flex-1">
          <p className="truncate text-xs text-dim">{TAGLINE}</p>
          {daemon && <p className="truncate font-mono text-[11px] text-faint tnum">{daemon}</p>}
        </div>
        <TunerStatus />
      </div>

      <nav className="flex gap-4 px-4">
        {TABS.map((t) => {
          const active = t.href === "/" ? pathname === "/" : pathname.startsWith(t.href);
          return (
            <Link
              key={t.href}
              href={t.href}
              // The active tab is marked by a rule sitting on the header's
              // own border, so switching tabs moves one line rather than
              // repainting a raised panel.
              className={`-mb-px border-b py-1.5 text-[13px] transition-colors ${
                active
                  ? "border-fg text-fg"
                  : "border-transparent text-dim hover:text-fg"
              }`}
            >
              {t.label}
            </Link>
          );
        })}
      </nav>
    </header>
  );
}
