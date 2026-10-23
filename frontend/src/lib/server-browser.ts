export type BrowserTab = {
  id: string;
  title: string;
  url: string;
  can_back: boolean;
  can_forward: boolean;
  dialog?: { type: string; message: string; default_prompt?: string };
};

export type BrowserSession = {
  id: string;
  tabs: BrowserTab[];
  active_tab_id?: string;
  width: number;
  height: number;
  expires_at?: string;
};

export type BrowserStatus = {
  available: boolean;
  reason?: string;
  engine?: string;
  sessions: BrowserSession[];
};

/** Address resolution happens locally; only the server browser visits the result. */
export function browserAddress(value: string): string {
  const text = value.trim();
  if (!text) return "about:blank";
  if (/^about:blank$/i.test(text)) return "about:blank";
  if (
    /^[a-z][a-z0-9+.-]*:/i.test(text) &&
    !/^[^\s/:]+:\d+(?:[/\?#]|$)/.test(text)
  ) {
    const url = new URL(text);
    if (
      !["http:", "https:"].includes(url.protocol) ||
      url.username ||
      url.password
    )
      throw new Error("unsupported_browser_address");
    return url.href;
  }
  if (
    !/\s/.test(text) &&
    /^(?:localhost|\[[0-9a-f:]+\]|(?:[^/]+\.)+[^/]+)(?::\d+)?(?:[/\?#]|$)/i.test(
      text,
    )
  ) {
    const url = new URL(`https://${text}`);
    if (url.username || url.password)
      throw new Error("unsupported_browser_address");
    return url.href;
  }
  return `https://duckduckgo.com/?q=${encodeURIComponent(text)}`;
}

export function browserPoint(
  clientX: number,
  clientY: number,
  rect: { left: number; top: number; width: number; height: number },
  viewport: { width: number; height: number },
): { x: number; y: number } {
  return {
    x: Math.max(
      0,
      Math.min(
        viewport.width - 1,
        Math.round(
          ((clientX - rect.left) / Math.max(1, rect.width)) * viewport.width,
        ),
      ),
    ),
    y: Math.max(
      0,
      Math.min(
        viewport.height - 1,
        Math.round(
          ((clientY - rect.top) / Math.max(1, rect.height)) * viewport.height,
        ),
      ),
    ),
  };
}
