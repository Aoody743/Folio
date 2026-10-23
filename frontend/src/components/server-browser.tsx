"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { action, APIError, intentKey, streamRequest } from "@/lib/api";
import { t as translate } from "@/lib/i18n";
import {
  BrowserSession,
  BrowserStatus,
  browserAddress,
  browserPoint,
} from "@/lib/server-browser";
import "./browser.css";
import { BrowserBookmarks } from "./browser-bookmarks";

type BrowserInput = {
  kind: "mouse" | "wheel" | "key" | "text";
  event?: string;
  x?: number;
  y?: number;
  button?: string;
  delta_x?: number;
  delta_y?: number;
  key?: string;
  code?: string;
  modifiers?: number;
  text?: string;
};
type InputResult = { resolve?: () => void; reject?: (error: Error) => void };

function errorMessage(error: unknown): string {
  if (error instanceof APIError) {
    if (error.code === "browser_sandbox_unavailable")
      return translate("浏览器沙箱未能启动，请检查服务器的浏览器安装与沙箱支持后重试。");
    if (error.code === "browser_unavailable")
      return translate("服务器浏览器暂不可用，请检查浏览器环境，或关闭当前会话后重新打开。");
    if (error.code === "browser_timeout")
      return translate("浏览器响应较慢，请稍后重连，或关闭会话后重新打开。");
    if (error.code === "browser_limit" || error.status === 429)
      return translate("浏览器资源正在使用中，请关闭多余会话或标签页，稍后重试。");
    if (error.code === "browser_last_tab")
      return translate("请保留一个标签页；如需结束会话，请使用“关闭浏览器”。");
    if (error.status === 401)
      return translate("登录已失效，请重新登录后继续浏览。");
    if (error.status === 403)
      return translate("当前账户没有此浏览器会话的操作权限。");
    if (error.status === 404)
      return translate("浏览器会话已结束，请重新打开浏览器。");
  }
  return error instanceof Error
    ? error.message
    : translate("浏览器操作失败，请重试。");
}

