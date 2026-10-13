"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import dynamic from "next/dynamic";
import { clearDrafts } from "@/lib/draft-cache";
import { authorizationReturn, AuthValues, loginHints } from "@/lib/auth-hints";
import { Theme } from "@astryxdesign/core/theme";
import { InternationalizationProvider } from "@astryxdesign/core/i18n";
import zhCN from "@astryxdesign/core/locales/zh-CN.generated.js";
import { neutralTheme } from "@astryxdesign/theme-neutral/built";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Dialog } from "@astryxdesign/core/Dialog";
import {
  api,
  APIError,
  Audit,
  bytes,
  Capability,
  duration,
  fullDate,
  list,
  Note,
  Session,
  setCSRF,
  source,
  SystemStatus,
  Task,
} from "@/lib/api";
import { Icon, IconName } from "./icon";
const NoteEditor = dynamic(() => import("./notes").then((m) => m.NoteEditor), {
  ssr: false,
});
const Settings = dynamic(() => import("./settings").then((m) => m.Settings), {
  ssr: false,
});
const ActionBrowser = dynamic(
  () => import("./actions").then((m) => m.ActionBrowser),
  { ssr: false },
);

const SitesView = dynamic(() => import("./sites").then((m) => m.SitesView), {
  ssr: false,
});
const SoftwareView = dynamic(
  () => import("./software").then((m) => m.SoftwareView),
  { ssr: false },
);
const FilesView = dynamic(() => import("./files").then((m) => m.FilesView), {
  ssr: false,
});
const PanelSettings = dynamic(
  () => import("./panel-settings").then((m) => m.PanelSettings),
  { ssr: false },
);
const OperationsView = dynamic(
  () => import("./operations").then((m) => m.OperationsView),
  { ssr: false },
);
const TerminalView = dynamic(
  () => import("./terminal").then((m) => m.TerminalView),
  { ssr: false },
);

