import { defineTheme } from "@astryxdesign/core/theme";
import { neutralTheme } from "@astryxdesign/theme-neutral/built";
import { folioIcons } from "./icons";

/** Folio's single component contract. Palette and font choices stay live. */
export const folioTheme = defineTheme({
  name: "folio",
  extends: neutralTheme,
  icons: folioIcons,
  typography: {
    scale: { base: 15, ratio: 1.18 },
    body: { family: "var(--folio-font)", fallbacks: "system-ui, sans-serif" },
    heading: { family: "var(--folio-font)", fallbacks: "system-ui, sans-serif", weights: { 1: "semibold", 2: "semibold", 3: "medium", 4: "medium" } },
    code: { family: "ui-monospace", fallbacks: "SFMono-Regular, Consolas, monospace" },
  },
  tokens: {
    "--color-background-body": "var(--bg)",
    "--color-background-card": "var(--surface)",
    "--color-background-surface": "var(--surface)",
    "--color-background-popover": "var(--raised)",
    "--color-background-muted": "var(--bg)",
    "--color-accent": "var(--accent)",
    "--color-accent-muted": "var(--soft)",
    "--color-on-accent": "var(--on-accent)",
    "--color-text-primary": "var(--text)",
    "--color-text-secondary": "var(--muted)",
    "--color-text-accent": "var(--accent)",
    "--color-icon-primary": "var(--text)",
    "--color-icon-secondary": "var(--muted)",
    "--color-icon-accent": "var(--accent)",
    "--color-border": "var(--border)",
    "--color-border-emphasized": "var(--border)",
    "--color-success": "var(--success)",
    "--color-warning": "var(--warning)",
    "--color-error": "var(--danger)",
    "--radius-container": "12px",
    "--radius-element": "8px",
    "--size-element-md": "var(--folio-control-height, 40px)",
    "--text-supporting-size": "13px",
  },
  components: {
    button: { base: { borderRadius: "8px", fontWeight: "500" } },
    card: { base: { padding: "20px", borderRadius: "12px" } },
    tab: { base: { fontWeight: "500", borderRadius: "6px" } },
  },
});
