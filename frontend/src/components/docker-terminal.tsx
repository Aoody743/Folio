"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, apiErrorMessage, Capability, intentKey } from "@/lib/api";
import {
  dockerTerminalCapability,
  dockerTerminalInputFrame,
  dockerTerminalRead,
  dockerTerminalSession,
  dockerTerminalStateLabel,
  DockerTerminalSession,
} from "@/lib/docker-terminal";
import { Unavailable } from "./resource-ui";

type QueuedInput = { input_id: string; data: string };
type SocketMessage = Record<string, unknown>;

function encode(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function decode(value: string): Uint8Array {
  return Uint8Array.from(atob(value), (character) => character.charCodeAt(0));
}

function isRecord(value: unknown): value is SocketMessage {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

function boundedDimension(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, Math.floor(value)));
}

export function DockerTerminalManagement({
  instanceID,
  containerName,
  running,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  containerName: string;
  running: boolean;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const openCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_open",
    "containers:manage",
    ["id", "name", "request_id"],
    false,
    ["id", "name", "request_id", "rows", "cols"],
  );
  const readCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_read",
    "containers:read",
    ["session_id"],
    true,
    ["session_id", "after", "limit"],
  );
  const inputCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_input",
    "containers:manage",
    ["session_id", "input_id", "data"],
    false,
  );
  const resizeCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_resize",
    "containers:manage",
    ["session_id", "rows", "cols"],
    false,
  );
  const closeCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_close",
    "containers:manage",
    ["session_id"],
    false,
  );
  const statusCapability = dockerTerminalCapability(
    capabilities,
    "docker_terminal_status",
    "containers:read",
    ["session_id"],
    true,
  );
  const missing = [
    [openCapability, "docker_terminal_open"],
    [readCapability, "docker_terminal_read"],
    [inputCapability, "docker_terminal_input"],
    [resizeCapability, "docker_terminal_resize"],
    [closeCapability, "docker_terminal_close"],
    [statusCapability, "docker_terminal_status"],
  ].filter(([capability]) => !capability).map(([, name]) => name);

  const [session, setSession] = useState<DockerTerminalSession>();
  const [attached, setAttached] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [readNotice, setReadNotice] = useState("");
  const [existingID, setExistingID] = useState("");
  const [inputPaused, setInputPaused] = useState(false);
  const [queuedCount, setQueuedCount] = useState(0);
  const canvas = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | undefined>(undefined);
  const fit = useRef<FitAddon | undefined>(undefined);
  const sessionRef = useRef<DockerTerminalSession | undefined>(undefined);
  const socketRef = useRef<WebSocket | undefined>(undefined);
  const queue = useRef<QueuedInput[]>([]);
  const inputInFlight = useRef<string | undefined>(undefined);
  const inputUncertain = useRef(false);
  const inputDraining = useRef(false);
  const inputPausedRef = useRef(false);
  const cursor = useRef(0);
  const openIntent = useRef<{ body: string; requestID: string } | undefined>(undefined);
  const closeIntent = useRef<{ sessionID: string; key: string } | undefined>(undefined);
  sessionRef.current = session;

  const displayRead = useCallback(
    (value: unknown, expectedID: string) => {
      const parsed = dockerTerminalRead(
        value,
        instanceID,
        containerName,
        expectedID,
        cursor.current,
      );
      if (!parsed) throw new Error("Docker终端输出响应无效，请重新读取状态。");
      for (const frame of parsed.frames) {
        if (frame.sequence <= cursor.current) continue;
        terminal.current?.write(decode(frame.data));
        cursor.current = frame.sequence;
      }
      cursor.current = Math.max(cursor.current, parsed.next);
      sessionRef.current = parsed;
      setSession(parsed);
      if (parsed.output_unavailable)
        setReadNotice("服务重启后，之前缓冲的终端输出不可恢复；当前新输出仍可读取。");
      else if (parsed.truncated)
        setReadNotice("部分输出已超出服务端回放范围；后续输出继续同步。");
      else setReadNotice("");
    },
    [instanceID, containerName],
  );

  const drainInput = useCallback(() => {
    if (inputDraining.current || inputPausedRef.current || inputUncertain.current || inputInFlight.current) return;
    const socket = socketRef.current;
    const next = queue.current[0];
    if (!socket || socket.readyState !== WebSocket.OPEN || !next) return;
    inputDraining.current = true;
    try {
      const current = sessionRef.current;
      if (!current) return;
      dockerTerminalInputFrame(
        current.session_id,
        next.input_id,
        next.data,
      );
      inputInFlight.current = next.input_id;
      socket.send(JSON.stringify({ type: "input", input_id: next.input_id, data: next.data }));
    } catch (e) {
      inputInFlight.current = undefined;
      inputUncertain.current = true;
      inputPausedRef.current = true;
      setInputPaused(true);
      setError(apiErrorMessage(e));
    } finally {
      inputDraining.current = false;
    }
  }, []);

  const sendBytes = useCallback(
    (bytes: Uint8Array) => {
      const current = sessionRef.current;
      if (!current || current.execution_state === "exited" || current.close_requested) return;
      if (bytes.length > 64 * 1024 || queue.current.length + Math.ceil(bytes.length / (16 * 1024)) > 4) {
        inputPausedRef.current = true;
        setInputPaused(true);
        setError("待确认输入达到本地内存上限，已暂停键盘输入。请先确认或放弃保留的输入。");
        return;
      }
      for (let offset = 0; offset < bytes.length; offset += 16 * 1024) {
        const part = bytes.slice(offset, offset + 16 * 1024);
        queue.current.push({ input_id: intentKey(), data: encode(part) });
      }
      setQueuedCount(queue.current.length);
      if (!socketRef.current || socketRef.current.readyState !== WebSocket.OPEN) {
        setError("浏览器传输当前未连接；输入暂存在本页内存中，发送前请确认会话状态。");
        inputPausedRef.current = true;
        setInputPaused(true);
        return;
      }
      if (!inputPausedRef.current) drainInput();
    },
    [drainInput],
  );

  const attachSocket = useCallback(
    (sessionID: string) => {
      if (!active || typeof window === "undefined") return;
      const current = socketRef.current;
      if (current && current.readyState <= WebSocket.OPEN) return;
      const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
      const socket = new WebSocket(
        `${scheme}//${window.location.host}/api/v1/terminal/sessions/${encodeURIComponent(sessionID)}/ws`,
      );
      socketRef.current = socket;
      setAttached(false);
      socket.onopen = () => {
        if (socketRef.current !== socket) return;
        setAttached(true);
        setError("");
        if (!inputUncertain.current && !inputPausedRef.current) drainInput();
      };
      socket.onmessage = (event) => {
        if (socketRef.current !== socket || typeof event.data !== "string") return;
        let message: unknown;
        try {
          message = JSON.parse(event.data);
        } catch {
          setError("Docker终端传输返回了无法读取的消息。");
          return;
        }
        if (!isRecord(message) || typeof message.type !== "string") {
          setError("Docker终端传输消息格式无效。");
          return;
        }
        if (message.type === "output") {
          try {
            displayRead(message.data, sessionID);
          } catch (e) {
            setReadNotice(apiErrorMessage(e));
          }
          return;
        }
        if (message.type === "ack" && message.operation === "input") {
          const head = queue.current[0];
          const result = message.data;
          if (
            !head ||
            message.input_id !== head.input_id ||
            !isRecord(result) ||
            result.session_id !== sessionID ||
            result.input_id !== head.input_id ||
            result.accepted !== true
          ) {
            inputUncertain.current = true;
            inputPausedRef.current = true;
            setInputPaused(true);
            setError("终端输入确认与待发送帧不匹配；已暂停，不会自动重发。");
            return;
          }
          queue.current.shift();
          inputInFlight.current = undefined;
          inputUncertain.current = false;
          inputPausedRef.current = false;
          setQueuedCount(queue.current.length);
          setError("");
          if (!inputPausedRef.current) drainInput();
          return;
        }
        if (message.type === "error") {
          const details = isRecord(message.error) ? message.error : undefined;
          setError(
            typeof details?.message === "string"
              ? details.message
              : "Docker终端传输报告错误。",
          );
          if (inputInFlight.current) {
            inputUncertain.current = true;
            inputPausedRef.current = true;
            setInputPaused(true);
          }
        }
      };
      socket.onerror = () => {
        if (socketRef.current !== socket) return;
        setError("Docker终端浏览器传输发生错误；会话可能仍在服务端运行。");
      };
      socket.onclose = () => {
        if (socketRef.current !== socket) return;
        socketRef.current = undefined;
        setAttached(false);
        if (inputInFlight.current) {
          inputUncertain.current = true;
          inputPausedRef.current = true;
          setInputPaused(true);
          setError("传输在输入确认前断开。请检查状态后，用相同帧编号明确重试或放弃。");
        }
      };
    },
    [active, displayRead, drainInput],
  );

  const readOutput = useCallback(
    async (sessionID: string) => {
      if (!readCapability) return;
      const value = await action<unknown>("docker_terminal_read", {
        session_id: sessionID,
        after: cursor.current,
        limit: 32,
      });
      displayRead(value, sessionID);
    },
    [displayRead, readCapability],
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
    const input = view.onData((data) => sendBytes(new TextEncoder().encode(data)));
    const binary = view.onBinary((data) =>
      sendBytes(Uint8Array.from(data, (character) => character.charCodeAt(0))),
    );
    let resizeTimer: ReturnType<typeof setTimeout> | undefined;
    const resize = () => {
      if (!canvas.current?.clientWidth) return;
      try {
        fitter.fit();
      } catch {}
      const current = sessionRef.current;
      const socket = socketRef.current;
      if (!current || !socket || socket.readyState !== WebSocket.OPEN) return;
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        if (socket.readyState !== WebSocket.OPEN) return;
        const rows = boundedDimension(view.rows, 1, 500);
        const cols = boundedDimension(view.cols, 1, 1000);
        socket.send(JSON.stringify({ type: "resize", rows, cols }));
      }, 150);
    };
    const observer = new ResizeObserver(resize);
    observer.observe(canvas.current);
    resize();
    return () => {
      observer.disconnect();
      clearTimeout(resizeTimer);
      input.dispose();
      binary.dispose();
      view.dispose();
      terminal.current = undefined;
      fit.current = undefined;
    };
  }, [sendBytes, missing.length]);

  useEffect(() => {
    if (active || !socketRef.current) return;
    socketRef.current.close();
    socketRef.current = undefined;
    setAttached(false);
  }, [active]);

  useEffect(() => () => {
    socketRef.current?.close();
    socketRef.current = undefined;
  }, []);

  const open = async () => {
    if (!running) {
      setError("容器当前未运行；启动容器后再打开交互终端。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      fit.current?.fit();
      const rows = boundedDimension(terminal.current?.rows ?? 24, 1, 500);
      const cols = boundedDimension(terminal.current?.cols ?? 80, 1, 1000);
      const request = { id: instanceID, name: containerName, rows, cols };
      const body = JSON.stringify(request);
      if (openIntent.current?.body !== body)
        openIntent.current = { body, requestID: intentKey() };
      const requestID = openIntent.current.requestID;
      const value = await action<unknown>(
        "docker_terminal_open",
        { ...request, request_id: requestID },
        { idempotencyKey: requestID },
      );
      const parsed = dockerTerminalSession(value, instanceID, containerName);
      if (!parsed) throw new Error("Docker终端打开响应无效；请检查服务端会话状态后再重试。");
      openIntent.current = undefined;
      closeIntent.current = undefined;
      queue.current = [];
      inputInFlight.current = undefined;
      inputUncertain.current = false;
      inputPausedRef.current = false;
      setQueuedCount(0);
      setInputPaused(false);
      cursor.current = 0;
      terminal.current?.clear();
      sessionRef.current = parsed;
      setSession(parsed);
      onExecuted();
      try {
        await readOutput(parsed.session_id);
      } catch (e) {
        setReadNotice(`初始输出读取失败：${apiErrorMessage(e)}`);
      }
      attachSocket(parsed.session_id);
      terminal.current?.focus();
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const attachExisting = async () => {
    const sessionID = existingID.trim();
    if (!sessionID) return;
    setBusy(true);
    setError("");
    try {
      const value = await action<unknown>("docker_terminal_status", { session_id: sessionID });
      const parsed = dockerTerminalSession(value, instanceID, containerName);
      if (!parsed) throw new Error("会话与当前实例/容器不匹配，或状态响应无效。");
      if (sessionRef.current?.session_id !== parsed.session_id) {
        cursor.current = 0;
        terminal.current?.clear();
        queue.current = [];
        inputInFlight.current = undefined;
        inputUncertain.current = false;
        inputPausedRef.current = false;
        setQueuedCount(0);
        setInputPaused(false);
      }
      sessionRef.current = parsed;
      setSession(parsed);
      await readOutput(parsed.session_id);
      attachSocket(parsed.session_id);
      terminal.current?.focus();
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const refreshStatus = async () => {
    const current = sessionRef.current;
    if (!current) return;
    setBusy(true);
    setError("");
    try {
      const value = await action<unknown>("docker_terminal_status", {
        session_id: current.session_id,
      });
      const parsed = dockerTerminalSession(value, instanceID, containerName);
      if (!parsed) throw new Error("Docker终端状态响应无效。");
      sessionRef.current = parsed;
      setSession(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const syncOutput = async () => {
    const current = sessionRef.current;
    if (!current) return;
    setBusy(true);
    setError("");
    try {
      await readOutput(current.session_id);
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const close = async () => {
    const current = sessionRef.current;
    if (!current || queue.current.length > 0) return;
    setBusy(true);
    setError("");
    try {
      if (closeIntent.current?.sessionID !== current.session_id)
        closeIntent.current = { sessionID: current.session_id, key: intentKey() };
      const value = await action<unknown>(
        "docker_terminal_close",
        { session_id: current.session_id },
        { idempotencyKey: closeIntent.current.key },
      );
      const parsed = dockerTerminalSession(value, instanceID, containerName);
      if (!parsed) throw new Error("关闭请求已返回，但Engine状态响应无效；请重新读取状态。");
      closeIntent.current = undefined;
      sessionRef.current = parsed;
      setSession(parsed);
      socketRef.current?.close();
      socketRef.current = undefined;
      setAttached(false);
      setError(
        parsed.execution_state === "running"
          ? "终端流已关闭；Engine中的shell进程仍在运行。"
          : "终端流已关闭；Engine状态已重新读取。",
      );
      onExecuted();
    } catch (e) {
      setError(`${apiErrorMessage(e)} 未自动重试；请先读取会话状态。`);
    } finally {
      setBusy(false);
    }
  };

  const retryUncertain = () => {
    if (!inputUncertain.current || !queue.current.length) return;
    inputUncertain.current = false;
    inputInFlight.current = undefined;
    inputPausedRef.current = false;
    setInputPaused(false);
    setError("");
    drainInput();
  };

  const discardPending = () => {
    queue.current = [];
    inputInFlight.current = undefined;
    inputUncertain.current = false;
    inputPausedRef.current = false;
    setQueuedCount(0);
    setInputPaused(false);
    setError("");
  };

  const canOpen = !!openCapability && running && (!session || session.execution_state === "exited");
  const noSession = !session;

  return (
    <section className="stack" aria-label="交互式Docker终端">
      <div className="card-heading">
        <h3>交互式 Docker 终端</h3>
        {session && (
          <span className={`status-chip ${session.execution_state === "running" ? "success" : "neutral"}`}>
            {dockerTerminalStateLabel(session.execution_state)}
          </span>
        )}
      </div>
      {missing.length ? (
        <Unavailable
          title="当前账户或后端未提供完整的 Docker 终端能力"
          detail={`缺少经过校验的能力：${missing.join("、")}。`}
        />
      ) : !/^([a-z][a-z0-9_-]{0,31})$/.test(containerName) ? (
        <Unavailable title="容器名称不符合受管逻辑名称格式" />
      ) : (
        <>
          <p className="muted">
            固定启动受管容器内的 <code>/bin/sh -i</code>。输入通过浏览器 WebSocket 传输并使用会话帧编号去重；终端内容只保存在当前页面内存中。
          </p>
          {error && <div className="notice danger" role="alert">{error}</div>}
          {readNotice && <div className="notice warning" role="status">{readNotice}</div>}
          <div
            ref={canvas}
            className="terminal-canvas"
            aria-label="Docker容器终端输出"
            hidden={noSession}
          />
          {noSession && (
            <div className="button-row">
              <Button
                label="打开交互终端"
                variant="primary"
                isDisabled={!canOpen || busy}
                isLoading={busy}
                onClick={() => void open()}
              />
              {!running && <span className="muted">容器必须处于运行状态。</span>}
            </div>
          )}
          {session && (
            <>
              <p className="muted">
                会话 <code>{session.session_id}</code> · {attached ? "浏览器已连接" : "浏览器未连接"} · Agent流 {session.stream_started ? "已启动" : "等待启动"}
                {session.attached ? " · 服务端已附着" : " · 服务端未附着"}
              </p>
              {session.execution_state === "exited" && running && (
                <div className="notice warning">
                  <p>当前会话已退出。新建会话会清空此页内保留的旧终端输出。</p>
                  <Button
                    label="打开新终端会话"
                    variant="primary"
                    isDisabled={!canOpen || busy}
                    isLoading={busy}
                    onClick={() => void open()}
                  />
                </div>
              )}
              <div className="button-row">
                {attached ? (
                  <Button label="重读缓冲输出" isDisabled={busy || !readCapability} onClick={() => void syncOutput()} />
                ) : (
                  <Button label="重新连接浏览器终端" isDisabled={busy || !active} isLoading={busy} onClick={() => attachSocket(session.session_id)} />
                )}
                <Button label="读取Engine状态" variant="ghost" isDisabled={busy || !statusCapability} onClick={() => void refreshStatus()} />
                <Button
                  label="关闭终端流"
                  variant="ghost"
                  isDisabled={busy || queuedCount > 0 || !closeCapability || session.execution_state === "exited"}
                  isLoading={busy}
                  onClick={() => void close()}
                />
              </div>
              {queuedCount > 0 && (
                <div className="notice warning">
                  <p>{queuedCount} 个终端输入帧尚未确认。关闭流前请重试同一帧或明确放弃；确认前不要重复执行相同命令。</p>
                  {inputPaused ? (
                    <Button label="用相同帧编号重试" isDisabled={!attached} onClick={retryUncertain} />
                  ) : (
                    <Button label="重试保留输入" isDisabled={!attached} onClick={() => drainInput()} />
                  )}
                  <Button label="放弃未确认输入" variant="ghost" onClick={discardPending} />
                </div>
              )}
            </>
          )}
          {!session && statusCapability && readCapability && (
            <details className="terminal-attach">
              <summary>接回已有会话</summary>
              <p className="muted">粘贴先前显示的受管会话编号。服务端会按原始会话所有者和容器资源重新核验权限。</p>
              <div className="button-row">
                <TextInput label="Docker终端会话编号" value={existingID} onChange={setExistingID} autoComplete="off" />
                <Button label="读取状态并接入" isDisabled={!existingID || busy} isLoading={busy} onClick={() => void attachExisting()} />
              </div>
            </details>
          )}
        </>
      )}
    </section>
  );
}
