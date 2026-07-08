"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import "./globals.css";
import { TunerStatus } from "@/components/TunerStatus";

const tabs = [
  { href: "/", label: "Live" },
  { href: "/guide", label: "Guide" },
  { href: "/schedules", label: "Schedules" },
  { href: "/recordings", label: "Recordings" },
];

export default function RootLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();

  return (
    <html lang="ja">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>ferrite</title>
      </head>
      <body className="min-h-screen">
        <header className="flex items-center justify-between px-4 py-3 border-b" style={{ borderColor: "var(--color-border)" }}>
          <div className="flex items-center gap-2 font-bold text-lg">
            📺 <span>ferrite</span>
          </div>
          <TunerStatus />
        </header>
        <nav className="flex gap-1 px-4 pt-3 pb-1 border-b" style={{ borderColor: "var(--color-border)" }}>
          {tabs.map((t) => {
            const active = t.href === "/" ? pathname === "/" : pathname.startsWith(t.href);
            return (
              <Link
                key={t.href}
                href={t.href}
                className={`px-4 py-2 rounded-t-lg text-sm font-medium transition-colors ${
                  active
                    ? "text-white border-b-2"
                    : "hover:text-white"
                }`}
                style={active ? {
                  background: "var(--color-surface)",
                  borderColor: "var(--color-accent)",
                  color: "var(--color-accent)",
                } : {
                  color: "var(--color-text-muted)",
                }}
              >
                {t.label}
              </Link>
            );
          })}
        </nav>

        <main className="p-4">{children}</main>
      </body>
    </html>
  );
}
