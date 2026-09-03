"use client";

import "./globals.css";
import { Header } from "@/components/Header";
import { Problems } from "@/components/Problems";

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>ferrite</title>
      </head>
      <body className="min-h-screen">
        <Header />
        {/* Above the page and below the nav: true of every tab, so it is
            not any one page's to draw — and drawn before the content it
            is warning you not to trust. Renders nothing on a healthy box. */}
        <Problems />
        <main className="p-4">{children}</main>
      </body>
    </html>
  );
}
