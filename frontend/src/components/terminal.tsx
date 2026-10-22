"use client";
import { t as translate } from "@/lib/i18n";
import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import {
  action,
  APIError,
  Capability,
  fullDate,
  intentKey,
  list,
} from "@/lib/api";
import { Page, TerminalSession, TerminalTarget } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { ActionBrowser } from "./actions";
import { Icon } from "./icon";
import { Unavailable } from "./resource-ui";
type ReadResult = TerminalSession & {
  frames: { sequence: number; data: string }[];
  next: number;
  exited: boolean;
  exit_code: number;
  truncated: boolean;
};
function encode(bytes: Uint8Array): string {
  let text = "";
  for (const byte of bytes) text += String.fromCharCode(byte);
  return btoa(text);
}
function decode(text: string): Uint8Array {
  return Uint8Array.from(atob(text), (v) => v.charCodeAt(0));
}
export function TerminalView({
  capabilities,
  onExecuted,
  active = true,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const targets = useResource<TerminalTarget[] | Page<TerminalTarget>>(
    "terminal_targets_list",
    {},
    active && has("terminal_targets_list"),
  );
  const commands = useResource<
    { id: string; title: string; command: string }[]
  >("terminal_commands_list", {}, active && has("terminal_commands_list"));
  const [target, setTarget] = useState("");
  const [existingID, setExistingID] = useState("");
  const [session, setSession] = useState<TerminalSession>();
  const [error, setError] = useState("");
  const [readError, setReadError] = useState("");
  const [connectionLost, setConnectionLost] = useState(false);
  const [busy, setBusy] = useState(false);
  const [exited, setExited] = useState(false);
  const [operation, setOperation] = useState<string>();
  const canvas = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | undefined>(undefined);
  const fit = useRef<FitAddon | undefined>(undefined);
  const exitedRef = useRef(exited);
  exitedRef.current = exited;
  const sessionRef = useRef(session);
  sessionRef.current = session;
  const after = useRef(0);
  const inputQueue = useRef<{ id: string; data: string; key: string }[]>([]);
  const draining = useRef(false);
  const paused = useRef(false);
  const [inputPaused, setInputPaused] = useState(false);
  const readGeneration = useRef(0);
  const connectionIntent = useRef<{ body: string; key: string }>(undefined);
  const closeIntent = useRef<{ id: string; key: string }>(undefined);
  useEffect(() => {
    const rows = list(targets.data);
    if (!target && rows.length) setTarget(rows[0].id);
  }, [targets.data, target]);
  const drain = useCallback(async () => {
    if (draining.current || paused.current) return;
    draining.current = true;
    try {
      while (inputQueue.current.length && !paused.current) {
        const frame = inputQueue.current[0];
        try {
          await action(
            "terminal_input",
            { id: frame.id, data: frame.data },
            { idempotencyKey: frame.key },
          );
          if (inputQueue.current[0] === frame) inputQueue.current.shift();
        } catch (e) {
          if (sessionRef.current?.id !== frame.id) {
            if (inputQueue.current[0] === frame) inputQueue.current.shift();
            continue;
          }
          paused.current = true;
          setInputPaused(true);
          setError(
            translate("终端输入发送失败，已暂停后续输入：{0}", [
              (e as Error).message,
            ]),
          );
        }
      }
    } finally {
      draining.current = false;
    }
  }, []);
  const sendBytes = useCallback(
    (bytes: Uint8Array) => {
      const current = sessionRef.current;
      if (!current || exitedRef.current || paused.current) return;
      if (inputQueue.current.length >= 4 || bytes.length > 64 * 1024) {
        paused.current = true;
        setInputPaused(true);
        setError(
          translate("待发送输入过多，已暂停。请核对终端后重试保留的输入。"),
        );
        return;
      }
      for (let i = 0; i < bytes.length; i += 16 * 1024)
        inputQueue.current.push({
          id: current.id,
          data: encode(bytes.slice(i, i + 16 * 1024)),
          key: intentKey(),
        });
      void drain();
    },
    [drain],
  );
  useEffect(() => {
    if (!canvas.current) return;
    const view = new Terminal({
      convertEol: false,
      cursorBlink: false,
      scrollback: 2000,
      fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace",
      fontSize: 13,
      theme: {
        background: "#10141b",
        foreground: "#edf2fa",
        cursor: "#88adff",
        selectionBackground: "#88adff44",
      },
    });
    const fitter = new FitAddon();
    view.loadAddon(fitter);
    view.open(canvas.current);
    terminal.current = view;
    fit.current = fitter;
    const input = view.onData((data) =>
      sendBytes(new TextEncoder().encode(data)),
    );
    const binary = view.onBinary((data) =>
      sendBytes(Uint8Array.from(data, (v) => v.charCodeAt(0))),
    );
    let scheduled: ReturnType<typeof setTimeout> | undefined;
    const resize = () => {
      if (!canvas.current?.clientWidth) return;
      try {
        fitter.fit();
      } catch {}
      const current = sessionRef.current;
      if (current) {
        clearTimeout(scheduled);
        scheduled = setTimeout(
          () =>
            void action(
              "terminal_resize",
              { id: current.id, rows: view.rows, cols: view.cols },
              { idempotencyKey: intentKey() },
            ).catch((e) => setError((e as Error).message)),
          150,
        );
      }
    };
    const observer = new ResizeObserver(resize);
    observer.observe(canvas.current);
    resize();
    return () => {
      observer.disconnect();
      clearTimeout(scheduled);
      input.dispose();
      binary.dispose();
      view.dispose();
      terminal.current = undefined;
      fit.current = undefined;
    };
  }, [sendBytes, has("terminal_open")]);
  useEffect(() => {
    if (!session || connectionLost) return;
    let disposed = false;
    let timeout: ReturnType<typeof setTimeout> | undefined;
    const generation = ++readGeneration.current;
    const read = async () => {
      try {
        const update = await action<ReadResult>("terminal_read", {
          id: session.id,
          after: after.current,
          limit: 8,
        });
        if (disposed || generation !== readGeneration.current) return;
        setReadError("");
        for (const frame of update.frames ?? []) {
          terminal.current?.write(decode(frame.data));
          after.current = Math.max(after.current, frame.sequence);
        }
        after.current = Math.max(after.current, update.next ?? 0);
        setSession((previous) =>
          previous &&
          update.expires_at &&
          previous.expires_at !== update.expires_at
            ? { ...previous, expires_at: update.expires_at }
            : previous,
        );
        if (update.truncated)
          setReadError(
            translate("部分终端输出已超过回放范围；当前输出已继续同步。"),
          );
        if (update.exited) {
          setExited(true);
          terminal.current?.writeln(
            translate("\r\n[进程已退出，代码 {0}]", [update.exit_code]),
          );
          return;
        }
        timeout = setTimeout(read, active ? 150 : 800);
      } catch (e) {
        if (disposed) return;
        setReadError((e as Error).message);
        if (e instanceof APIError && [401, 403, 404, 410].includes(e.status)) {
          paused.current = true;
          setInputPaused(true);
          setExited(true);
          setConnectionLost(true);
          return;
        }
        timeout = setTimeout(read, 1000);
      }
    };
    void read();
    return () => {
      disposed = true;
      clearTimeout(timeout);
    };
  }, [session?.id, active, connectionLost]);
  useEffect(() => {
    if (!session?.id || exited || !active || !has("terminal_renew")) return;
    let disposed = false;
    let renewing = false;
    let key: string | undefined;
    const renew = async () => {
      const current = sessionRef.current;
      if (
        disposed ||
        renewing ||
        document.visibilityState !== "visible" ||
        !current ||
        current.id !== session.id ||
        exitedRef.current
      )
        return;
      const expiry = Date.parse(current.expires_at);
      if (Number.isFinite(expiry) && expiry - Date.now() > 30000) return;
      renewing = true;
      key ??= intentKey();
      try {
        const next = await action<Partial<TerminalSession>>(
          "terminal_renew",
          { id: current.id },
          { idempotencyKey: key },
        );
        if (!disposed)
          setSession((previous) =>
            previous?.id === current.id && next.expires_at
              ? { ...previous, expires_at: next.expires_at }
              : previous,
          );
        key = undefined;
      } catch (e) {
        if (!disposed)
          setError(translate("终端续租失败：{0}", [(e as Error).message]));
      } finally {
        renewing = false;
      }
    };
    const timer = setInterval(() => void renew(), 5000);
    const onVisible = () => void renew();
    document.addEventListener("visibilitychange", onVisible);
    void renew();
    return () => {
      disposed = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [session?.id, exited, active, has("terminal_renew")]);
  const connect = async () => {
    if (inputQueue.current.length) {
      setError(
        translate("仍有未确认的输入，请核对重试或明确放弃输入后切换会话。"),
      );
      return;
    }
    setBusy(true);
    setError("");
    try {
      fit.current?.fit();
      const input = {
        target_id: target,
        rows: terminal.current?.rows ?? 24,
        cols: terminal.current?.cols ?? 80,
      };
      const body = JSON.stringify(input);
      if (connectionIntent.current?.body !== body)
        connectionIntent.current = { body, key: intentKey() };
      const next = await action<TerminalSession>("terminal_open", input, {
        idempotencyKey: connectionIntent.current.key,
      });
      connectionIntent.current = undefined;
      after.current = 0;
      inputQueue.current = [];
      paused.current = false;
      setInputPaused(false);
      setExited(false);
      setConnectionLost(false);
      setReadError("");
      terminal.current?.clear();
      setSession(next);
      terminal.current?.focus();
      onExecuted();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const disconnect = async () => {
    if (!session) return;
    setBusy(true);
    try {
      if (closeIntent.current?.id !== session.id)
        closeIntent.current = { id: session.id, key: intentKey() };
      await action(
        "terminal_close",
        { id: session.id },
        { idempotencyKey: closeIntent.current.key },
      );
      closeIntent.current = undefined;
      inputQueue.current = [];
      paused.current = false;
      setInputPaused(false);
      sessionRef.current = undefined;
      setSession(undefined);
      setExited(true);
      setConnectionLost(false);
      terminal.current?.writeln(translate("\r\n[会话已关闭]"));
      onExecuted();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const attach = async () => {
    if (!existingID) return;
    if (inputQueue.current.length && session?.id !== existingID) {
      setError(
        translate("仍有未确认的输入，请核对重试或明确放弃输入后切换会话。"),
      );
      return;
    }
    setBusy(true);
    setError("");
    try {
      const next = await action<ReadResult>("terminal_read", {
        id: existingID,
        after: 0,
        limit: 8,
      });
      terminal.current?.clear();
      after.current = 0;
      for (const frame of next.frames ?? []) {
        terminal.current?.write(decode(frame.data));
        after.current = Math.max(after.current, frame.sequence);
      }
      after.current = Math.max(after.current, next.next ?? 0);
      if (session?.id !== next.id) {
        inputQueue.current = [];
        paused.current = false;
        setInputPaused(false);
      }
      setSession(next);
      setTarget(next.target_id);
      setExited(next.exited);
      setConnectionLost(false);
      setReadError(
        next.truncated
          ? translate("部分输出已超出回放范围，当前输出继续同步。")
          : "",
      );
      terminal.current?.focus();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="stack">
      <Card className="panel-card">
        <div className="card-heading">
          <h2>{translate("终端")}</h2>
          <span className="status-chip neutral">
            {translate("真实PTY / 校验主机密钥的SSH")}
          </span>
        </div>
        {!has("terminal_open") ? (
          <Unavailable
            title={translate("终端服务暂不可用")}
            icon="terminal"
            detail={translate("只有受信任的主机策略提供目标后，才允许连接。")}
          />
        ) : (
          <>
            <div className="terminal-toolbar">
              <label className="native-field">
                {translate("连接目标")}
                <select
                  aria-label={translate("终端连接目标")}
                  value={target}
                  onChange={(e) => setTarget(e.target.value)}
                  disabled={!!session && !exited}
                >
                  {list(targets.data).map((t) => (
                    <option key={t.id} value={t.id}>
                      {t.label ?? t.name ?? t.id}
                      {t.host
                        ? ` · ${t.username ?? t.user ?? ""}@${t.host}`
                        : ""}
                    </option>
                  ))}
                </select>
              </label>
              <Button
                label={translate("连接终端")}
                variant="primary"
                isDisabled={!target || (!!session && !exited)}
                isLoading={busy}
                onClick={() => void connect()}
              />
              {session && !exited && (
                <Button
                  label={translate("关闭终端会话")}
                  onClick={() => void disconnect()}
                  isLoading={busy}
                />
              )}
            </div>
            {has("terminal_read") && (
              <details className="terminal-attach">
                <summary>{translate("接入已有会话")}</summary>
                <p className="muted">
                  {translate(
                    "填写已知会话标识。读取、输入与续租仍按该会话的实际目标权限核验。",
                  )}
                </p>
                <div className="button-row">
                  <TextInput
                    label={translate("已有会话标识")}
                    value={existingID}
                    onChange={setExistingID}
                  />
                  <Button
                    label={translate("接入已有终端会话")}
                    onClick={() => void attach()}
                    isLoading={busy}
                    isDisabled={!existingID || (!!session && !exited)}
                  />
                </div>
              </details>
            )}
            {targets.error && (
              <div className="notice danger">{targets.error}</div>
            )}
            {(error || readError) && (
              <div className="notice danger" role="alert">
                {error || readError}
              </div>
            )}
            {inputPaused && (
              <div className="notice warning">
                <p>
                  {translate(
                    "后续键盘输入已暂停。重试会复用原帧请求标识，避免重复输入。",
                  )}
                </p>
                <Button
                  label={translate("重试保留的终端输入")}
                  isDisabled={exited}
                  onClick={() => {
                    paused.current = false;
                    setInputPaused(false);
                    setError("");
                    void drain();
                  }}
                />
                <Button
                  label={translate("放弃保留的终端输入")}
                  variant="ghost"
                  onClick={() => {
                    inputQueue.current = [];
                    paused.current = false;
                    setInputPaused(false);
                    setError("");
                  }}
                />
              </div>
            )}
            {session && (
              <p className="muted">
                {translate("会话")}
                {session.id}
                {translate(" · 到期 ")}
                {fullDate(session.expires_at)}·{" "}
                {connectionLost
                  ? translate("连接已失效")
                  : exited
                    ? translate("已退出")
                    : translate("已连接")}
              </p>
            )}
            <div
              ref={canvas}
              className="terminal-canvas"
              aria-label={translate("终端输出")}
            />
            {list(commands.data).length > 0 && (
              <details>
                <summary>{translate("快捷命令")}</summary>
                {list(commands.data).map((command) => (
                  <div className="command-row" key={command.id}>
                    <div>
                      <strong>{command.title}</strong>
                      <pre>{command.command}</pre>
                    </div>
                    <Button
                      label={translate("输入 {0}", [command.title])}
                      isDisabled={!session || exited || inputPaused}
                      onClick={() =>
                        sendBytes(new TextEncoder().encode(command.command))
                      }
                    />
                  </div>
                ))}
                <p className="muted">
                  {translate("快捷命令只输入终端；请核对内容后按Enter执行。")}
                </p>
              </details>
            )}
          </>
        )}
      </Card>
      <details className="advanced-actions">
        <summary>{translate("SSH目标、快捷命令与会话操作")}</summary>
        <ActionBrowser
          capabilities={capabilities}
          category="terminal"
          selected={operation}
          onSelect={setOperation}
          onExecuted={() => {
            void targets.reload();
            void commands.reload();
            onExecuted();
          }}
        />
      </details>
    </div>
  );
}
