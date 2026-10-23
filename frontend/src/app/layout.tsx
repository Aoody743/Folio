import type { Metadata, Viewport } from "next";
import "./layers.css";
import "@astryxdesign/core/reset.css";
import "@astryxdesign/core/astryx.css";
import "@/themes/folio.css";
import "./font-subsets.css";
import "./globals.css";
import "./information.css";
import "./folio.css";
export const metadata: Metadata = {
  title: "折纸面板",
  description: "AI 时代的 Linux 服务器可视化工具",
  icons: { icon: "/folio-logo.png" },
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