export function ServerBrowserView({ active = true }: { active?: boolean }) {
  const [status, setStatus] = useState<BrowserStatus>();
  const [session, setSession] = useState<BrowserSession>();
  const [tabID, setTabID] = useState("");
  const [address, setAddress] = useState("");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [checking, setChecking] = useState(false);
  const [sendingText, setSendingText] = useState(false);
  const [error, setError] = useState("");
  const [frameError, setFrameError] = useState("");
  const [frameURL, setFrameURL] = useState("");
  const [retry, setRetry] = useState(0);
  const [fullscreen, setFullscreen] = useState(false);
  const [viewport, setViewport] = useState(() =>
    typeof window !== "undefined" && window.innerWidth < 600 ? "390x844" : "1280x800",
  );
  const [showText, setShowText] = useState(false);
  const [promptText, setPromptText] = useState("");
  const root = useRef<HTMLElement>(null);
  const image = useRef<HTMLImageElement>(null);
  const surface = useRef<HTMLDivElement>(null);
  const currentSession = useRef(session);
  const currentTab = useRef(tabID);
  const currentImage = useRef("");
  const generation = useRef(0);
  const mounted = useRef(true);
  const queue = useRef<
    ({
      id: string;
      tab: string;
      input: BrowserInput;
      key: string;
    } & InputResult)[]
  >([]);
  const draining = useRef(false);
  const inputPaused = useRef(false);
  const busyRef = useRef(false);
  const sendingTextRef = useRef(false);
  const touch = useRef<{ x: number; y: number; moved: boolean } | undefined>(
    undefined,
  );
  const lastMove = useRef(0);
  const addressFocused = useRef(false);
  currentSession.current = session;
  currentTab.current = tabID;
  const clearQueue = useCallback(() => {
    const previous = queue.current;
    queue.current = [];
    for (const item of previous)
      item.reject?.(
        new Error(translate("输入未确认，已保留文字。请核对网页后重试。")),
      );
  }, []);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      generation.current++;
      clearQueue();
      if (currentImage.current) URL.revokeObjectURL(currentImage.current);
    };
  }, [clearQueue]);

  const accept = useCallback((next: BrowserSession) => {
    if (!mounted.current) return;
    if (currentSession.current && (currentSession.current.width !== next.width || currentSession.current.height !== next.height))
      setFrameURL("");
    setSession(next);
    setTabID((previous) =>
      next.tabs.some((tab) => tab.id === previous)
        ? previous
        : next.active_tab_id || next.tabs[0]?.id || "",
    );
  }, []);

  const check = useCallback(async () => {
    setChecking(true);
    setError("");
    const epoch = generation.current;
    try {
      const next = await action<BrowserStatus>("browser_status");
      if (!mounted.current || generation.current !== epoch) return;
      setStatus(next);
      if (!currentSession.current && next.sessions?.length)
        accept(next.sessions[0]);
    } catch (e) {
      if (mounted.current && generation.current === epoch)
        setError(errorMessage(e));
    } finally {
      if (mounted.current) setChecking(false);
    }
  }, [accept]);

  useEffect(() => {
    if (active) void check();
  }, [active, check]);

  const run = useCallback(
    async (name: string, input: Record<string, unknown>, update = true) => {
      if (busyRef.current) return;
      busyRef.current = true;
      setBusy(true);
      setError("");
      const epoch = ++generation.current;
      try {
        // Session responses contain browsing URLs. Keep them out of the durable
        // idempotency cache; these controls are never retried automatically.
        const result = await action<BrowserSession>(
          name,
          input,
          name === "browser_close" ? { idempotencyKey: intentKey() } : {},
        );
        if (mounted.current && generation.current === epoch && update)
          accept(result);
        return result;
      } catch (e) {
        if (
          name === "browser_close" &&
          e instanceof APIError &&
          e.status === 404
        )
          return {} as BrowserSession;
        if (mounted.current && generation.current === epoch)
          setError(errorMessage(e));
      } finally {
        busyRef.current = false;
        if (mounted.current) setBusy(false);
      }
    },
    [accept],
  );

  const fitWindow = useCallback(() => {
    const current = currentSession.current;
    const tab = currentTab.current;
    const target = surface.current;
    if (!current || !tab || !target) return;
    const rect = target.getBoundingClientRect();
    const width = Math.max(320, Math.min(1920, Math.round(rect.width)));
    const reserve = document.fullscreenElement ? 24 : 112;
    const height = Math.max(240, Math.min(1200, Math.floor(window.innerHeight - rect.top - reserve)));
    if (current.width !== width || current.height !== height)
      void run("browser_resize", { id: current.id, tab_id: tab, width, height });
  }, [run]);

  useEffect(() => {
    if (!active || !fullscreen) return;
    const timer = setTimeout(fitWindow, 100);
    window.addEventListener("resize", fitWindow);
    return () => { clearTimeout(timer); window.removeEventListener("resize", fitWindow); };
  }, [active, fullscreen, showText, fitWindow]);

  const drain = useCallback(async () => {
    if (draining.current || inputPaused.current) return;
    draining.current = true;
    try {
      while (queue.current.length && !inputPaused.current && mounted.current) {
        const item = queue.current[0];
        if (
          currentSession.current?.id !== item.id ||
          currentTab.current !== item.tab
        ) {
          queue.current.shift();
          item.reject?.(
            new Error(translate("输入未确认，已保留文字。请核对网页后重试。")),
          );
          continue;
        }
        try {
          await action(
            "browser_input",
            { id: item.id, tab_id: item.tab, ...item.input },
            { idempotencyKey: item.key },
          );
          if (queue.current[0] === item) queue.current.shift();
          item.resolve?.();
        } catch (e) {
          // Never replay clicks or key presses automatically after an uncertain response.
          const waiting = queue.current;
          queue.current = [];
          for (const pending of waiting)
            pending.reject?.(
              e instanceof Error ? e : new Error(errorMessage(e)),
            );
          inputPaused.current = true;
          setFrameError(errorMessage(e));
        }
      }
    } finally {
      draining.current = false;
    }
  }, []);

  const send = useCallback(
    (input: BrowserInput, result: InputResult = {}) => {
      const current = currentSession.current;
      const tab = currentTab.current;
      if (!current || !tab || inputPaused.current || !image.current || image.current.naturalWidth !== current.width || image.current.naturalHeight !== current.height) {
        result.reject?.(
          new Error(translate("输入未确认，已保留文字。请核对网页后重试。")),
        );
        return;
      }
      if (queue.current.length >= 48) {
        clearQueue();
        inputPaused.current = true;
        setFrameError(
          translate("输入发送较慢，已暂停。请核对页面后重新连接。"),
        );
        result.reject?.(
          new Error(translate("输入未确认，已保留文字。请核对网页后重试。")),
        );
        return;
      }
      queue.current.push({
        id: current.id,
        tab,
        input,
        key: intentKey(),
        ...result,
      });
      void drain();
    },
    [drain, clearQueue],
  );

  const id = session?.id;
  useEffect(() => {
    if (!active || !id || !tabID) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();
    setFrameError("");
    inputPaused.current = false;
    const frame = async () => {
      try {
        const response = await streamRequest(
          `/api/v1/browser/sessions/${encodeURIComponent(id)}/tabs/${encodeURIComponent(tabID)}/frame`,
          { signal: controller.signal },
        );
        if (!response.headers.get("Content-Type")?.startsWith("image/jpeg"))
          throw new Error(translate("浏览器画面无法读取，请重新连接。"));
        const blob = await response.blob();
        if (stopped) return;
        const next = URL.createObjectURL(blob);
        const previous = currentImage.current;
        currentImage.current = next;
        setFrameURL(next);
        if (previous) URL.revokeObjectURL(previous);
        timer = setTimeout(frame, 400);
      } catch (e) {
        if (stopped || controller.signal.aborted) return;
        inputPaused.current = true;
        setFrameError(errorMessage(e));
      }
    };
    void frame();
    return () => {
      stopped = true;
      controller.abort();
      clearTimeout(timer);
      clearQueue();
    };
  }, [active, id, tabID, session?.width, session?.height, retry, clearQueue]);

  useEffect(() => {
    if (!active || !id) return;
    let stopped = false;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const update = async () => {
      const epoch = generation.current;
      try {
        const result = await action<BrowserSession>(
          "browser_tabs",
          { id },
          { signal: controller.signal },
        );
        if (!stopped && !busyRef.current && generation.current === epoch && currentSession.current?.id === id) accept(result);
      } catch (e) {
        if (
          !stopped &&
          e instanceof APIError &&
          [401, 403, 404].includes(e.status)
        ) {
          setFrameError(errorMessage(e));
          inputPaused.current = true;
          return;
        }
      }
      if (!stopped) timer = setTimeout(update, 2500);
    };
    timer = setTimeout(update, 1200);
    return () => {
      stopped = true;
      clearTimeout(timer);
      controller.abort();
    };
  }, [active, id, accept]);

  const tab = session?.tabs.find((item) => item.id === tabID);
  const tabURL = tab?.url;
  const dialog = tab?.dialog;
  useEffect(() => {
    setPromptText(dialog?.default_prompt || "");
  }, [dialog?.type, dialog?.message, dialog?.default_prompt, tabID]);
  useEffect(() => {
    if (!addressFocused.current)
      setAddress(tabURL === "about:blank" ? "" : tabURL || "");
  }, [tabURL, tabID]);

  useEffect(() => {
    const changed = () =>
      setFullscreen(document.fullscreenElement === root.current);
    document.addEventListener("fullscreenchange", changed);
    return () => document.removeEventListener("fullscreenchange", changed);
  }, []);

  useEffect(() => {
    if (!active && document.fullscreenElement === root.current)
      void document.exitFullscreen().catch(() => {});
  }, [active]);

  useEffect(() => {
    const target = surface.current;
    if (!target || !id || !tabID || !active) return;
    const wheel = (event: WheelEvent) => {
      event.preventDefault();
      const rect = image.current?.getBoundingClientRect();
      const current = currentSession.current;
      if (!rect || !current) return;
      const point = browserPoint(event.clientX, event.clientY, rect, current);
      const scale =
        event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? current.height : 1;
      send({
        kind: "wheel",
        ...point,
        delta_x: Math.max(-2000, Math.min(2000, event.deltaX * scale)),
        delta_y: Math.max(-2000, Math.min(2000, event.deltaY * scale)),
      });
    };
    target.addEventListener("wheel", wheel, { passive: false });
    return () => target.removeEventListener("wheel", wheel);
  }, [active, id, tabID, send]);

  const point = (event: React.PointerEvent) => {
    const rect = image.current?.getBoundingClientRect();
    return rect && session
      ? browserPoint(event.clientX, event.clientY, rect, session)
      : undefined;
  };
  const modifiers = (event: React.KeyboardEvent) =>
    (event.altKey ? 1 : 0) |
    (event.ctrlKey ? 2 : 0) |
    (event.metaKey ? 4 : 0) |
    (event.shiftKey ? 8 : 0);
  const button = (value: number) =>
    value === 2 ? "right" : value === 1 ? "middle" : "left";
  const keyboard = (
    event: React.KeyboardEvent<HTMLDivElement>,
    release = false,
  ) => {
    if (
      event.nativeEvent.isComposing ||
      event.key === "Process" ||
      event.key === "Dead"
    )
      return;
    // Clipboard paste is handled by onPaste, so the local browser may provide its text.
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "v")
      return;
    event.preventDefault();
    if (
      !release &&
      event.key.length === 1 &&
      !event.ctrlKey &&
      !event.altKey &&
      !event.metaKey
    ) {
      send({ kind: "text", text: event.key });
    } else if (
      event.key.length !== 1 ||
      event.ctrlKey ||
      event.altKey ||
      event.metaKey
    ) {
      send({
        kind: "key",
        event: release ? "keyUp" : "keyDown",
        key: event.key,
        code: event.code,
        modifiers: modifiers(event),
      });
    }
  };

  const open = async () => {
    const [width, height] = viewport.split("x").map(Number);
    const opened = await run("browser_open", { width, height });
    if (opened) {
      setFrameError("");
      setRetry((value) => value + 1);
    }
  };
  const navigate = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!session || !tabID) return;
    try {
      const url = browserAddress(address);
      await run("browser_navigate", { id: session.id, tab_id: tabID, url });
      surface.current?.focus();
    } catch {
      setError(translate("请输入网址或搜索内容，仅支持 HTTP 和 HTTPS 网页。"));
    }
  };
  const close = async () => {
    if (!session) return;
    const result = await run("browser_close", { id: session.id }, false);
    if (result) {
      generation.current++;
      clearQueue();
      setSession(undefined);
      setTabID("");
      setAddress("");
      setText("");
      setFrameURL("");
      if (currentImage.current) URL.revokeObjectURL(currentImage.current);
      currentImage.current = "";
      setFrameError("");
      void check();
    }
  };

  const sendText = async (event: React.FormEvent) => {
    event.preventDefault();
    const current = currentSession.current;
    const tab = currentTab.current;
    if (
      !current ||
      !tab ||
      !text ||
      sendingTextRef.current ||
      inputPaused.current
    )
      return;
    const submitted = text;
    if (new TextEncoder().encode(submitted).byteLength > 16384) {
      setError(
        translate("一次最多发送 16 KiB 文字，请分段发送；当前内容已保留。"),
      );
      return;
    }
    sendingTextRef.current = true;
    setSendingText(true);
    try {
      await new Promise<void>((resolve, reject) =>
        send({ kind: "text", text: submitted }, { resolve, reject }),
      );
      if (
        mounted.current &&
        currentSession.current?.id === current.id &&
        currentTab.current === tab
      ) {
        setText((value) => (value === submitted ? "" : value));
        surface.current?.focus();
      }
    } catch (e) {
      if (mounted.current) {
        inputPaused.current = true;
        setFrameError(errorMessage(e));
      }
    } finally {
      sendingTextRef.current = false;
      if (mounted.current) setSendingText(false);
    }
  };

  return (
    <section
      ref={root}
      className={`server-browser ${fullscreen ? "is-fullscreen" : ""}`}
      aria-label={translate("服务器浏览器")}
    >
      <div className="browser-heading">
        <div>
          <h2>{translate("服务器浏览器")}</h2>
          <p className="muted">
            {translate("网页在服务器上打开，网络请求使用服务器的出口 IP。")}
          </p>
        </div>
        <div className="browser-heading-actions">
          {session && (
            <button type="button" disabled={busy} onClick={() => void close()}>
              {translate("关闭浏览器")}
            </button>
          )}
          {session && (
            <button
              type="button"
              onClick={() => {
                if (document.fullscreenElement)
                  void document
                    .exitFullscreen()
                    .catch((e) => setError(errorMessage(e)));
                else
                  void root.current
                    ?.requestFullscreen()
                    .catch((e) => setError(errorMessage(e)));
              }}
            >
              {fullscreen ? translate("退出全屏") : translate("全屏")}
            </button>
          )}
        </div>
      </div>
      {error && (
        <div className="browser-error" role="alert">
          {error}
        </div>
      )}
      {!session && (
        <div className="browser-start">
          <p>{translate("临时会话独立运行，关闭后清除本次浏览器资料。")}</p>
          {!status && <p role="status">{translate("正在检查服务器浏览器…")}</p>}
          {status && !status.available && (
            <div className="browser-missing" role="status">
              <strong>{translate("服务器浏览器暂不可用")}</strong>
              {status.reason && <details><summary>{translate("技术详情")}</summary><p>{status.reason}</p></details>}
              <p>
                {translate(
                  "请管理员在服务器安装 Chromium 或 Google Chrome，并允许面板服务以普通用户运行浏览器沙箱，然后重试。",
                )}
              </p>
            </div>
          )}
          <div className="browser-start-actions">
            <label>
              {translate("浏览器尺寸")}
              <select
                value={viewport}
                onChange={(e) => setViewport(e.target.value)}
              >
                <option value="1280x800">1280 × 800</option>
                <option value="1024x768">1024 × 768</option>
                <option value="390x844">390 × 844</option>
              </select>
            </label>
            <button
              type="button"
              disabled={busy || checking || !status?.available}
              onClick={() => void open()}
            >
              {busy ? translate("正在打开…") : translate("打开浏览器")}
            </button>
            <button
              type="button"
              disabled={checking || busy}
              onClick={() => void check()}
            >
              {translate("重新检查")}
            </button>
          </div>
        </div>
      )}
      {session && (
        <>
          <div
            className="browser-tabs"
            role="tablist"
            aria-label={translate("浏览器标签页")}
          >
            {session.tabs.map((item, index) => (
              <div
                key={item.id}
                className={`browser-tab ${item.id === tabID ? "selected" : ""}`}
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={item.id === tabID}
                  aria-controls="server-browser-screen"
                  title={item.url}
                  onClick={() => {
                    clearQueue();
                    setTabID(item.id);
                    setFrameURL("");
                    setError("");
                  }}
                >
                  {item.title ||
                    (item.url !== "about:blank"
                      ? item.url
                      : translate("新标签页"))}
                </button>
                <button
                  type="button"
                  className="browser-tab-close"
                  disabled={busy}
                  aria-label={translate("关闭标签页 {0}", [index + 1])}
                  onClick={() =>
                    void run("browser_tab_close", {
                      id: session.id,
                      tab_id: item.id,
                    })
                  }
                >
                  ×
                </button>
              </div>
            ))}
            <button
              type="button"
              className="browser-new-tab"
              disabled={busy}
              aria-label={translate("新建标签页")}
              onClick={() =>
                void run("browser_tab_open", {
                  id: session.id,
                  url: "about:blank",
                }).then((next) => {
                  if (next) {
                    setTabID(
                      next.active_tab_id ||
                        next.tabs[next.tabs.length - 1]?.id ||
                        "",
                    );
                    setAddress("");
                  }
                })
              }
            >
              ＋
            </button>
          </div>
          <div className="browser-toolbar">
            <div className="browser-history-controls">
              <button
                type="button"
                aria-label={translate("后退")}
                title={translate("后退")}
                disabled={busy || !tab?.can_back}
                onClick={() =>
                  void run("browser_history", {
                    id: session.id,
                    tab_id: tabID,
                    direction: "back",
                  })
                }
              >
                ←
              </button>
              <button
                type="button"
                aria-label={translate("前进")}
                title={translate("前进")}
                disabled={busy || !tab?.can_forward}
                onClick={() =>
                  void run("browser_history", {
                    id: session.id,
                    tab_id: tabID,
                    direction: "forward",
                  })
                }
              >
                →
              </button>
              <button
                type="button"
                aria-label={translate("刷新网页")}
                title={translate("刷新网页")}
                disabled={busy || !tabID}
                onClick={() =>
                  void run("browser_history", {
                    id: session.id,
                    tab_id: tabID,
                    direction: "reload",
                  })
                }
              >
                ↻
              </button>
              <button
                type="button"
                aria-label={translate("停止加载")}
                title={translate("停止加载")}
                disabled={!tabID}
                onClick={() =>
                  void run("browser_history", {
                    id: session.id,
                    tab_id: tabID,
                    direction: "stop",
                  })
                }
              >
                ×
              </button>
            </div>
            <form
              onSubmit={(e) => void navigate(e)}
              className="browser-address"
            >
              <input
                type="text"
                aria-label={translate("网址或搜索")}
                placeholder={translate("输入网址或搜索内容")}
                value={address}
                spellCheck={false}
                autoCapitalize="none"
                autoCorrect="off"
                autoComplete="off"
                onChange={(e) => setAddress(e.target.value)}
                onFocus={() => {
                  addressFocused.current = true;
                }}
                onBlur={() => {
                  addressFocused.current = false;
                }}
              />
              <button type="submit" disabled={busy || !tabID}>
                {translate("前往")}
              </button>
            </form>
          </div>
          <BrowserBookmarks currentURL={tabURL} onOpen={url=>{if(session && tabID)void run("browser_navigate",{id:session.id,tab_id:tabID,url});}}/>
          <div className="browser-view-tools">
            <button type="button" disabled={busy} onClick={fitWindow}>{translate("适应窗口")}</button>
            <label>
              {translate("浏览器尺寸")}
              <select
                value={`${session.width}x${session.height}`}
                disabled={busy}
                onChange={(event) => {
                  const [width, height] = event.target.value
                    .split("x")
                    .map(Number);
                  void run("browser_resize", {
                    id: session.id,
                    tab_id: tabID,
                    width,
                    height,
                  });
                }}
              >
                <option value="1280x800">1280 × 800</option>
                <option value="1024x768">1024 × 768</option>
                <option value="390x844">390 × 844</option>
                {!["1280x800", "1024x768", "390x844"].includes(
                  `${session.width}x${session.height}`,
                ) && (
                  <option value={`${session.width}x${session.height}`}>
                    {session.width} × {session.height}
                  </option>
                )}
              </select>
            </label>
            <button
              type="button"
              aria-expanded={showText}
              onClick={() => setShowText((value) => !value)}
            >
              {translate("输入文字")}
            </button>
            <span className="muted">
              {translate("点击画面后可使用键盘；手机可滑动画面滚动网页。")}
            </span>
          </div>
          {dialog && (
            <section className="browser-page-dialog" role="dialog" aria-label={translate("网页提示")}>
              <strong>{translate("网页提示")}</strong>
              <p>{dialog.message}</p>
              {dialog.type === "prompt" && (
                <label>{translate("回复内容")}<input value={promptText} maxLength={16000} onChange={(event) => setPromptText(event.target.value)} /></label>
              )}
              <div className="browser-heading-actions">
                <button disabled={busy} type="button" onClick={() => void run("browser_dialog", { id: session.id, tab_id: tabID, accept: true, prompt_text: promptText }).then((result) => { if (result) setRetry((value) => value + 1); })}>{translate("确定")}</button>
                {dialog.type !== "alert" && <button disabled={busy} type="button" onClick={() => void run("browser_dialog", { id: session.id, tab_id: tabID, accept: false }).then((result) => { if (result) setRetry((value) => value + 1); })}>{translate("取消")}</button>}
              </div>
            </section>
          )}
          {showText && (
            <form
              className="browser-text-entry"
              onSubmit={(event) => void sendText(event)}
            >
              <label htmlFor="server-browser-text">
                {translate("发送到网页当前输入框")}
              </label>
              <textarea
                id="server-browser-text"
                value={text}
                onChange={(event) => setText(event.target.value)}
                placeholder={translate("支持中文、粘贴和手机输入法")}
                rows={2}
                autoComplete="off"
              />
              <button
                type="submit"
                disabled={!text || Boolean(frameError) || sendingText}
              >
                {translate("发送文字")}
              </button>
            </form>
          )}
          {frameError && (
            <div className="browser-error" role="alert">
              <span>{frameError}</span>
              <button
                type="button"
                onClick={() => {
                  clearQueue();
                  setRetry((value) => value + 1);
                }}
              >
                {translate("重新连接")}
              </button>
            </div>
          )}
          <div
            ref={surface}
            id="server-browser-screen"
            role="application"
            aria-label={translate("远程网页画面")}
            tabIndex={0}
            className="browser-surface server-browser-viewport"
            onContextMenu={(event) => event.preventDefault()}
            onKeyDown={(event) => keyboard(event)}
            onKeyUp={(event) => keyboard(event, true)}
            onPaste={(event) => {
              event.preventDefault();
              const pasted = event.clipboardData.getData("text/plain");
              if (new TextEncoder().encode(pasted).byteLength > 16384) {
                setText(pasted);
                setShowText(true);
                setError(
                  translate(
                    "一次最多发送 16 KiB 文字，请分段发送；当前内容已保留。",
                  ),
                );
              } else if (pasted)
                send(
                  { kind: "text", text: pasted },
                  {
                    reject: () => {
                      if (mounted.current) {
                        setText(pasted);
                        setShowText(true);
                      }
                    },
                  },
                );
            }}
            onPointerDown={(event) => {
              const at = point(event);
              if (!at) return;
              event.preventDefault();
              surface.current?.focus();
              event.currentTarget.setPointerCapture(event.pointerId);
              if (event.pointerType === "touch")
                touch.current = {
                  x: event.clientX,
                  y: event.clientY,
                  moved: false,
                };
              else
                send({
                  kind: "mouse",
                  event: "mousePressed",
                  ...at,
                  button: button(event.button),
                });
            }}
            onPointerMove={(event) => {
              const at = point(event);
              if (!at) return;
              if (event.pointerType === "touch" && touch.current) {
                const deltaX = touch.current.x - event.clientX,
                  deltaY = touch.current.y - event.clientY;
                if (Math.abs(deltaX) + Math.abs(deltaY) > 4) {
                  touch.current.moved = true;
                  send({
                    kind: "wheel",
                    ...at,
                    delta_x:
                      deltaX *
                      (session.width /
                        Math.max(1, image.current?.clientWidth || 1)),
                    delta_y:
                      deltaY *
                      (session.height /
                        Math.max(1, image.current?.clientHeight || 1)),
                  });
                  touch.current.x = event.clientX;
                  touch.current.y = event.clientY;
                }
              } else if (performance.now() - lastMove.current > 250) {
                lastMove.current = performance.now();
                send({
                  kind: "mouse",
                  event: "mouseMoved",
                  ...at,
                  button: event.buttons ? button(event.button) : "none",
                });
              }
            }}
            onPointerUp={(event) => {
              const at = point(event);
              if (!at) return;
              if (event.pointerType === "touch") {
                if (!touch.current?.moved) {
                  send({
                    kind: "mouse",
                    event: "mousePressed",
                    ...at,
                    button: "left",
                  });
                  send({
                    kind: "mouse",
                    event: "mouseReleased",
                    ...at,
                    button: "left",
                  });
                }
                touch.current = undefined;
              } else
                send({
                  kind: "mouse",
                  event: "mouseReleased",
                  ...at,
                  button: button(event.button),
                });
            }}
            onPointerCancel={() => {
              touch.current = undefined;
            }}
          >
            {frameURL ? (
              <img
                ref={image}
                src={frameURL}
                alt={translate("服务器浏览器网页画面")}
                width={session.width}
                height={session.height}
                draggable={false}
              />
            ) : (
              <div className="browser-frame-loading" role="status">
                {tabID
                  ? translate("正在连接浏览器画面…")
                  : translate("新建标签页即可开始浏览。")}
              </div>
            )}
          </div>
          <p className="browser-session-note muted">{translate("面板模式暂不支持网页内上传与下载文件；请使用文件页传输服务器文件。")}</p>
          <p className="browser-session-note muted">
            {translate(
              "离开此页会暂停画面。关闭浏览器或会话到期后，会清除本次浏览数据。",
            )}
          </p>
        </>
      )}
    </section>
  );
}