export type Palette = "moon" | "space" | "violet" | "pine" | "sand";
export const palettes: { id: Palette; name: string; desc: string }[] = [
  { id: "moon", name: "月白", desc: "清晰 · 浅色" },
  { id: "space", name: "深空", desc: "沉静 · 深色" },
  { id: "violet", name: "雾紫", desc: "柔和 · 浅色" },
  { id: "pine", name: "松绿", desc: "自然 · 深色" },
  { id: "sand", name: "暖砂", desc: "温暖 · 浅色" },
];
const nav: { id: string; label: string; icon: IconName }[] = [
  { id: "overview", label: "概览", icon: "overview" },
  { id: "sites", label: "网站", icon: "sites" },
  { id: "software", label: "软件", icon: "software" },
  { id: "files", label: "文件", icon: "files" },
  { id: "more", label: "更多", icon: "more" },
];
const stateText: Record<string, string> = {
  queued: "排队中",
  running: "执行中",
  succeeded: "执行成功",
  success: "执行成功",
  completed: "执行成功",
  failed: "执行失败",
  cancelled: "已取消",
  canceled: "已取消",
  verifying: "验证中",
  verified: "验证成功",
  pending: "待执行",
};
export const taskState = (task: Task) => task.state ?? task.status ?? "unknown";
function Metric({
  label,
  value,
  ratio,
  hint,
}: {
  label: string;
  value: string;
  ratio?: number | null;
  hint?: string;
}) {
  return (
    <div className="metric">
      <div className="metric-label">{label}</div>
      <div className="metric-value">{value}</div>
      <div className="meter" aria-hidden="true">
        <span style={{ width: `${Math.min(100, Math.max(0, ratio ?? 0))}%` }} />
      </div>
      <div className="metric-hint">{hint ?? "未提供数据"}</div>
    </div>
  );
}
function CardTitle({
  icon,
  title,
  children,
}: {
  icon: IconName;
  title: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="card-heading">
      <h2>
        <span className="heading-icon">
          <Icon name={icon} />
        </span>
        {title}
      </h2>
      {children}
    </div>
  );
}
function Empty({
  icon,
  title,
  detail,
  children,
}: {
  icon: IconName;
  title: string;
  detail?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="empty">
      <span className="empty-icon">
        <Icon name={icon} size={24} />
      </span>
      <strong>{title}</strong>
      {detail && <p>{detail}</p>}
      {children}
    </div>
  );
}
export function Panel() {
  const [session, setSession] = useState<Session>();
  const [authError, setAuthError] = useState("");
  const [authBusy, setAuthBusy] = useState(false);
  const [palette, setPalette] = useState<Palette>("moon");
  const [section, setSection] = useState("overview");
  const [morePage, setMorePage] = useState("menu");
  const [visited, setVisited] = useState<Record<string, boolean>>({});
  const [fileRequest, setFileRequest] = useState<{
    root: string;
    sequence: number;
  }>();
  const [status, setStatus] = useState<SystemStatus>();
  const [notes, setNotes] = useState<Note[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [audit, setAudit] = useState<Audit[]>([]);
  const [caps, setCaps] = useState<Capability[]>([]);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [loading, setLoading] = useState(false);
  const [connection, setConnection] = useState<
    "connecting" | "online" | "offline"
  >("connecting");
  const [lastLoaded, setLastLoaded] = useState<string>();
  const [noteOpen, setNoteOpen] = useState(false);
  const [selectedNote, setSelectedNote] = useState<Note>();
  const [editorKey, setEditorKey] = useState(0);
  const [editorReady, setEditorReady] = useState(false);
  const [hasDraft, setHasDraft] = useState(false);
  const [draftWarning, setDraftWarning] = useState(false);
  const [notesAll, setNotesAll] = useState(false);
  const [notesPage, setNotesPage] = useState(0);
  const [archived, setArchived] = useState(false);
  const [taskOpen, setTaskOpen] = useState(false);
  const [selectedTask, setSelectedTask] = useState<Task>();
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [selectedAction, setSelectedAction] = useState<string>();
  const refreshTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const refreshGeneration = useRef(0);
  useEffect(() => {
    const name = ["sites", "software", "files"].includes(section)
      ? section
      : section === "more" && morePage === "terminal"
        ? "terminal"
        : "";
    if (name) setVisited((v) => (v[name] ? v : { ...v, [name]: true }));
  }, [section, morePage]);
  const scopes = session?.user?.scopes ?? [];
  const permitted = (scope: string) =>
    scopes.includes("*") ||
    scopes.includes(scope) ||
    scopes.includes(scope.split(":")[0] + ":*");
  const canWriteNotes =
    permitted("notes:write") || caps.some((c) => c.name === "notes_update");
  const canDeleteNotes =
    permitted("notes:delete") || caps.some((c) => c.name === "notes_delete");
  const initialHints = useRef(
    typeof window !== "undefined"
      ? loginHints(location.hash)
      : { loginEntry: "", temporaryToken: "", remainingHash: "" },
  );
  const bootstrapStarted = useRef(false);
  const sessionBoundary = useRef<string>(undefined);
  const receiveSession = useCallback((next: Session) => {
    const identity = next.authenticated
      ? JSON.stringify([
          next.user?.id,
          next.user?.kind,
          next.user?.scopes,
          next.user?.resources,
        ])
      : "unauthenticated";
    const previous = sessionBoundary.current;
    const changed = previous !== undefined && previous !== identity;
    sessionBoundary.current = identity;
    setCSRF(next.csrf_token);
    if (!next.authenticated || changed) {
      clearDrafts();
      setVisited({});
      ++refreshGeneration.current;
      setNotes([]);
      setTasks([]);
      setAudit([]);
      setCaps([]);
      setStatus(undefined);
      setEditorReady(false);
      setNoteOpen(false);
      setHasDraft(false);
      setTaskOpen(false);
      setSearchOpen(false);
      setSelectedAction(undefined);
      setFileRequest(undefined);
      if (previous !== undefined && previous !== "unauthenticated") {
        setSection("overview");
        setMorePage("menu");
      }
    }
    setSession(next);
    document.title = next.panel?.panel_name || "PowerLinux";
  }, []);
  const loadSession = useCallback(async () => {
    try {
      const next = await api<Session>("/session");
      receiveSession(next);
      setAuthError("");
      return next;
    } catch (e) {
      setAuthError((e as Error).message);
    }
  }, [receiveSession]);
  const refresh = useCallback(async () => {
    if (!session?.authenticated) return;
    const generation = ++refreshGeneration.current;
    setLoading(true);
    const names = [
      "system",
      "notes",
      "tasks",
      "audit",
      "capabilities",
      "session",
    ];
    const results = await Promise.allSettled([
      api<SystemStatus>("/system/status"),
      Promise.all([
        api<Note[]>(
          `/notes?limit=${notesAll ? 20 : 100}&offset=${notesAll ? notesPage * 20 : 0}`,
        ),
        api<Note[]>(
          `/notes?archived=true&limit=${notesAll ? 20 : 100}&offset=${notesAll ? notesPage * 20 : 0}`,
        ),
      ]).then(([active, archived]) => [...list(active), ...list(archived)]),
      api<Task[]>("/tasks?limit=50"),
      api<Audit[]>("/audit?limit=30"),
      api<Capability[]>("/capabilities"),
      api<Session>("/session"),
    ]);
    if (generation !== refreshGeneration.current) return;
    const nextErrors: Record<string, string> = {};
    results.forEach((result, i) => {
      if (result.status === "rejected") {
        nextErrors[names[i]] =
          result.reason instanceof Error ? result.reason.message : "读取失败";
        if (result.reason instanceof APIError && result.reason.status === 401)
          void loadSession();
        return;
      }
      switch (i) {
        case 0:
          setStatus(result.value as SystemStatus);
          break;
        case 1:
          setNotes(list(result.value as Note[]));
          break;
        case 2:
          setTasks(list(result.value as Task[]));
          break;
        case 3:
          setAudit(list(result.value as Audit[]));
          break;
        case 4:
          setCaps(list(result.value as Capability[]));
          break;
        case 5: {
          const next = result.value as Session;
          receiveSession(next);
          break;
        }
      }
    });
    setErrors(nextErrors);
    setLastLoaded(new Date().toISOString());
    setLoading(false);
  }, [
    session?.authenticated,
    loadSession,
    receiveSession,
    notesAll,
    notesPage,
  ]);
  useEffect(() => {
    if (bootstrapStarted.current) return;
    bootstrapStarted.current = true;
    const hints = initialHints.current;
    if (hints.remainingHash !== location.hash)
      history.replaceState(
        null,
        "",
        location.pathname + location.search + hints.remainingHash,
      );
    if (hints.temporaryToken) {
      setAuthBusy(true);
      void api("/auth/temporary", {
        method: "POST",
        body: { token: hints.temporaryToken },
      })
        .then(async () => {
          const next = await loadSession();
          const back = authorizationReturn(location.search);
          if (next?.authenticated && back) location.assign(back);
        })
        .catch(async (e) => {
          await loadSession();
          setAuthError((e as Error).message);
        })
        .finally(() => {
          setAuthBusy(false);
          hints.temporaryToken = "";
        });
    } else void loadSession();
    const view = new URLSearchParams(location.search).get("view");
    if (view === "audit") {
      setSection("more");
      setMorePage("audit");
    }
    const stored = document.documentElement.dataset.palette;
    if (palettes.some((p) => p.id === stored)) setPalette(stored as Palette);
  }, [loadSession]);
  useEffect(() => {
    document.documentElement.dataset.palette = palette;
    try {
      localStorage.setItem("powerlinux.theme", palette);
    } catch {}
  }, [palette]);
  useEffect(() => {
    if (!session?.authenticated) return;
    void refresh();
    const timer = setInterval(() => void refresh(), 15000);
    const visible = () => {
      if (document.visibilityState === "visible") void refresh();
    };
    document.addEventListener("visibilitychange", visible);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [session?.authenticated, refresh]);
  useEffect(() => {
    if (!session?.authenticated) return;
    const channel = new EventSource("/api/v1/events");
    const invalidated = () => {
      window.dispatchEvent(new CustomEvent("powerlinux:invalidate"));
      clearTimeout(refreshTimer.current);
      refreshTimer.current = setTimeout(() => void refresh(), 150);
    };
    channel.onopen = () => setConnection("online");
    channel.onerror = () => setConnection("offline");
    channel.onmessage = invalidated;
    [
      "invalidate",
      "change",
      "notes.changed",
      "note.updated",
      "update",
      "task.updated",
      "audit.updated",
      "notes_changed",
      "tasks_changed",
      "audit_changed",
      "resource_changed",
    ].forEach((event) => channel.addEventListener(event, invalidated));
    return () => {
      channel.close();
      clearTimeout(refreshTimer.current);
    };
  }, [session?.authenticated, refresh]);
  useEffect(() => {
    const shortcut = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setSearchOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", shortcut);
    return () => window.removeEventListener("keydown", shortcut);
  }, []);
  const signIn = async (values: AuthValues): Promise<boolean> => {
    setAuthBusy(true);
    setAuthError("");
    try {
      await api(session?.initialized ? "/auth/login" : "/auth/setup", {
        method: "POST",
        body: values,
      });
      if (!session?.initialized)
        await api("/auth/login", {
          method: "POST",
          body: { username: values.username, password: values.password },
        });
      const authenticated = await loadSession();
      if (authenticated?.authenticated) {
        const back = authorizationReturn(location.search);
        if (back) location.assign(back);
      }
      return authenticated?.authenticated === true;
    } catch (e) {
      setAuthError((e as Error).message);
      return false;
    } finally {
      setAuthBusy(false);
    }
  };
  const logout = async () => {
    try {
      await api("/auth/logout", { method: "POST" });
      setNotes([]);
      setTasks([]);
      setAudit([]);
      setStatus(undefined);
      setEditorReady(false);
      setNoteOpen(false);
      setHasDraft(false);
      await loadSession();
    } catch (e) {
      setAuthError((e as Error).message);
    }
  };
  const openNote = (note?: Note) => {
    if (editorReady && selectedNote?.id === note?.id) {
      setNoteOpen(true);
      return;
    }
    if (hasDraft) {
      setDraftWarning(true);
      setNoteOpen(true);
      return;
    }
    setSelectedNote(note);
    setEditorKey((k) => k + 1);
    setEditorReady(true);
    setNoteOpen(true);
  };
  const noteSaved = useCallback(
    (note?: Note) => {
      if (note) setSelectedNote(note);
      void refresh();
    },
    [refresh],
  );
  const displayedNotes = notes
    .filter((n) => n.archived === archived)
    .sort(
      (a, b) =>
        Number(b.pinned) - Number(a.pinned) ||
        b.updated_at.localeCompare(a.updated_at),
    );
  const recentTasks = [...tasks].sort(
    (a, b) =>
      Number(taskState(b) === "running") - Number(taskState(a) === "running") ||
      b.created_at.localeCompare(a.created_at),
  );
  const failedTasks = recentTasks.filter((t) => taskState(t) === "failed");
  const rootClass = `app palette-${palette}`;
  return (
    <InternationalizationProvider locale="zh-CN" messages={{ "zh-CN": zhCN }}>
      <Theme
        theme={neutralTheme}
        mode={palette === "space" || palette === "pine" ? "dark" : "light"}
      >
        <div className={rootClass} key={sessionBoundary.current}>
          {!session?.authenticated ? (
            <Auth
              initialized={session?.initialized}
              panel={session?.panel}
              initialEntry={initialHints.current.loginEntry}
              loading={!session && !authError}
              busy={authBusy}
              error={authError}
              onSubmit={signIn}
              retry={() => void loadSession()}
            />
          ) : (
            <>
              <a className="skip-link" href="#main">
                跳至内容
              </a>
              <header className="topbar">
                <button
                  className="brand"
                  onClick={() => {
                    setSection("overview");
                    setNotesAll(false);
                  }}
                  aria-label={`${session?.panel?.panel_name ?? "PowerLinux"} 概览`}
                >
                  <span className="brand-mark">
                    <i />
                    <i />
                    <i />
                    <i />
                  </span>
                  <strong>{session?.panel?.panel_name ?? "PowerLinux"}</strong>
                </button>
                <div className="host-pill">
                  <span
                    className={`status-dot ${connection === "online" && !errors.system ? "online" : "offline"}`}
                  />
                  <span>{status?.hostname ?? "当前主机"}</span>
                  <small>
                    {errors.system
                      ? "数据读取失败"
                      : connection === "online"
                        ? "在线"
                        : connection === "connecting"
                          ? "连接中"
                          : "实时通道断开"}
                  </small>
                </div>
                <div className="top-actions">
                  <Button
                    label="全局搜索"
                    icon={<Icon name="search" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setSearchOpen(true)}
                  />
                  <Button
                    label="最近任务"
                    icon={<Icon name="tasks" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setTaskOpen(true)}
                  />
                  <button
                    className="user-badge"
                    title={session.user?.name}
                    aria-label="账户设置"
                    onClick={() => {
                      setSection("more");
                      setMorePage("security");
                    }}
                  >
                    {(session.user?.name ?? "U").slice(0, 1).toUpperCase()}
                  </button>
                </div>
              </header>
              <main id="main" className="main">
                <div className="page-heading">
                  <div>
                    <span className="eyebrow">
                      {section === "overview" ? "WORKSPACE" : "POWERLINUX"}
                    </span>
                    <h1>
                      {section === "overview"
                        ? notesAll
                          ? "共享便签"
                          : "主机概览"
                        : nav.find((n) => n.id === section)?.label}
                    </h1>
                    <p>
                      {section === "overview"
                        ? "状态、任务与共享信息，在一处查看。"
                        : section === "sites"
                          ? "网站、域名与证书管理。"
                          : section === "software"
                            ? "软件、数据库与容器管理。"
                            : section === "files"
                              ? "文件与配置管理。"
                              : "主机管理、接入与个人设置。"}
                    </p>
                  </div>
                  <div className="button-row">
                    <Button
                      label="刷新数据"
                      icon={<Icon name="refresh" />}
                      variant="ghost"
                      isLoading={loading}
                      onClick={() => void refresh()}
                    />
                    {section === "overview" && canWriteNotes && (
                      <Button
                        label="新建便签"
                        icon={<Icon name="plus" />}
                        variant="primary"
                        onClick={() => openNote()}
                      />
                    )}
                  </div>
                </div>
                {authError && (
                  <div className="notice danger" role="alert">
                    {authError}
                  </div>
                )}
                {hasDraft && !noteOpen && (
                  <div className="notice warning" role="status">
                    便签草稿已保留。
                    <Button
                      label="继续编辑草稿"
                      variant="ghost"
                      onClick={() => setNoteOpen(true)}
                    />
                  </div>
                )}
                {draftWarning && (
                  <div className="notice warning" role="alert">
                    请先保存或放弃当前便签草稿，再打开其他便签。
                    <Button
                      label="知道了"
                      variant="ghost"
                      onClick={() => setDraftWarning(false)}
                    />
                  </div>
                )}
                {connection === "offline" && (
                  <div className="notice warning" role="status">
                    实时更新已断开，保留最近快照。每 15
                    秒尝试读取，恢复后自动同步。
                  </div>
                )}
                {section === "overview" &&
                  (notesAll ? (
                    <Card className="panel-card">
                      <div className="card-heading">
                        <h2>
                          全部便签{" "}
                          <span className="status-chip neutral">
                            本页 {displayedNotes.length} 条
                          </span>
                        </h2>
                        <div className="button-row">
                          <Button
                            label={archived ? "查看未归档" : "查看归档"}
                            onClick={() => {
                              setArchived(!archived);
                              setNotesPage(0);
                            }}
                          />
                          <Button
                            label="返回概览"
                            variant="ghost"
                            onClick={() => {
                              setNotesAll(false);
                              setArchived(false);
                              setNotesPage(0);
                            }}
                          />
                        </div>
                      </div>
                      {errors.notes && (
                        <div className="notice danger">{errors.notes}</div>
                      )}
                      <div className="notes-grid">
                        {displayedNotes.map((n) => (
                          <NoteTile
                            key={n.id}
                            note={n}
                            open={() => openNote(n)}
                          />
                        ))}
                      </div>
                      {loading && (
                        <p className="muted" role="status">
                          正在读取共享便签…
                        </p>
                      )}
                      {!displayedNotes.length && !errors.notes && !loading && (
                        <Empty
                          icon="note"
                          title={
                            notesPage > 0
                              ? "这一页没有便签"
                              : archived
                                ? "暂无归档便签"
                                : "还没有共享便签"
                          }
                          detail="记录待办或交接信息，人和已授权的 AI 都可编辑。"
                        />
                      )}
                      <div className="pagination" aria-label="便签分页">
                        <span className="muted">
                          第 {notesPage + 1} 页 · 每页 20 条
                        </span>
                        <div className="button-row">
                          <Button
                            label="上一页便签"
                            isDisabled={notesPage === 0 || loading}
                            onClick={() => {
                              setNotes([]);
                              setNotesPage((p) => Math.max(0, p - 1));
                            }}
                          />
                          <Button
                            label="下一页便签"
                            isDisabled={displayedNotes.length < 20 || loading}
                            onClick={() => {
                              setNotes([]);
                              setNotesPage((p) => p + 1);
                            }}
                          />
                        </div>
                      </div>
                    </Card>
                  ) : (
                    <div className="overview-grid">
                      <Card className="panel-card primary-card system-card">
                        <CardTitle icon="overview" title="主机总览">
                          <span
                            className={`status-chip ${errors.system ? "warning" : "neutral"}`}
                          >
                            {errors.system ? "快照过期" : "主机用量"}
                          </span>
                        </CardTitle>
                        {errors.system && (
                          <div className="notice danger" role="alert">
                            {errors.system}
                          </div>
                        )}
                        <div className="host-summary">
                          <strong>
                            {status?.hostname ??
                              (loading ? "正在读取…" : "主机信息未提供")}
                          </strong>
                          <span>
                            {[status?.os, status?.architecture]
                              .filter(Boolean)
                              .join(" · ") || ""}
                          </span>
                        </div>
                        <div className="metrics">
                          <Metric
                            label="CPU"
                            value={
                              status?.cpu_percent == null
                                ? "—"
                                : `${status.cpu_percent.toFixed(1)}%`
                            }
                            ratio={status?.cpu_percent}
                            hint={
                              status?.cpu_cores
                                ? `${status.cpu_cores} 个逻辑核心`
                                : "主机 CPU 总用量"
                            }
                          />
                          <Metric
                            label="内存"
                            value={bytes(status?.memory?.used_bytes)}
                            ratio={
                              status?.memory
                                ? (status.memory.used_bytes /
                                    status.memory.total_bytes) *
                                  100
                                : undefined
                            }
                            hint={
                              status?.memory
                                ? `共 ${bytes(status.memory.total_bytes)}`
                                : undefined
                            }
                          />
                          <Metric
                            label="磁盘"
                            value={bytes(status?.disk?.used_bytes)}
                            ratio={
                              status?.disk
                                ? (status.disk.used_bytes /
                                    status.disk.total_bytes) *
                                  100
                                : undefined
                            }
                            hint={
                              status?.disk
                                ? `共 ${bytes(status.disk.total_bytes)}`
                                : undefined
                            }
                          />
                        </div>
                        <div className="card-foot">
                          <span>已运行 {duration(status?.uptime_seconds)}</span>
                          <time
                            title={fullDate(
                              status?.updated_at ??
                                status?.sampled_at ??
                                lastLoaded,
                            )}
                          >
                            更新于{" "}
                            {fullDate(
                              status?.updated_at ??
                                status?.sampled_at ??
                                lastLoaded,
                            )}
                          </time>
                        </div>
                      </Card>
                      <Card className="panel-card primary-card notes-card">
                        <CardTitle icon="note" title="共享便签">
                          <button
                            className="text-button"
                            onClick={() => {
                              setNotes([]);
                              setNotesPage(0);
                              setNotesAll(true);
                            }}
                          >
                            查看全部 <Icon name="arrow" size={14} />
                          </button>
                        </CardTitle>
                        {errors.notes ? (
                          <div className="notice danger" role="alert">
                            {errors.notes}
                          </div>
                        ) : displayedNotes.length ? (
                          <div className="notes-preview">
                            {displayedNotes.slice(0, 3).map((n) => (
                              <NoteTile
                                key={n.id}
                                note={n}
                                open={() => openNote(n)}
                              />
                            ))}
                          </div>
                        ) : (
                          <Empty
                            icon="note"
                            title="留下一条共享信息"
                            detail="记录待办或交接信息，人和已授权的 AI 都可编辑。"
                          >
                            {canWriteNotes && (
                              <Button
                                label="创建第一条便签"
                                variant="ghost"
                                icon={<Icon name="plus" />}
                                onClick={() => openNote()}
                              />
                            )}
                          </Empty>
                        )}
                      </Card>
                      <Card className="panel-card secondary-card">
                        <CardTitle icon="tasks" title="最近任务">
                          <button
                            className="text-button"
                            onClick={() => setTaskOpen(true)}
                          >
                            查看全部 <Icon name="arrow" size={14} />
                          </button>
                        </CardTitle>
                        {errors.tasks ? (
                          <div className="notice danger" role="alert">
                            {errors.tasks}
                          </div>
                        ) : recentTasks.length ? (
                          <div className="rows">
                            {recentTasks.slice(0, 3).map((t) => (
                              <TaskRow
                                key={t.id}
                                task={t}
                                open={() => {
                                  setSelectedTask(t);
                                  setTaskOpen(true);
                                }}
                              />
                            ))}
                          </div>
                        ) : (
                          <Empty
                            icon="tasks"
                            title="暂无任务"
                            detail="安装、备份和升级的进度将显示在这里。"
                          />
                        )}
                      </Card>
                      <Card className="panel-card secondary-card">
                        <CardTitle icon="events" title="待处理事件">
                          <span className="count">{failedTasks.length}</span>
                        </CardTitle>
                        {errors.tasks ? (
                          <div className="notice warning">
                            任务状态暂时无法读取。
                          </div>
                        ) : failedTasks.length ? (
                          <div className="rows">
                            {failedTasks.slice(0, 3).map((t) => (
                              <button
                                className="event-row"
                                key={t.id}
                                onClick={() => {
                                  setSelectedTask(t);
                                  setTaskOpen(true);
                                }}
                              >
                                <span className="event-dot" />
                                <div>
                                  <strong>
                                    {t.name ??
                                      t.operation ??
                                      t.action ??
                                      "任务"}
                                    执行失败
                                  </strong>
                                  <p>{t.error ?? "打开任务查看失败原因。"}</p>
                                </div>
                                <Icon name="arrow" size={16} />
                              </button>
                            ))}
                          </div>
                        ) : (
                          <Empty
                            icon="check"
                            title="当前没有待处理任务异常"
                            detail="这里只显示实际任务错误与提醒。"
                          />
                        )}
                      </Card>
                    </div>
                  ))}
                {visited.sites && (
                  <div hidden={section !== "sites"}>
                    <SitesView
                      capabilities={caps}
                      onExecuted={noteSaved}
                      active={section === "sites"}
                      onFiles={(root) => {
                        setFileRequest((previous) => ({
                          root,
                          sequence: (previous?.sequence ?? 0) + 1,
                        }));
                        setSection("files");
                        setVisited((previous) => ({
                          ...previous,
                          files: true,
                        }));
                      }}
                    />
                  </div>
                )}
                {visited.software && (
                  <div hidden={section !== "software"}>
                    <SoftwareView
                      capabilities={caps}
                      onExecuted={noteSaved}
                      active={section === "software"}
                    />
                  </div>
                )}
                {visited.files && (
                  <div hidden={section !== "files"}>
                    <FilesView
                      capabilities={caps}
                      onExecuted={noteSaved}
                      active={section === "files"}
                      rootRequest={fileRequest}
                    />
                  </div>
                )}
                {visited.terminal && (
                  <div hidden={section !== "more" || morePage !== "terminal"}>
                    <TerminalView
                      capabilities={caps}
                      onExecuted={noteSaved}
                      active={section === "more" && morePage === "terminal"}
                    />
                  </div>
                )}
                {selectedAction &&
                  ["sites", "software", "files"].includes(section) && (
                    <ActionBrowser
                      capabilities={caps}
                      category={section}
                      selected={selectedAction}
                      onSelect={setSelectedAction}
                      onExecuted={noteSaved}
                    />
                  )}
                {section === "more" && (
                  <>
                    {morePage === "menu" ? (
                      <div className="more-grid">
                        {[
                          {
                            id: "monitor",
                            icon: "overview",
                            title: "监控",
                            desc: "主机指标与状态",
                          },
                          {
                            id: "terminal",
                            icon: "terminal",
                            title: "终端",
                            desc: "受控主机操作",
                          },
                          {
                            id: "schedules",
                            icon: "tasks",
                            title: "计划任务",
                            desc: "自动执行与任务进度",
                          },
                          {
                            id: "security",
                            icon: "shield",
                            title: "安全与账户",
                            desc: "密码、两步验证与会话",
                          },
                          {
                            id: "storage",
                            icon: "files",
                            title: "备份存储",
                            desc: "备份与恢复",
                          },
                          {
                            id: "appearance",
                            icon: "settings",
                            title: "外观",
                            desc: "五套配色与主题预览",
                          },
                          {
                            id: "mcp",
                            icon: "software",
                            title: "MCP 接入",
                            desc: "客户端身份与授权范围",
                          },
                          {
                            id: "audit",
                            icon: "history",
                            title: "操作记录",
                            desc: "网页与 MCP 共用审计",
                          },
                        ].map((item) => (
                          <button
                            key={item.id}
                            className="more-tile"
                            onClick={() => setMorePage(item.id)}
                          >
                            <span className="heading-icon">
                              <Icon name={item.icon as IconName} />
                            </span>
                            <div>
                              <strong>{item.title}</strong>
                              <p>{item.desc}</p>
                            </div>
                            <Icon name="arrow" size={18} />
                          </button>
                        ))}
                        <Button
                          label="退出登录"
                          variant="ghost"
                          icon={<Icon name="logout" />}
                          onClick={() => void logout()}
                        />
                      </div>
                    ) : (
                      <>
                        <Button
                          label="返回更多"
                          variant="ghost"
                          onClick={() => setMorePage("menu")}
                        />
                        {["appearance", "mcp", "security"].includes(
                          morePage,
                        ) ? (
                          <>
                            {" "}
                            <Settings
                              page={morePage}
                              palette={palette}
                              onPalette={setPalette}
                              session={session}
                              onSession={loadSession}
                              capabilities={caps}
                            />
                            {morePage === "security" && (
                              <details className="advanced-actions">
                                <summary>其他已授权的安全与凭据操作</summary>
                                <PanelSettings
                                  capabilities={caps}
                                  onExecuted={() => {
                                    void loadSession();
                                    noteSaved();
                                  }}
                                />
                                <ActionBrowser
                                  capabilities={caps}
                                  category="security"
                                  selected={selectedAction}
                                  onSelect={setSelectedAction}
                                  onExecuted={noteSaved}
                                />
                              </details>
                            )}
                          </>
                        ) : morePage === "audit" ? (
                          <Card className="panel-card">
                            <CardTitle icon="history" title="操作记录" />
                            {errors.audit && (
                              <div className="notice danger">
                                {errors.audit}
                              </div>
                            )}
                            {audit.length ? (
                              <div className="audit-list">
                                {audit.map((entry, i) => (
                                  <div
                                    className="audit-row"
                                    key={entry.id ?? i}
                                  >
                                    <span
                                      className={`status-chip ${entry.success === false || entry.status === "failed" ? "danger" : "neutral"}`}
                                    >
                                      {entry.success === false ||
                                      entry.status === "failed"
                                        ? "失败"
                                        : "已记录"}
                                    </span>
                                    <div>
                                      <strong>{entry.action}</strong>
                                      <p>
                                        {source(entry.actor)} ·{" "}
                                        {fullDate(entry.created_at)}
                                        {(entry.error ?? entry.error_code)
                                          ? ` · ${entry.error ?? entry.error_code}`
                                          : ""}
                                      </p>
                                    </div>
                                  </div>
                                ))}
                              </div>
                            ) : (
                              <Empty icon="history" title="暂无操作记录" />
                            )}
                          </Card>
                        ) : morePage === "terminal" ? null : [
                            "monitor",
                            "schedules",
                          ].includes(morePage) ? (
                          <OperationsView
                            page={morePage as "monitor" | "schedules"}
                            capabilities={caps}
                            identityKey={JSON.stringify([
                              session?.user?.id,
                              session?.user?.kind,
                              session?.user?.scopes,
                              session?.user?.resources,
                            ])}
                            onExecuted={noteSaved}
                          />
                        ) : (
                          <ActionBrowser
                            capabilities={caps}
                            category={morePage}
                            selected={selectedAction}
                            onSelect={setSelectedAction}
                            onExecuted={noteSaved}
                            error={errors.capabilities}
                          />
                        )}
                      </>
                    )}
                  </>
                )}
                {session.panel?.footer && (
                  <footer className="panel-footer">
                    {session.panel.footer}
                  </footer>
                )}
              </main>
              <nav className="bottom-nav" aria-label="主导航">
                {nav.map((item) => (
                  <button
                    key={item.id}
                    onClick={() => {
                      setSection(item.id);
                      setSelectedAction(undefined);
                      setNotesAll(false);
                      setArchived(false);
                      setMorePage("menu");
                    }}
                    className={section === item.id ? "active" : ""}
                    aria-current={section === item.id ? "page" : undefined}
                  >
                    <Icon name={item.icon} size={21} />
                    <span>{item.label}</span>
                  </button>
                ))}
              </nav>
              {editorReady && (
                <NoteEditor
                  key={editorKey}
                  note={
                    selectedNote
                      ? (notes.find((n) => n.id === selectedNote.id) ??
                        selectedNote)
                      : undefined
                  }
                  open={noteOpen}
                  onClose={() => setNoteOpen(false)}
                  onSaved={noteSaved}
                  onDraftChange={setHasDraft}
                  canWrite={canWriteNotes}
                  canDelete={canDeleteNotes}
                />
              )}
              <Dialog
                isOpen={taskOpen}
                onOpenChange={setTaskOpen}
                width={720}
                padding={6}
                aria-label="任务列表"
              >
                <div className="dialog-title">
                  <h2>{selectedTask ? "任务详情" : "最近任务"}</h2>
                  <Button
                    label="关闭任务"
                    isIconOnly
                    icon={<Icon name="close" />}
                    variant="ghost"
                    onClick={() => {
                      setTaskOpen(false);
                      setSelectedTask(undefined);
                    }}
                  />
                </div>
                {selectedTask ? (
                  <>
                    <TaskRow
                      task={
                        tasks.find((t) => t.id === selectedTask.id) ??
                        selectedTask
                      }
                    />
                    <p className="muted">任务 ID · {selectedTask.id}</p>
                    {selectedTask.error && (
                      <div className="notice danger">{selectedTask.error}</div>
                    )}
                    <pre className="json-result">
                      {JSON.stringify(
                        tasks.find((t) => t.id === selectedTask.id) ??
                          selectedTask,
                        null,
                        2,
                      )}
                    </pre>
                    <Button
                      label="返回任务列表"
                      onClick={() => setSelectedTask(undefined)}
                    />
                  </>
                ) : (
                  <div className="rows">
                    {recentTasks.map((t) => (
                      <TaskRow
                        key={t.id}
                        task={t}
                        open={() => setSelectedTask(t)}
                      />
                    ))}
                    {!recentTasks.length && (
                      <Empty icon="tasks" title="暂无任务" />
                    )}
                  </div>
                )}
              </Dialog>
              <Dialog
                isOpen={searchOpen}
                onOpenChange={setSearchOpen}
                width={640}
                padding={6}
                aria-label="全局搜索"
              >
                <div className="dialog-title">
                  <h2>搜索页面与资源</h2>
                  <Button
                    label="关闭搜索"
                    icon={<Icon name="close" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setSearchOpen(false)}
                  />
                </div>
                <TextInput
                  label="搜索"
                  isLabelHidden
                  hasAutoFocus
                  value={query}
                  onChange={setQuery}
                  placeholder="页面、便签、软件与操作…"
                  startIcon={<Icon name="search" />}
                />
                <div className="search-results">
                  {nav
                    .filter((n) => n.label.includes(query))
                    .map((n) => (
                      <button
                        key={n.id}
                        onClick={() => {
                          setSection(n.id);
                          setSearchOpen(false);
                          setMorePage("menu");
                        }}
                      >
                        <Icon name={n.icon} />
                        <span>{n.label}</span>
                        <small>页面</small>
                      </button>
                    ))}
                  {notes
                    .filter(
                      (n) =>
                        !n.archived &&
                        (n.title.includes(query) || n.content.includes(query)),
                    )
                    .slice(0, 8)
                    .map((n) => (
                      <button
                        key={n.id}
                        onClick={() => {
                          setSearchOpen(false);
                          openNote(n);
                        }}
                      >
                        <Icon name="note" />
                        <span>{n.title}</span>
                        <small>便签</small>
                      </button>
                    ))}
                  {caps
                    .filter(
                      (c) =>
                        c.name.includes(query) ||
                        c.description?.includes(query),
                    )
                    .slice(0, 15)
                    .map((c) => (
                      <button
                        key={c.name}
                        onClick={() => {
                          setSelectedAction(c.name);
                          setSection("software");
                          setSearchOpen(false);
                        }}
                      >
                        <Icon name="software" />
                        <span>{c.description || c.name}</span>
                        <small>动作</small>
                      </button>
                    ))}
                </div>
                <p className="muted">
                  搜索用于导航。选择动作后，在操作页面填写参数并执行。
                </p>
              </Dialog>
            </>
          )}
        </div>
      </Theme>
    </InternationalizationProvider>
  );
}
function NoteTile({ note, open }: { note: Note; open: () => void }) {
  return (
    <button className="note-tile" onClick={open}>
      <div className="note-tile-title">
        <strong>{note.title}</strong>
        {note.pinned && <Icon name="pin" size={15} />}
      </div>
      <p>{note.content || "暂无正文"}</p>
      <div className="note-tile-meta">
        <span>{source(note.updated_by)}</span>
        <time title={fullDate(note.updated_at)}>
          {fullDate(note.updated_at)}
        </time>
      </div>
    </button>
  );
}
function TaskRow({ task, open }: { task: Task; open?: () => void }) {
  const state = taskState(task);
  const content = (
    <>
      <span className={`task-icon ${state}`}>
        <Icon
          name={
            state === "failed"
              ? "events"
              : state === "running"
                ? "refresh"
                : "tasks"
          }
          size={17}
        />
      </span>
      <div className="task-info">
        <strong>{task.name ?? task.operation ?? task.action ?? "任务"}</strong>
        <p>
          {task.target ?? task.resource ?? source(task.actor)} ·{" "}
          {fullDate(task.created_at)}
        </p>
        {task.progress !== undefined && (
          <progress max={100} value={task.progress} aria-label="任务进度" />
        )}
      </div>
      <span
        className={`status-chip ${state === "failed" ? "danger" : state === "running" ? "accent" : "neutral"}`}
      >
        {stateText[state] ?? state}
      </span>
    </>
  );
  return open ? (
    <button className="task-row" onClick={open}>
      {content}
    </button>
  ) : (
    <div className="task-row">{content}</div>
  );
}
function Auth({
  panel,
  initialEntry,
  initialized,
  loading,
  busy,
  error,
  onSubmit,
  retry,
}: {
  panel?: Session["panel"];
  initialEntry: string;
  initialized?: boolean;
  loading: boolean;
  busy: boolean;
  error: string;
  onSubmit: (v: AuthValues) => Promise<boolean>;
  retry: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [otp, setOTP] = useState("");
  const [token, setToken] = useState("");
  const [entry, setEntry] = useState(initialEntry);
  const [captcha, setCaptcha] = useState<{
    id: string;
    image: string;
    expires_at: string;
  }>();
  const [answer, setAnswer] = useState("");
  const [captchaError, setCaptchaError] = useState("");
  const [captchaBusy, setCaptchaBusy] = useState(false);
  const refreshCaptcha = useCallback(async () => {
    setCaptchaBusy(true);
    setAnswer("");
    setCaptchaError("");
    try {
      const next = await api<{ id: string; image: string; expires_at: string }>(
        "/auth/captcha",
      );
      if (!next.image.startsWith("data:image/png;base64,"))
        throw new Error("验证码图像格式无效。");
      setCaptcha(next);
    } catch (e) {
      setCaptcha(undefined);
      setCaptchaError((e as Error).message);
    } finally {
      setCaptchaBusy(false);
    }
  }, []);
  useEffect(() => {
    if (initialized && panel?.captcha_enabled) void refreshCaptcha();
    else setCaptcha(undefined);
  }, [initialized, panel?.captcha_enabled, refreshCaptcha]);
  useEffect(() => {
    if (!captcha) return;
    const remaining = Date.parse(captcha.expires_at) - Date.now();
    if (!Number.isFinite(remaining)) return;
    const timer = setTimeout(
      () => void refreshCaptcha(),
      Math.max(1000, remaining),
    );
    return () => clearTimeout(timer);
  }, [captcha, refreshCaptcha]);
  return (
    <main className="auth-shell">
      <div className="auth-brand">
        <span className="brand-mark">
          <i />
          <i />
          <i />
          <i />
        </span>
        <strong>{panel?.panel_name ?? "PowerLinux"}</strong>
      </div>
      <Card className="auth-card panel-card">
        <span className="eyebrow">YOUR LINUX WORKSPACE</span>
        <h1>{initialized === false ? "初始化面板" : "欢迎回来"}</h1>
        <p className="muted">
          {initialized === false
            ? "创建管理员账户，开始管理你的主机。"
            : "登录后查看状态、任务与共享信息。"}
        </p>
        {error && (
          <div className="notice danger" role="alert">
            {error}
            {initialized === undefined && (
              <Button label="重新连接" onClick={retry} />
            )}
          </div>
        )}
        {loading ? (
          <p role="status" className="loading-message">
            正在连接面板服务…
          </p>
        ) : (
          initialized !== undefined && (
            <form
              className="form-stack"
              onSubmit={(e) => {
                e.preventDefault();
                void onSubmit({
                  username,
                  password,
                  otp,
                  setup_token: token,
                  ...(initialized
                    ? {
                        login_entry: entry,
                        ...(panel?.captcha_enabled
                          ? { captcha_id: captcha?.id, captcha_answer: answer }
                          : {}),
                      }
                    : {}),
                }).then((success) => {
                  if (!success && panel?.captcha_enabled) void refreshCaptcha();
                });
              }}
            >
              <TextInput
                label="用户名"
                value={username}
                onChange={setUsername}
                isRequired
                autoComplete="username"
                hasAutoFocus
                htmlName="username"
              />
              <TextInput
                label="密码"
                type="password"
                value={password}
                onChange={setPassword}
                isRequired
                autoComplete={initialized ? "current-password" : "new-password"}
                htmlName="password"
              />
              {initialized ? (
                <TextInput
                  label="两步验证码"
                  value={otp}
                  onChange={setOTP}
                  isOptional
                  autoComplete="one-time-code"
                  description="已启用两步验证时填写。"
                />
              ) : (
                <TextInput
                  label="初始化令牌"
                  type="password"
                  value={token}
                  onChange={setToken}
                  isRequired
                  autoComplete="off"
                  description="从服务器数据目录的 bootstrap-token 文件读取；不会通过网页公开。"
                />
              )}
              {initialized && panel?.custom_login_entry && (
                <TextInput
                  label="登录入口"
                  type="password"
                  value={entry}
                  onChange={setEntry}
                  autoComplete="off"
                  description="使用管理员提供的入口链接，或填写入口名称。"
                />
              )}
              {initialized && panel?.captcha_enabled && (
                <div className="captcha-field">
                  {captcha && <img src={captcha.image} alt="登录图形验证码" />}
                  <Button
                    label="刷新图形验证码"
                    variant="ghost"
                    isLoading={captchaBusy}
                    onClick={() => void refreshCaptcha()}
                  />
                  <TextInput
                    label="图形验证码"
                    value={answer}
                    onChange={setAnswer}
                    isRequired
                    autoComplete="off"
                  />
                  {captchaError && (
                    <div className="notice danger" role="alert">
                      {captchaError}
                    </div>
                  )}
                </div>
              )}
              {panel?.maintenance && (
                <div className="notice warning">
                  面板正在维护，暂不能建立新的登录会话。
                </div>
              )}
              <Button
                label={initialized ? "登录" : "初始化面板"}
                variant="primary"
                type="submit"
                isLoading={busy}
                isDisabled={
                  panel?.maintenance ||
                  !!(
                    initialized &&
                    panel?.captcha_enabled &&
                    (!captcha || !answer)
                  )
                }
                width="100%"
              />
            </form>
          )
        )}
        <div className="auth-footer">
          <Icon name="shield" size={16} />
          <span>一个面板 · 网页与 MCP 共用授权</span>
        </div>
      </Card>
      <p className="auth-caption">
        {panel?.footer ?? "信息清晰，操作可追溯。"}
      </p>
    </main>
  );
}
