"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";

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
const optionalDatabaseLabels: Record<
  (typeof optionalDatabasePlugins)[number],
  string
> = {
  mysql: "MySQL",
  postgresql: "PostgreSQL",
};
const stackSetupKey = "folio.stack-setup-dismissed";
const optionalDatabaseSetupKey =
  "powerlinux.optional-databases.setup-dismissed.v1";
type OptionalDatabase = (typeof optionalDatabasePlugins)[number];

const isOptionalDatabase = (plugin: string): plugin is OptionalDatabase =>
  optionalDatabasePlugins.some((candidate) => candidate === plugin);

export function HostingStackSetup({
  capabilities,
}: {
  capabilities: Capability[];
}) {
  const [stackDismissed, setStackDismissed] = useState(false);
  useEffect(() => {
    try {
      setStackDismissed(localStorage.getItem(stackSetupKey) === "yes");
    } catch {
      /* Optional preference. */
    }
  }, []);
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
  const [selectedDatabases, setSelectedDatabases] = useState<
    Record<string, boolean>
  >({
    mysql: false,
    postgresql: false,
  });
  const [databaseSetupDismissed, setDatabaseSetupDismissed] = useState(false);
  const [databasePreferenceLoaded, setDatabasePreferenceLoaded] =
    useState(false);
  const databaseQueue = useRef<OptionalDatabase[]>([]);

  useEffect(() => {
    try {
      setDatabaseSetupDismissed(
        localStorage.getItem(optionalDatabaseSetupKey) === "yes",
      );
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
          isOptionalDatabase(row.plugin_id) && row.status !== "uninstalled",
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
        throw new Error(
          translate("软件计划返回的数据不完整，请在软件页面查看安装状态。"),
        );
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
      setError(
        cause instanceof Error ? cause.message : translate("提交安装计划失败"),
      );
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
        setNotice(translate("Web 服务已安装，正在安装 PHP-FPM…"));
        void installed.reload();
        void install("php-apt");
      } else if (isOptionalDatabase(completedPlugin)) {
        void installed.reload();
        const nextDatabase = databaseQueue.current.shift();
        if (nextDatabase) {
          setNotice(
            translate("{0} 已安装，正在安装 {1}…", [
              optionalDatabaseLabels[completedPlugin],
              optionalDatabaseLabels[nextDatabase],
            ]),
          );
          void install(nextDatabase);
        } else {
          databaseQueue.current = [];
          setDatabaseSetupDismissed(true);
          try {
            localStorage.setItem(optionalDatabaseSetupKey, "yes");
          } catch {
            // The software page remains available if browser storage is disabled.
          }
          setNotice(translate("所选数据库安装任务已完成。"));
        }
      } else {
        void installed.reload();
        setNotice(translate("任务已完成，正在确认安装状态…"));
      }
    } else {
      databaseQueue.current = [];
      setPending(undefined);
      setPendingPlugin("");
      setStackChoice(undefined);
      setError(
        task.error || translate("安装任务未成功，请检查任务日志后重试。"),
      );
      void installed.reload();
    }
  };

  const installSelectedDatabases = () => {
    const selected = missingDatabases.filter(
      (plugin) => selectedDatabases[plugin],
    );
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
    if (stackDismissed)
      return (
        <div className="setup-resume">
          <span>{translate("网站运行环境尚未设置")}</span>
          <Button
            label={translate("继续设置")}
            variant="ghost"
            onClick={() => setStackDismissed(false)}
          />
        </div>
      );
    return (
      <Card className="panel-card setup-card">
        <SectionHeader icon="settings" className="card-heading">
          <h2>{translate("准备好创建第一个网站了吗？")}</h2>
          <span className="status-chip neutral">{translate("可选设置")}</span>
        </SectionHeader>
        <p className="muted">
          {translate("选择适合你的网站运行环境，也可以稍后到软件页面设置。")}
        </p>
        {!canInstall && (
          <div className="notice warning">
            {translate(
              "当前身份没有软件安装权限，请使用具有软件管理权限的管理员账户完成首次设置。",
            )}
          </div>
        )}
        <details className="setup-choices">
          <summary>{translate("选择网站运行环境")}</summary>
          <p className="muted">
            {translate(
              "推荐 Nginx + PHP，适合常见网站；需要 Lua 扩展时可选 OpenResty。也可以稍后到软件页面设置。",
            )}
          </p>
          <div className="button-row">
            <Button
              label={translate("安装 Nginx + PHP（推荐）")}
              variant="primary"
              isDisabled={!canInstall || busy}
              isLoading={busy && stackChoice === "nginx"}
              onClick={() => {
                setStackChoice("nginx");
                void install("nginx");
              }}
            />
            <Button
              label={translate("安装 OpenResty + PHP")}
              isDisabled={!canInstall || busy}
              isLoading={busy && stackChoice === "openresty"}
              onClick={() => {
                setStackChoice("openresty");
                void install("openresty");
              }}
            />
          </div>
          <details className="setup-details">
            <summary>{translate("更换运行栈前需要了解什么？")}</summary>
            <p className="muted">
              {translate(
                "安装后不能直接切换。更换时需要先删除当前栈的网站配置（网站文件默认保留），卸载当前栈，再安装另一套。",
              )}
            </p>
          </details>
        </details>
        <Button
          label={translate("稍后设置")}
          variant="ghost"
          onClick={() => {
            setStackDismissed(true);
            try {
              localStorage.setItem(stackSetupKey, "yes");
            } catch {
              /* Optional preference. */
            }
          }}
        />
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
      </Card>
    );
  }

  if (installed.error && !web) {
    return (
      <Card className="panel-card">
        <SectionHeader icon="settings" className="card-heading">
          <h2>{translate("网站运行栈状态不可用")}</h2>
        </SectionHeader>
        <div className="notice warning" role="alert">
          {installed.error}
          {translate("。请先确认主机特权执行器可连接，再刷新软件状态。")}
        </div>
        <Button
          label={translate("重新读取")}
          variant="ghost"
          onClick={() => void installed.reload()}
        />
      </Card>
    );
  }

  if (pending) {
    return (
      <Card className="panel-card">
        <SectionHeader icon="settings" className="card-heading">
          <h2>
            {isOptionalDatabase(pendingPlugin)
              ? translate("{0} 安装任务", [
                  optionalDatabaseLabels[pendingPlugin],
                ])
              : translate("网站运行栈安装任务")}
          </h2>
        </SectionHeader>
        <TaskResult
          submitted={pending}
          capabilities={capabilities}
          onFinished={taskFinished}
          onObservationUnavailable={(reason) => setError(reason.message)}
        />
        {error && (
          <div className="notice warning" role="alert">
            {error}
          </div>
        )}
      </Card>
    );
  }

  if (web && !php) {
    return (
      <Card className="panel-card">
        <SectionHeader icon="settings" className="card-heading">
          <h2>
            {web.plugin_id === "nginx" ? "Nginx" : "OpenResty"}
            {translate(" 网站栈")}
          </h2>
          <span className="status-chip neutral">{translate("已选择")}</span>
        </SectionHeader>
        <p className="muted">
          {translate(
            "Web 服务已安装。继续安装受管 PHP-FPM 后即可创建 PHP 网站。",
          )}
        </p>
        {notice && (
          <div className="notice" role="status">
            {notice}
          </div>
        )}
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
        <Button
          label={translate("安装 PHP-FPM")}
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
        <SectionHeader icon="settings" className="card-heading">
          <h2>{translate("可选数据库")}</h2>
          <span className="status-chip neutral">{translate("按需安装")}</span>
        </SectionHeader>
        <p className="muted">
          {translate(
            "可以跳过数据库安装，也可以同时选择 MySQL 和 PostgreSQL。未选择的数据库不会安装，之后仍可在软件页面添加。",
          )}
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
                <span className="muted">
                  {translate("独立安装和管理，不影响网站运行栈选择")}
                </span>
              </span>
            </label>
          ))}
        </div>
        {!canInstall && (
          <div className="notice warning">
            {translate(
              "当前身份没有软件安装权限，请使用具有软件管理权限的管理员账户完成数据库安装。",
            )}
          </div>
        )}
        {notice && (
          <div className="notice" role="status">
            {notice}
          </div>
        )}
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
        <div className="button-row">
          <Button
            label={translate("安装所选数据库")}
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
            label={translate("暂不安装")}
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
