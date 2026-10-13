import type { Metadata, Viewport } from "next";
import "@astryxdesign/core/reset.css";
import "@astryxdesign/core/astryx.css";
import "@astryxdesign/theme-neutral/theme.css";
import "./globals.css";
export const metadata: Metadata = {
  title: "PowerLinux",
  description: "轻量 Linux 管理面板",
  icons: { icon: "/icon.svg" },
};
export const viewport: Viewport = { width: "device-width", initialScale: 1 };
export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="zh-CN" data-palette="moon" suppressHydrationWarning>
      <head>
        <script src="/theme.js" />
      </head>
      <body>{children}</body>
    </html>
  );
}
