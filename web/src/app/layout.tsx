"use client";

import "./globals.css";
import { Header } from "@/components/Header";

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
        <main className="p-4">{children}</main>
      </body>
    </html>
  );
}
