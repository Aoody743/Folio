"use client";
import { t as translate } from "@/lib/i18n";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import dynamic from "next/dynamic";
import { clearDrafts } from "@/lib/draft-cache";
import { firstReleaseCapabilities } from "@/lib/first-release-surface";
import { authorizationReturn, AuthValues, loginHints } from "@/lib/auth-hints";
import { Theme } from "@astryxdesign/core/theme";
import { InternationalizationProvider } from "@astryxdesign/core/i18n";
import en from "@astryxdesign/core/locales/en.json";
import {
  getLocale,
  initializeLocale,
  serverLocale,
  subscribeLocale,
} from "@/lib/i18n";
import { LanguageControl } from "./language-control";
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
import { TaskResult } from "./task-result";
import { actionLabel } from "@/lib/labels";
import { HostingStackSetup } from "./hosting-stack-setup";
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

function displayPanelName(name?: string) {
  return !name || name === "Folio · 折纸面板"
    ? translate("Folio · 折纸面板")
    : name;
}

export type Palette = "moon" | "space" | "violet" | "pine" | "sand";
export const palettes: { id: Palette; name: string; desc: string }[] = [
  {
    id: "moon",
    get name() {
      return translate("月白");
    },
    get desc() {
      return translate("清晰 · 浅色");
    },
  },
  {
    id: "space",
    get name() {
      return translate("深空");
    },
    get desc() {
      return translate("沉静 · 深色");
    },
  },
  {
    id: "violet",
    get name() {
      return translate("雾紫");
    },
    get desc() {
      return translate("柔和 · 浅色");
    },
  },
  {
    id: "pine",
    get name() {
      return translate("松绿");
    },
    get desc() {
      return translate("自然 · 深色");
    },
  },
  {
    id: "sand",
    get name() {
      return translate("暖砂");
    },
    get desc() {
      return translate("温暖 · 浅色");
    },
  },
];
const nav: { id: string; label: string; icon: IconName }[] = [
  {
    id: "overview",
    get label() {
      return translate("概览");
    },
    icon: "overview",
  },
  {
    id: "sites",
    get label() {
      return translate("网站");
    },
    icon: "sites",
  },
  {
    id: "software",
    get label() {
      return translate("软件");
    },
    icon: "software",
  },
  {
    id: "files",
    get label() {
      return translate("文件");
    },
    icon: "files",
  },
  {
    id: "more",
    get label() {
      return translate("更多");
    },
    icon: "more",
  },
];
const moreNav: { id: string; icon: string; title: string; desc: string }[] = [
  {
    id: "monitor",
    icon: "overview",
    get title() {
      return translate("监控");
    },
    get desc() {
      return translate("主机指标与状态");
    },
  },
  {
    id: "terminal",
    icon: "terminal",
    get title() {
      return translate("终端");
    },
    get desc() {
      return translate("受控主机操作");
    },
  },
  {
    id: "schedules",
    icon: "tasks",
    get title() {
      return translate("计划任务");
    },
    get desc() {
      return translate("自动执行与任务进度");
    },
  },
  {
    id: "security",
    icon: "shield",
    get title() {
      return translate("安全与账户");
    },
    get desc() {
      return translate("密码、两步验证与会话");
    },
  },
  {
    id: "appearance",
    icon: "settings",
    get title() {
      return translate("外观");
    },
    get desc() {
      return translate("五套配色与主题预览");
    },
  },
  {
    id: "mcp",
    icon: "software",
    get title() {
      return translate("MCP 接入");
    },
    get desc() {
      return translate("客户端身份与授权范围");
    },
  },
  {
    id: "audit",
    icon: "history",
    get title() {
      return translate("操作记录");
    },
    get desc() {
      return translate("网页与 MCP 共用审计");
    },
  },
];
const stateText: Record<string, string> = {
  get queued() {
    return translate("排队中");
  },
  get running() {
    return translate("执行中");
  },
  get succeeded() {
    return translate("执行成功");
  },
  get success() {
    return translate("执行成功");
  },
  get completed() {
    return translate("执行成功");
  },
  get failed() {
    return translate("执行失败");
  },
  get cancelled() {
    return translate("已取消");
  },
  get canceled() {
    return translate("已取消");
  },
  get verifying() {
    return translate("验证中");
  },
  get verified() {
    return translate("验证成功");
  },
  get pending() {
    return translate("待执行");
  },
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
      <div className="metric-hint">{hint ?? translate("未提供数据")}</div>
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
  const locale = useSyncExternalStore(subscribeLocale, getLocale, serverLocale);
  useEffect(() => {
    initializeLocale();
    const sync = (event: StorageEvent) => {
      if (event.key === "folio.language") initializeLocale();
    };
    window.addEventListener("storage", sync);
    return () => window.removeEventListener("storage", sync);
  }, []);
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
  const hasCapability = (name: string) =>
    caps.some((capability) => capability.name === name);
  const visibleNav = nav.filter(
    (item) =>
      item.id === "overview" ||
      item.id === "more" ||
      (item.id === "sites" && hasCapability("sites_list")) ||
      (item.id === "software" && hasCapability("plugins_catalog")) ||
      (item.id === "files" && hasCapability("fs_roots")),
  );
  const visibleMoreNav = moreNav.filter((item) =>
    item.id === "monitor"
      ? hasCapability("monitor_history") || hasCapability("services_list")
      : item.id === "terminal"
        ? hasCapability("terminal_targets_list")
        : item.id === "schedules"
          ? hasCapability("schedules_list") ||
            hasCapability("notifications_channels_list")
          : true,
  );
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
    document.title = displayPanelName(next.panel?.panel_name);
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
          result.reason instanceof Error
            ? result.reason.message
            : translate("读取失败");
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
          setCaps(firstReleaseCapabilities(list(result.value as Capability[])));
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
    <InternationalizationProvider
      locale={locale}
      messages={{ "zh-CN": zhCN, en }}
    >
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
                {translate("跳至内容")}
              </a>
              <header className="topbar">
                <button
                  className="brand"
                  onClick={() => {
                    setSection("overview");
                    setNotesAll(false);
                  }}
                  aria-label={translate("{0} 概览", [
                    displayPanelName(session?.panel?.panel_name),
                  ])}
                >
                  <img
                    className="brand-logo"
                    src="/folio-logo.png"
                    alt=""
                    aria-hidden="true"
                  />
                  <strong>
                    {displayPanelName(session?.panel?.panel_name)}
                  </strong>
                </button>
                <div className="host-pill">
                  <span
                    className={`status-dot ${connection === "online" && !errors.system ? "online" : "offline"}`}
                  />
                  <span>{status?.hostname ?? translate("当前主机")}</span>
                  <small>
                    {errors.system
                      ? translate("数据读取失败")
                      : connection === "online"
                        ? translate("在线")
                        : connection === "connecting"
                          ? translate("连接中")
                          : translate("实时通道断开")}
                  </small>
                </div>
                <div className="top-actions">
                  <LanguageControl />
                  <Button
                    label={translate("全局搜索")}
                    icon={<Icon name="search" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setSearchOpen(true)}
                  />
                  <Button
                    label={translate("最近任务")}
                    icon={<Icon name="tasks" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setTaskOpen(true)}
                  />
                  <button
                    className="user-badge"
                    title={session.user?.name}
                    aria-label={translate("账户设置")}
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
                      {section === "overview" ? "WORKSPACE" : "FOLIO"}
                    </span>
                    <h1>
                      {section === "overview"
                        ? notesAll
                          ? translate("共享便签")
                          : translate("主机概览")
                        : nav.find((n) => n.id === section)?.label}
                    </h1>
                    <p>
                      {section === "overview"
                        ? translate("状态、任务与共享信息，在一处查看。")
                        : section === "sites"
                          ? translate("网站、域名与证书管理。")
                          : section === "software"
                            ? translate("软件、数据库与容器管理。")
                            : section === "files"
                              ? translate("文件与配置管理。")
                              : translate("主机管理、接入与个人设置。")}
                    </p>
                  </div>
                  <div className="button-row">
                    <Button
                      label={translate("刷新数据")}
                      icon={<Icon name="refresh" />}
                      variant="ghost"
                      isLoading={loading}
                      onClick={() => void refresh()}
                    />
                    {section === "overview" && canWriteNotes && (
                      <Button
                        label={translate("新建便签")}
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
                    {translate("便签草稿已保留。")}
                    <Button
                      label={translate("继续编辑草稿")}
                      variant="ghost"
                      onClick={() => setNoteOpen(true)}
                    />
                  </div>
                )}
                {draftWarning && (
                  <div className="notice warning" role="alert">
                    {translate("请先保存或放弃当前便签草稿，再打开其他便签。")}
                    <Button
                      label={translate("知道了")}
                      variant="ghost"
                      onClick={() => setDraftWarning(false)}
                    />
                  </div>
                )}
                {connection === "offline" && (
                  <div className="notice warning" role="status">
                    {translate(
                      "实时更新已断开，保留最近快照。每 15 秒尝试读取，恢复后自动同步。",
                    )}
                  </div>
                )}
                {section === "overview" && !notesAll && (
                  <HostingStackSetup capabilities={caps} />
                )}
                {section === "overview" &&
                  (notesAll ? (
                    <Card className="panel-card">
                      <div className="card-heading">
                        <h2>
                          {translate("全部便签")}{" "}
                          <span className="status-chip neutral">
                            {translate("本页")}
                            {displayedNotes.length}
                            {translate("条")}
                          </span>
                        </h2>
                        <div className="button-row">
                          <Button
                            label={
                              archived
                                ? translate("查看未归档")
                                : translate("查看归档")
                            }
                            onClick={() => {
                              setArchived(!archived);
                              setNotesPage(0);
                            }}
                          />
                          <Button
                            label={translate("返回概览")}
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
                          {translate("正在读取共享便签…")}
                        </p>
                      )}
                      {!displayedNotes.length && !errors.notes && !loading && (
                        <Empty
                          icon="note"
                          title={
                            notesPage > 0
                              ? translate("这一页没有便签")
                              : archived
                                ? translate("暂无归档便签")
                                : translate("还没有共享便签")
                          }
                          detail={translate(
                            "记录待办或交接信息，人和已授权的 AI 都可编辑。",
                          )}
                        />
                      )}
                      <div
                        className="pagination"
                        aria-label={translate("便签分页")}
                      >
                        <span className="muted">
                          {translate("第")}
                          {notesPage + 1}
                          {translate("页 · 每页 20 条")}
                        </span>
                        <div className="button-row">
                          <Button
                            label={translate("上一页便签")}
                            isDisabled={notesPage === 0 || loading}
                            onClick={() => {
                              setNotes([]);
                              setNotesPage((p) => Math.max(0, p - 1));
                            }}
                          />
                          <Button
                            label={translate("下一页便签")}
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
                        <CardTitle
                          icon="overview"
                          title={translate("主机总览")}
                        >
                          <span
                            className={`status-chip ${errors.system ? "warning" : "neutral"}`}
                          >
                            {errors.system
                              ? translate("快照过期")
                              : translate("主机用量")}
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
                              (loading
                                ? translate("正在读取…")
                                : translate("主机信息未提供"))}
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
                                ? translate("{0} 个逻辑核心", [
                                    status.cpu_cores,
                                  ])
                                : translate("主机 CPU 总用量")
                            }
                          />
                          <Metric
                            label={translate("内存")}
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
                                ? translate("共 {0}", [
                                    bytes(status.memory.total_bytes),
                                  ])
                                : undefined
                            }
                          />
                          <Metric
                            label={translate("磁盘")}
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
                                ? translate("共 {0}", [
                                    bytes(status.disk.total_bytes),
                                  ])
                                : undefined
                            }
                          />
                        </div>
                        <div className="card-foot">
                          <span>
                            {translate("已运行 ")}
                            {duration(status?.uptime_seconds)}
                          </span>
                          <time
                            title={fullDate(
                              status?.updated_at ??
                                status?.sampled_at ??
                                lastLoaded,
                            )}
                          >
                            {translate("更新于")}{" "}
                            {fullDate(
                              status?.updated_at ??
                                status?.sampled_at ??
                                lastLoaded,
                            )}
                          </time>
                        </div>
                      </Card>
                      <Card
                        className={`panel-card primary-card notes-card ${displayedNotes.length ? "" : "compact-empty"}`}
                      >
                        <CardTitle icon="note" title={translate("共享便签")}>
                          <button
                            className="text-button"
                            onClick={() => {
                              setNotes([]);
                              setNotesPage(0);
                              setNotesAll(true);
                            }}
                          >
                            {translate("查看全部")}
                            <Icon name="arrow" size={14} />
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
                            title={translate("留下一条共享信息")}
                            detail={translate(
                              "记录待办或交接信息，人和已授权的 AI 都可编辑。",
                            )}
                          >
                            {canWriteNotes && (
                              <Button
                                label={translate("创建第一条便签")}
                                variant="ghost"
                                icon={<Icon name="plus" />}
                                onClick={() => openNote()}
                              />
                            )}
                          </Empty>
                        )}
                      </Card>
                      <Card
                        className={`panel-card secondary-card ${recentTasks.length ? "" : "compact-empty"}`}
                      >
                        <CardTitle icon="tasks" title={translate("最近任务")}>
                          <button
                            className="text-button"
                            onClick={() => setTaskOpen(true)}
                          >
                            {translate("查看全部")}
                            <Icon name="arrow" size={14} />
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
                            title={translate("暂无任务")}
                            detail={translate(
                              "安装、备份和升级的进度将显示在这里。",
                            )}
                          />
                        )}
                      </Card>
                      <Card
                        className={`panel-card secondary-card ${failedTasks.length ? "" : "compact-empty"}`}
                      >
                        <CardTitle
                          icon="events"
                          title={translate("待处理事件")}
                        >
                          <span className="count">{failedTasks.length}</span>
                        </CardTitle>
                        {errors.tasks ? (
                          <div className="notice warning">
                            {translate("任务状态暂时无法读取。")}
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
                                      translate("任务")}
                                    {translate("执行失败")}
                                  </strong>
                                  <p>
                                    {t.error ??
                                      translate("打开任务查看失败原因。")}
                                  </p>
                                </div>
                                <Icon name="arrow" size={16} />
                              </button>
                            ))}
                          </div>
                        ) : (
                          <Empty
                            icon="check"
                            title={translate("当前没有待处理任务异常")}
                            detail={translate("这里只显示实际任务错误与提醒。")}
                          />
                        )}
                      </Card>
                      <Card className="panel-card secondary-card">
                        <CardTitle
                          icon="overview"
                          title={translate("常用入口")}
                        />
                        <div className="rows">
                          {[
                            {
                              id: "sites",
                              icon: "sites",
                              title: translate("网站管理"),
                              detail: translate("网站、域名与证书"),
                            },
                            {
                              id: "software",
                              icon: "software",
                              title: translate("软件管理"),
                              detail: translate("服务、数据库与容器"),
                            },
                            {
                              id: "files",
                              icon: "files",
                              title: translate("文件管理"),
                              detail: translate("浏览文件与配置"),
                            },
                          ]
                            .filter((item) =>
                              visibleNav.some(
                                (navItem) => navItem.id === item.id,
                              ),
                            )
                            .map((item) => (
                              <button
                                className="event-row quick-link-row"
                                key={item.id}
                                onClick={() => {
                                  setSection(item.id);
                                  setSelectedAction(undefined);
                                  setNotesAll(false);
                                }}
                              >
                                <span className="heading-icon">
                                  <Icon
                                    name={item.icon as IconName}
                                    size={16}
                                  />
                                </span>
                                <div>
                                  <strong>{item.title}</strong>
                                  <p>{item.detail}</p>
                                </div>
                                <Icon name="arrow" size={16} />
                              </button>
                            ))}
                        </div>
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
                        {visibleMoreNav.map((item) => (
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
                          label={translate("退出登录")}
                          variant="ghost"
                          icon={<Icon name="logout" />}
                          onClick={() => void logout()}
                        />
                      </div>
                    ) : (
                      <>
                        <Button
                          label={translate("返回更多")}
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
                                <summary>
                                  {translate("其他已授权的安全与凭据操作")}
                                </summary>
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
                            <CardTitle
                              icon="history"
                              title={translate("操作记录")}
                            />
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
                                        ? translate("失败")
                                        : translate("已记录")}
                                    </span>
                                    <div>
                                      <strong>{entry.action}</strong>
                                      <p>
                                        {source(entry.actor)}·{" "}
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
                              <Empty
                                icon="history"
                                title={translate("暂无操作记录")}
                              />
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
                    {displayPanelName(session.panel.footer)}
                  </footer>
                )}
              </main>
              <nav className="bottom-nav" aria-label={translate("主导航")}>
                {visibleNav.map((item) => (
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
                aria-label={translate("任务列表")}
              >
                <div className="dialog-title">
                  <h2>
                    {selectedTask
                      ? translate("任务详情")
                      : translate("最近任务")}
                  </h2>
                  <Button
                    label={translate("关闭任务")}
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
                    <TaskResult
                      submitted={selectedTask}
                      capabilities={caps}
                      onFinished={() => void refresh()}
                    />
                    <Button
                      label={translate("返回任务列表")}
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
                      <Empty icon="tasks" title={translate("暂无任务")} />
                    )}
                  </div>
                )}
              </Dialog>
              <Dialog
                isOpen={searchOpen}
                onOpenChange={setSearchOpen}
                width={640}
                padding={6}
                aria-label={translate("全局搜索")}
              >
                <div className="dialog-title">
                  <h2>{translate("搜索页面与资源")}</h2>
                  <Button
                    label={translate("关闭搜索")}
                    icon={<Icon name="close" />}
                    isIconOnly
                    variant="ghost"
                    onClick={() => setSearchOpen(false)}
                  />
                </div>
                <TextInput
                  label={translate("搜索")}
                  isLabelHidden
                  hasAutoFocus
                  value={query}
                  onChange={setQuery}
                  placeholder={translate("页面、便签、软件与操作…")}
                  startIcon={<Icon name="search" />}
                />
                <div className="search-results">
                  {visibleNav
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
                        <small>{translate("页面")}</small>
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
                        <small>{translate("便签")}</small>
                      </button>
                    ))}
                  {caps
                    .filter(
                      (c) =>
                        c.name.includes(query) ||
                        c.description?.includes(query) ||
                        actionLabel(c)
                          .toLowerCase()
                          .includes(query.toLowerCase()),
                    )
                    .slice(0, 15)
                    .map((c) => (
                      <button
                        key={c.name}
                        onClick={() => {
                          setSelectedAction(c.name);
                          setSection("more");
                          setMorePage("actions");
                          setSearchOpen(false);
                        }}
                      >
                        <Icon name="software" />
                        <span>{actionLabel(c)}</span>
                        <small>{translate("动作")}</small>
                      </button>
                    ))}
                </div>
                <p className="muted">
                  {translate(
                    "搜索用于导航。选择动作后，在操作页面填写参数并执行。",
                  )}
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
      <p>{note.content || translate("暂无正文")}</p>
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
        <strong>
          {task.name ?? task.operation ?? task.action ?? translate("任务")}
        </strong>
        <p>
          {task.target ?? task.resource ?? source(task.actor)}·{" "}
          {fullDate(task.created_at)}
        </p>
        {task.progress !== undefined && (
          <progress
            max={100}
            value={task.progress}
            aria-label={translate("任务进度")}
          />
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
        throw new Error(translate("验证码图像格式无效。"));
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
      <div className="auth-language">
        <LanguageControl />
      </div>
      <div className="auth-brand">
        <img
          className="brand-logo"
          src="/folio-logo.png"
          alt=""
          aria-hidden="true"
        />
        <strong>{displayPanelName(panel?.panel_name)}</strong>
      </div>
      <Card className="auth-card panel-card">
        <span className="eyebrow">YOUR LINUX WORKSPACE</span>
        <h1>
          {initialized === false
            ? translate("初始化面板")
            : translate("欢迎回来")}
        </h1>
        <p className="muted">
          {initialized === false
            ? translate("创建管理员账户，开始管理你的主机。")
            : translate("登录后查看状态、任务与共享信息。")}
        </p>
        {error && (
          <div className="notice danger" role="alert">
            {error}
            {initialized === undefined && (
              <Button label={translate("重新连接")} onClick={retry} />
            )}
          </div>
        )}
        {loading ? (
          <p role="status" className="loading-message">
            {translate("正在连接面板服务…")}
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
                label={translate("用户名")}
                value={username}
                onChange={setUsername}
                isRequired
                autoComplete="username"
                hasAutoFocus
                htmlName="username"
              />
              <TextInput
                label={translate("密码")}
                type="password"
                value={password}
                onChange={setPassword}
                isRequired
                autoComplete={initialized ? "current-password" : "new-password"}
                htmlName="password"
              />
              {initialized ? (
                <TextInput
                  label={translate("两步验证码")}
                  value={otp}
                  onChange={setOTP}
                  isOptional
                  autoComplete="one-time-code"
                  description={translate("已启用两步验证时填写。")}
                />
              ) : (
                <TextInput
                  label={translate("初始化令牌")}
                  type="password"
                  value={token}
                  onChange={setToken}
                  isRequired
                  autoComplete="off"
                  description={translate(
                    "从服务器数据目录的 bootstrap-token 文件读取；不会通过网页公开。",
                  )}
                />
              )}
              {initialized && panel?.custom_login_entry && (
                <TextInput
                  label={translate("登录入口")}
                  type="password"
                  value={entry}
                  onChange={setEntry}
                  autoComplete="off"
                  description={translate(
                    "使用管理员提供的入口链接，或填写入口名称。",
                  )}
                />
              )}
              {initialized && panel?.captcha_enabled && (
                <div className="captcha-field">
                  {captcha && (
                    <img
                      src={captcha.image}
                      alt={translate("登录图形验证码")}
                    />
                  )}
                  <Button
                    label={translate("刷新图形验证码")}
                    variant="ghost"
                    isLoading={captchaBusy}
                    onClick={() => void refreshCaptcha()}
                  />
                  <TextInput
                    label={translate("图形验证码")}
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
                  {translate("面板正在维护，暂不能建立新的登录会话。")}
                </div>
              )}
              <Button
                label={
                  initialized ? translate("登录") : translate("初始化面板")
                }
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
          <span>{translate("一个面板 · 网页与 MCP 共用授权")}</span>
        </div>
      </Card>
      <p className="auth-caption">
        {panel?.footer ?? translate("信息清晰，操作可追溯。")}
      </p>
    </main>
  );
}
