"use client";

import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { action, Capability, intentKey, Task } from "@/lib/api";
import { SoftwareInstance } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";

type Stack = "nginx" | "openresty";
type SoftwarePage = { items?: SoftwareInstance[] };
const optionalDatabasePlugins = ["mysql", "postgresql"] as const;
const optionalDatabaseLabels: Record<(typeof optionalDatabasePlugins)[number], string> = {
  mysql: "MySQL",
  postgresql: "PostgreSQL",
};
const optionalDatabaseSetupKey = "powerlinux.optional-databases.setup-dismissed.v1";
type OptionalDatabase = (typeof optionalDatabasePlugins)[number];

const isOptionalDatabase = (plugin: string): plugin is OptionalDatabase =>
  optionalDatabasePlugins.some((candidate) => candidate === plugin);

export function HostingStackSetup({ capabilities }: { capabilities: Capability[] }) {
  const installed = useResource<SoftwarePage>(
    "plugins_list",
    {},
    capabilities.some((capability) => capability.name === "plugins_list"),
  );
  const [busy, setBusy] = useState(false);
  const [stackChoice, setStackChoice] = useState<Stack>();
  const [pending, setPending] = useState<unknown>();
  const [pendingPlugin, setPendingPlugin] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [selectedDatabases, setSelectedDatabases] = useState<Record<string, boolean>>({
    mysql: false,
    postgresql: false,
  });
  const [databaseSetupDismissed, setDatabaseSetupDismissed] = useState(false);
  const [databasePreferenceLoaded, setDatabasePreferenceLoaded] = useState(false);
  const databaseQueue = useRef<OptionalDatabase[]>([]);

  useEffect(() => {
    try {
      setDatabaseSetupDismissed(localStorage.getItem(optionalDatabaseSetupKey) === "yes");
    } catch {
      setDatabaseSetupDismissed(false);
    } finally {
      setDatabasePreferenceLoaded(true);
    }
  }, []);

  const rows = installed.data?.items ?? [];
  const web = rows.find(
    (row) =>
      ["nginx", "openresty"].includes(row.plugin_id) &&
      row.status !== "uninstalled",
  );
  const php = rows.some(
    (row) =>
      ["php", "php-apt", "php-yum"].includes(row.plugin_id) &&
      row.status !== "uninstalled",
  );
  const installedDatabases = new Set(
    rows
      .filter(
        (row) =>
          isOptionalDatabase(row.plugin_id) &&
          row.status !== "uninstalled",
      )
      .map((row) => row.plugin_id),
  );
  const missingDatabases = optionalDatabasePlugins.filter(
    (plugin) => !installedDatabases.has(plugin),
  );
  const canInstall =
    capabilities.some((capability) => capability.name === "software_plan") &&
    capabilities.some((capability) => capability.name === "software_apply");

  const install = async (plugin: string) => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const plan = (await action<Record<string, unknown>>("software_plan", {
        operation: "install",
        plugin_id: plugin,
      })) as Record<string, unknown>;
      const planID = String(plan.plan_id ?? plan.id ?? "");
      if (!planID || typeof plan.expected_revision !== "number")
        throw new Error("软件计划返回的数据不完整，请在软件页面查看安装状态。");
      const key = intentKey();
      const task = await action<unknown>(
        "software_apply",
        {
          plan_id: planID,
          expected_revision: plan.expected_revision,
          idempotency_key: key,
        },
        { idempotencyKey: key },
      );
      setPendingPlugin(plugin);
      setPending(task);
    } catch (cause) {
      databaseQueue.current = [];
      setStackChoice(undefined);
      setError(cause instanceof Error ? cause.message : "提交安装计划失败");
    } finally {
      setBusy(false);
    }
  };

  const taskFinished = (task: Task) => {
    const state = task.status ?? task.state;
    if (state === "succeeded" || state === "success" || state === "completed") {
      setPending(undefined);
      const completedPlugin = pendingPlugin;
      setPendingPlugin("");
      if (completedPlugin === stackChoice) {
        setNotice("Web 服务已安装，正在安装 PHP-FPM…");
        void installed.reload();
        void install("php-apt");
      } else if (isOptionalDatabase(completedPlugin)) {
        void installed.reload();
        const nextDatabase = databaseQueue.current.shift();
        if (nextDatabase) {
          setNotice(`${optionalDatabaseLabels[completedPlugin]} 已安装，正在安装 ${optionalDatabaseLabels[nextDatabase]}…`);
          void install(nextDatabase);
        } else {
          databaseQueue.current = [];
          setDatabaseSetupDismissed(true);
          try {
            localStorage.setItem(optionalDatabaseSetupKey, "yes");
          } catch {
            // The software page remains available if browser storage is disabled.
          }
          setNotice("所选数据库安装任务已完成。");
        }
      } else {
        void installed.reload();
        setNotice("任务已完成，正在确认安装状态…");
      }
    } else {
      databaseQueue.current = [];
      setPending(undefined);
      setPendingPlugin("");
      setStackChoice(undefined);
      setError(task.error || "安装任务未成功，请检查任务日志后重试。");
      void installed.reload();
    }
  };

  const installSelectedDatabases = () => {
    const selected = missingDatabases.filter((plugin) => selectedDatabases[plugin]);
    if (!selected.length) return;
    databaseQueue.current = selected.slice(1);
    setError("");
    setNotice("");
    void install(selected[0]);
  };

  const dismissDatabaseSetup = () => {
    databaseQueue.current = [];
    setDatabaseSetupDismissed(true);
    try {
      localStorage.setItem(optionalDatabaseSetupKey, "yes");
    } catch {
      // The software page remains available if browser storage is disabled.
    }
  };

  if (installed.loading && !installed.data) return null;
  if (!installed.data && !installed.error) return null;
  if (!web && !stackChoice && !pending && !installed.error) {
    return (
      <Card className="panel-card">
        <div className="card-heading">
          <h2>首次设置网站运行栈</h2>
          <span className="status-chip neutral">首次启动选择</span>
        </div>
        <p className="muted">
          选择 Nginx + PHP 或 OpenResty + PHP。安装后不能直接切换；如需更换，先删除当前栈的网站配置（网站文件默认保留），卸载当前栈，再安装另一套。
        </p>
        {!canInstall && (
          <div className="notice warning">
            当前身份没有软件安装权限，请使用具有软件管理权限的管理员账户完成首次设置。
          </div>
        )}
        <div className="button-row">
          <Button
            label="安装 Nginx + PHP"
            variant="primary"
            isDisabled={!canInstall || busy}
            isLoading={busy && stackChoice === "nginx"}
            onClick={() => {
              setStackChoice("nginx");
              void install("nginx");
            }}
          />
          <Button
            label="安装 OpenResty + PHP"
            isDisabled={!canInstall || busy}
            isLoading={busy && stackChoice === "openresty"}
            onClick={() => {
              setStackChoice("openresty");
              void install("openresty");
            }}
          />
        </div>
        {error && <div className="notice danger" role="alert">{error}</div>}
      </Card>
    );
  }

  if (installed.error && !web) {
    return (
      <Card className="panel-card">
        <div className="card-heading"><h2>网站运行栈状态不可用</h2></div>
        <div className="notice warning" role="alert">
          {installed.error}。请先确认 Agent 可连接，再刷新软件状态。
        </div>
        <Button label="重新读取" variant="ghost" onClick={() => void installed.reload()} />
      </Card>
    );
  }

  if (pending) {
    return (
      <Card className="panel-card">
        <div className="card-heading">
          <h2>
            {isOptionalDatabase(pendingPlugin)
              ? `${optionalDatabaseLabels[pendingPlugin]} 安装任务`
              : "网站运行栈安装任务"}
          </h2>
        </div>
        <TaskResult
          submitted={pending}
          capabilities={capabilities}
          onFinished={taskFinished}
          onObservationUnavailable={(reason) => setError(reason.message)}
        />
        {error && <div className="notice warning" role="alert">{error}</div>}
      </Card>
    );
  }

  if (web && !php) {
    return (
      <Card className="panel-card">
        <div className="card-heading">
          <h2>{web.plugin_id === "nginx" ? "Nginx" : "OpenResty"} 网站栈</h2>
          <span className="status-chip neutral">已选择</span>
        </div>
        <p className="muted">Web 服务已安装。继续安装受管 PHP-FPM 后即可创建 PHP 网站。</p>
        {notice && <div className="notice" role="status">{notice}</div>}
        {error && <div className="notice danger" role="alert">{error}</div>}
        <Button
          label="安装 PHP-FPM"
          variant="primary"
          isDisabled={!canInstall || busy}
          isLoading={busy}
          onClick={() => void install("php-apt")}
        />
      </Card>
    );
  }

  if (
    web &&
    php &&
    missingDatabases.length > 0 &&
    databasePreferenceLoaded &&
    !databaseSetupDismissed
  ) {
    return (
      <Card className="panel-card">
        <div className="card-heading">
          <h2>可选数据库</h2>
          <span className="status-chip neutral">按需安装</span>
        </div>
        <p className="muted">
          可以跳过数据库安装，也可以同时选择 MySQL 和 PostgreSQL。未选择的数据库不会安装，之后仍可在软件页面添加。
        </p>
        <div className="stack">
          {missingDatabases.map((plugin) => (
            <label className="check-label" key={plugin}>
              <input
                type="checkbox"
                checked={selectedDatabases[plugin] ?? false}
                disabled={!canInstall || busy}
                onChange={(event) =>
                  setSelectedDatabases((current) => ({
                    ...current,
                    [plugin]: event.target.checked,
                  }))
                }
              />
              <span>
                <strong>{optionalDatabaseLabels[plugin]}</strong>
                <span className="muted">独立安装和管理，不影响网站运行栈选择</span>
              </span>
            </label>
          ))}
        </div>
        {!canInstall && (
          <div className="notice warning">
            当前身份没有软件安装权限，请使用具有软件管理权限的管理员账户完成数据库安装。
          </div>
        )}
        {notice && <div className="notice" role="status">{notice}</div>}
        {error && <div className="notice danger" role="alert">{error}</div>}
        <div className="button-row">
          <Button
            label="安装所选数据库"
            variant="primary"
            isDisabled={
              !canInstall ||
              busy ||
              !missingDatabases.some((plugin) => selectedDatabases[plugin])
            }
            isLoading={busy}
            onClick={installSelectedDatabases}
          />
          <Button
            label="暂不安装"
            variant="ghost"
            isDisabled={busy}
            onClick={dismissDatabaseSetup}
          />
        </div>
      </Card>
    );
  }

  return null;
}
