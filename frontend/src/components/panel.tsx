"use client";
import { SectionHeader } from "./section-header";
import { replaceNavigationHash } from "@/lib/navigation";
import { useResource } from "@/lib/resource-hook";
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
import { folioTheme } from "@/themes/folio";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Dialog } from "@astryxdesign/core/Dialog";
import {
  api,
  APIError,
  Audit,
  Capability,
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
import { Overview } from "./overview";
import {
  navigate,
  resetNavigation,
  parseRoute,
  useRoute,
  usePageState,
  Section,
} from "@/lib/navigation";
import { ResourceDrawer, DetailScope } from "./resource-ui";
import { initializeFont } from "@/lib/fonts";
const ResourceSearch = dynamic(() => import("./resource-search").then(m => m.ResourceSearch), {ssr:false});
const EnvironmentView = dynamic(() => import("./environment").then(m => m.EnvironmentView), {ssr:false});
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
const ServerBrowserView = dynamic(
  () => import("./browser-apps").then((m) => m.BrowserHub),
  { ssr: false },
);

function displayPanelName(name?: string) {
  return !name || ["Folio · 折纸面板", "Folio · Server Panel", "折纸面板", "Folio"].includes(name)
    ? translate("折纸面板")
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
  {id:"environment",icon:"settings",get title(){return translate("环境与维护")},get desc(){return translate("连接、执行器与版本说明")}},
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
    id: "browser",
    icon: "browser",
    get title() {
      return translate("服务器浏览器");
    },
    get desc() {
      return translate("使用服务器网络访问网页");
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
      return translate("语言与外观");
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
    <SectionHeader icon={icon} className="card-heading">
      <h2>{title}</h2>
      {children}
    </SectionHeader>
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
  const route = useRoute();
  const section = route.section;
  const setSection = (value: string) => navigate(value as Section);
  const [morePage] = usePageState("more", "page", "menu");
  const setMorePage = (page: string) => {
    if (parseRoute(location.hash).section !== "more") return;
    navigate("more", page === "menu" ? {} : { page });
  };
  useEffect(() => {
    initializeFont();
  }, []);
  useEffect(() => {
    window.scrollTo(0, 0);
  }, [section, morePage]);
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
  const [noteOpen, setNoteOpen] = useState(false);
  const [selectedNote, setSelectedNote] = useState<Note>();
  const [editorKey, setEditorKey] = useState(0);
  const [editorReady, setEditorReady] = useState(false);
  const [hasDraft, setHasDraft] = useState(false);
  const [draftWarning, setDraftWarning] = useState(false);
  const [notesAll, setNotesAll] = useState(false);
  const [notesPage, setNotesPage] = usePageState("more", "offset", 0);
  const [archived, setArchived] = usePageState("more", "filter", false);
  const [taskPage, setTaskPage] = usePageState("more", "kind", "", true);
  const [taskID, setTaskID] = usePageState("more", "id", "", true);
  const taskOpen =
    section === "more" && morePage === "activity" && taskPage === "task";
  const selectedTask = taskID
    ? (tasks.find((task) => task.id === taskID) ?? ({ id: taskID } as Task))
    : undefined;
  const setSelectedTask = (task?: Task) => setTaskID(task?.id ?? "");
  const setTaskOpen = (value: boolean) => setTaskPage(value ? "task" : "");
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [actionPage, setActionPage] = usePageState("more", "view", "", true);
  const selectedAction = caps.some(capability=>capability.name===actionPage) ? actionPage : undefined;
  const setSelectedAction = (name?: string) => setActionPage(name ?? "");
  const hasCapability = (name: string) =>
    caps.some((capability) => capability.name === name);
  const visibleNav = nav.filter(
    (item) =>
      item.id === "overview" ||
      item.id === "more" ||
      (item.id === "sites" && hasCapability("sites_list")) ||
      (item.id === "software" &&
        [
          "plugins_catalog",
          "plugins_list",
          "hosted_apps_list",
          "browser_apps_list",
          "database_connections_list",
          "cache_connections_list",
        ].some(hasCapability)) ||
      (item.id === "files" && hasCapability("fs_roots")),
  );
  const browserApps = useResource<{items:{installed:boolean}[]}>("browser_apps_list",{},!!session?.authenticated && hasCapability("browser_apps_list"),15000);
  const visibleMoreNav = moreNav.filter((item) =>
    item.id === "monitor"
      ? hasCapability("monitor_history") || hasCapability("services_list")
      : item.id === "terminal"
        ? hasCapability("terminal_targets_list")
        : item.id === "browser"
          ? hasCapability("browser_status") && !!browserApps.data?.items.some(app => app.installed)
        : item.id === "schedules"
          ? hasCapability("schedules_list") ||
            hasCapability("notifications_channels_list")
          : true,
  );
  const refreshTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const refreshGeneration = useRef(0);
  const pageIdentity = session?.authenticated
    ? JSON.stringify([session.user?.id, session.user?.kind, session.user?.scopes, session.user?.resources])
    : "";
  useEffect(() => {
    if (!pageIdentity) return;
    const name = ["sites", "software", "files"].includes(section)
      ? section
      : section === "more" && ["terminal", "browser"].includes(morePage)
        ? morePage
        : "";
    if (name) setVisited((v) => (v[name] ? v : { ...v, [name]: true }));
  }, [section, morePage, pageIdentity]);
  const scopes = session?.user?.scopes ?? [];
  const permitted = (scope: string) =>
    scopes.includes("*") ||
    scopes.includes(scope) ||
    scopes.includes(scope.split(":")[0] + ":*");
  const canWriteNotes =
    permitted("notes:write") || caps.some((c) => c.name === "notes_update");
  const canDeleteNotes =
    permitted("notes:delete") || caps.some((c) => c.name === "notes_delete");
  const initialHash = useRef(typeof window !== "undefined" ? location.hash : "");
  const initialHints = useRef(loginHints(initialHash.current));
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
        resetNavigation();
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
  const notesPageActive =
    notesAll || (section === "more" && morePage === "notes");
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
          `/notes?limit=${notesPageActive ? 20 : 100}&offset=${notesPageActive ? notesPage * 20 : 0}`,
        ),
        api<Note[]>(
          `/notes?archived=true&limit=${notesPageActive ? 20 : 100}&offset=${notesPageActive ? notesPage * 20 : 0}`,
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
    setLoading(false);
  }, [
    session?.authenticated,
    loadSession,
    receiveSession,
    notesPageActive,
    notesPage,
  ]);
  useEffect(() => {
    if (bootstrapStarted.current) return;
    bootstrapStarted.current = true;
    const hints = initialHints.current;
    if (hints.remainingHash !== initialHash.current && location.hash === initialHash.current)
      replaceNavigationHash(hints.remainingHash);
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
  const rootClass = `app palette-${palette}`;
  return (
    <InternationalizationProvider
      locale={locale}
      messages={{ "zh-CN": zhCN, en }}
    >
      <Theme
        theme={folioTheme}
        mode={palette === "space" || palette === "pine" ? "dark" : "light"}
      >
        <div
          data-section={section}
          data-page={morePage}
          className={rootClass}
          key={sessionBoundary.current}
        >
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
              <a
                className="skip-link"
                href="#main"
                onClick={(e) => {
                  e.preventDefault();
                  document.getElementById("main")?.focus();
                }}
              >
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
                  <Button
                    label={translate("查看任务")}
                    icon={
                      <span className="activity-icon">
                        <Icon name="tasks" />
                        {recentTasks.filter((t) =>
                          ["running", "failed", "pending"].includes(
                            taskState(t),
                          ),
                        ).length > 0 && (
                          <b className="activity-badge">
                            {
                              recentTasks.filter((t) =>
                                ["running", "failed", "pending"].includes(
                                  taskState(t),
                                ),
                              ).length
                            }
                          </b>
                        )}
                      </span>
                    }
                    variant="ghost"
                    onClick={() => navigate("more", { page: "activity" })}
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
              <main id="main" tabIndex={-1} className="main">
                <div className="page-heading">
                  <div className="page-title-row">
                    {section === "more" && morePage !== "menu" && <Button label={translate("返回更多")} isIconOnly icon={<Icon name="back" />} variant="ghost" onClick={() => setMorePage("menu")} />}
                    <h1>
                      {section === "overview"
                        ? notesAll
                          ? translate("共享便签")
                          : translate("主机概览")
                        : section === "more" && morePage !== "menu"
                          ? (visibleMoreNav.find((item) => item.id === morePage)
                              ?.title ??
                            (
                              {
                                activity: translate("任务与异常"),
                                notes: translate("共享便签"),
                                actions: translate("高级工具"),
                              } as Record<string, string>
                            )[morePage] ??
                            translate("更多"))
                          : nav.find((n) => n.id === section)?.label}
                    </h1>
                  </div>
                  <div className="button-row">
                    <Button
                      label={translate("刷新数据")}
                      icon={<Icon name="refresh" />}
                      variant="ghost"
                      isLoading={loading}
                      onClick={() => {
                        window.dispatchEvent(
                          new Event("powerlinux:invalidate"),
                        );
                        void refresh();
                      }}
                    />
                    {((section === "overview" && notesAll) ||
                      (section === "more" && morePage === "notes")) &&
                      canWriteNotes && (
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
                {(section === "overview" ||
                  (section === "more" && morePage === "notes")) &&
                  (notesAll || morePage === "notes" ? (
                    <Card className="panel-card">
                      <SectionHeader icon="note" className="card-heading">
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
                              setSection("overview");
                              setNotesAll(false);
                              setArchived(false);
                              setNotesPage(0);
                            }}
                          />
                        </div>
                      </SectionHeader>
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
                    <Overview
                      status={status}
                      error={errors.system}
                      connection={connection}
                      capabilities={caps}
                    />
                  ))}
                {visited.sites && (
                  <div hidden={section !== "sites"}>
                    <DetailScope.Provider value={section === "sites"}>
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
                    </DetailScope.Provider>
                  </div>
                )}
                {visited.software && (
                  <div hidden={section !== "software"}>
                    <DetailScope.Provider value={section === "software"}>
                      <SoftwareView
                        capabilities={caps}
                        onExecuted={noteSaved}
                        active={section === "software"}
                      />
                    </DetailScope.Provider>
                  </div>
                )}
                {visited.files && (
                  <div hidden={section !== "files"}>
                    <DetailScope.Provider value={section === "files"}>
                      <FilesView
                        capabilities={caps}
                        onExecuted={noteSaved}
                        active={section === "files"}
                        rootRequest={fileRequest}
                      />
                    </DetailScope.Provider>
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
                {visited.browser && (
                  <div hidden={section !== "more" || morePage !== "browser"}>
                    <ServerBrowserView
                      active={section === "more" && morePage === "browser"}
                    />
                  </div>
                )}
                {section === "more" &&
                  !["notes", "browser"].includes(morePage) && (
                  <>
                    {morePage === "menu" ? (
                      <div className="more-groups">
                        {[
                          {
                            title: translate("运维"),
                            ids: ["monitor", "terminal", "browser", "schedules"],
                          },
                          {
                            title: translate("活动与记录"),
                            ids: ["activity", "notes", "audit"],
                          },
                          {
                            title: translate("设置与接入"),
                            ids: [
                              "environment",
                              "security",
                              "appearance",
                              "mcp",
                              "search",
                              "actions",
                            ],
                          },
                        ].map((group) => (
                          <section key={group.title}>
                            <h2>{group.title}</h2>
                            <div className="more-grid">
                              {[
                                ...visibleMoreNav,
                                {
                                  id: "activity",
                                  title: translate("任务与异常"),
                                  desc: "",
                                  icon: "tasks",
                                },
                                {
                                  id: "notes",
                                  title: translate("共享便签"),
                                  desc: "",
                                  icon: "note",
                                },
                                {
                                  id: "search",
                                  title: translate("全局搜索"),
                                  desc: "",
                                  icon: "search",
                                },
                                {
                                  id: "actions",
                                  title: translate("高级工具"),
                                  desc: "",
                                  icon: "settings",
                                },
                              ]
                                .filter((item) => group.ids.includes(item.id))
                                .map((item) => (
                                  <button
                                    key={item.id}
                                    className="more-tile"
                                    onClick={() =>
                                      item.id === "search"
                                        ? setSearchOpen(true)
                                        : setMorePage(item.id)
                                    }
                                  >
                                    <Icon name={item.icon as IconName} />
                                    <strong>{item.title}</strong>
                                    <Icon name="arrow" size={16} />
                                  </button>
                                ))}
                            </div>
                          </section>
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
                        ) : morePage === "environment" ? (
                          <EnvironmentView capabilities={caps}/>
                        ) : morePage === "activity" ? (
                          <Card className="panel-card">
                            <CardTitle
                              icon="tasks"
                              title={translate("任务与异常")}
                            />
                            {errors.tasks && (
                              <div className="notice danger">
                                {errors.tasks}
                              </div>
                            )}
                            {recentTasks.length ? (
                              <div className="rows">
                                {recentTasks.map((t) => (
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
                              />
                            )}
                          </Card>
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
                                      <strong>
                                        {actionLabel({
                                          name: entry.action,
                                          description: translate("操作"),
                                        })}
                                      </strong>
                                      <details className="technical-details">
                                        <summary>
                                          {translate("技术详情")}
                                        </summary>
                                        <code>{entry.action}</code>
                                      </details>
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
                        ) : ["terminal", "browser"].includes(morePage) ? null : [
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
                {session.panel?.footer &&
                  !["Folio · 折纸面板", "Folio · Server Panel"].includes(
                    session.panel.footer,
                  ) && (
                    <footer className="panel-footer">
                      {displayPanelName(session.panel.footer)}
                    </footer>
                  )}
              </main>
              <div id="detail-pages" />
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
              <ResourceDrawer
                open={taskOpen}
                onClose={() => {
                  setTaskOpen(false);
                  setSelectedTask(undefined);
                }}
                title={
                  selectedTask ? translate("任务详情") : translate("任务列表")
                }
              >
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
              </ResourceDrawer>
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
                  placeholder={translate("页面、域名、应用与数据库…")}
                  startIcon={<Icon name="search" />}
                />
                <div className="search-results">
                  {searchOpen && <ResourceSearch query={query} capabilities={caps} onPick={()=>setSearchOpen(false)}/>}
                  {visibleMoreNav.filter(n=>n.title.toLocaleLowerCase().includes(query.toLocaleLowerCase())).map(n=><button key={n.id} onClick={()=>{if(navigate("more",{page:n.id}))setSearchOpen(false)}}><Icon name={n.icon as IconName}/><span>{n.title}</span><small>{translate("页面")}</small></button>)}
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
                          navigate("more", { page: "actions", view: c.name });
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
          {task.name ??
            actionLabel({
              name: task.action ?? task.operation ?? "",
              description: translate("任务"),
            })}
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
        {stateText[state] ?? translate("状态未知")}
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
          <span>{translate("AI 时代的轻量化面板")}</span>
        </div>
      </Card>
      <div className="auth-language">
        <LanguageControl />
      </div>
    </main>
  );
}
