"use client";
import { t as translate } from "@/lib/i18n";

import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, Capability, intentKey, Task } from "@/lib/api";
import { streamRequest } from "@/lib/api";
import { fileHash } from "@/lib/upload";
import { Page, Site, SiteProxy } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { State, Pager, ResourceDrawer, ResourceError } from "./resource-ui";
import { usePageState } from "@/lib/navigation";
import { pageItems, usePageSize } from "@/lib/collection";
import { applicationState } from "@/lib/overview";

type HostedApplication = {
  id: string;
  name: string;
  runtime: "nodejs" | "nextjs" | "go";
  runtime_instance: string;
  user: string;
  status: string;
  port: number;
  revision: number;
  running: boolean;
  upstream: string;
  updated_at: string;
  health_check?: {
    path?: string;
    status_codes?: number[];
    timeout_seconds?: number;
  };
  source_repository?: string;
  source_branch?: string;
  source_commit?: string;
};

export function HostedApplications({
  capabilities,
  active,
  onExecuted,
}: {
  capabilities: Capability[];
  active: boolean;
  onExecuted: () => void;
}) {
  const allowed = (name: string) =>
    capabilities.some((capability) => capability.name === name);
  const apps = useResource<Page<HostedApplication>>(
    "hosted_apps_list",
    {},
    active && allowed("hosted_apps_list"),
  );
  const sites = useResource<Page<Site>>(
    "sites_list",
    { limit: 500, offset: 0 },
    active && allowed("sites_list"),
  );
  const [selectedID, setSelectedID] = usePageState("software", "id", "", true);
  const [creating, setCreating] = usePageState(
    "software",
    "create",
    false,
    true,
  );
  const [detailGroup, setDetailGroup] = usePageState(
    "software",
    "group",
    "overview",
  );
  const [statusFilter, setStatusFilter] = usePageState(
    "software",
    "status",
    "",
  );
  const [query, setQuery] = usePageState("software", "q", "");
  const [offset, setOffset] = usePageState("software", "offset", 0);
  const { ref: listRef, limit } = usePageSize(68);
  const [id, setID] = useState("");
  const [name, setName] = useState("");
  const [runtime, setRuntime] = useState<"nodejs" | "nextjs" | "go">("nextjs");
  const [siteID, setSiteID] = useState("");
  const [proxyPath, setProxyPath] = useState("/");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [submitted, setSubmitted] = useState<unknown>();
  const [sourceFiles, setSourceFiles] = useState<Record<string, File>>({});
  const [purgeConfirmations, setPurgeConfirmations] = useState<
    Record<string, string>
  >({});
  const [gitSources, setGitSources] = useState<
    Record<
      string,
      { repositoryURL: string; branch: string; hostKeySHA256: string }
    >
  >({});

  const refresh = () => {
    void apps.reload();
    void sites.reload();
    onExecuted();
  };
  const submitTask = async (operation: string, input: unknown) => {
    setBusy(true);
    setError("");
    setSubmitted(undefined);
    try {
      setSubmitted(
        await action(operation, input, { idempotencyKey: intentKey() }),
      );
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : translate("提交操作失败"),
      );
    } finally {
      setBusy(false);
    }
  };
  const bind = async (app: HostedApplication) => {
    if (!siteID) return;
    if (!proxyPath.startsWith("/") || /[\r\n;{}]/.test(proxyPath)) {
      setError(translate("网站路径必须以 / 开头，且不能包含配置控制字符。"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      const site = await action<Site>("sites_get", { id: siteID });
      const proxyID = `app-${app.id}`;
      const current = Array.isArray(site.proxies) ? site.proxies : [];
      const proxies: SiteProxy[] = [
        ...current.filter((proxy) => proxy.id !== proxyID),
        {
          id: proxyID,
          path: proxyPath,
          upstream: app.upstream,
          enabled: true,
          preserve_host: true,
          websocket: true,
        },
      ];
      setSubmitted(
        await action(
          "sites_update",
          { id: site.id, expected_revision: site.revision, proxies },
          { idempotencyKey: intentKey() },
        ),
      );
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : translate("绑定网站路由失败"),
      );
    } finally {
      setBusy(false);
    }
  };
  const unbind = async (app: HostedApplication) => {
    if (!siteID) return;
    setBusy(true);
    setError("");
    try {
      const site = await action<Site>("sites_get", { id: siteID });
      const proxyID = `app-${app.id}`;
      const current = Array.isArray(site.proxies) ? site.proxies : [];
      if (!current.some((proxy) => proxy.id === proxyID)) {
        setError(translate("所选网站没有该应用的受管反代规则。"));
        return;
      }
      setSubmitted(
        await action(
          "sites_update",
          {
            id: site.id,
            expected_revision: site.revision,
            proxies: current.filter((proxy) => proxy.id !== proxyID),
          },
          { idempotencyKey: intentKey() },
        ),
      );
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : translate("解除网站路由失败"),
      );
    } finally {
      setBusy(false);
    }
  };
  const importSource = async (app: HostedApplication) => {
    const file = sourceFiles[app.id];
    if (!file) return;
    setBusy(true);
    setError("");
    try {
      const digest = await fileHash(file);
      const query = new URLSearchParams({
        id: app.id,
        expected_revision: String(app.revision),
      });
      const response = await streamRequest(
        `/api/v1/applications/source/import?${query}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/octet-stream",
            "X-Content-SHA256": digest,
          },
          body: file,
        },
      );
      setSubmitted(await response.json());
      setSourceFiles((files) => {
        const next = { ...files };
        delete next[app.id];
        return next;
      });
      refresh();
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : translate("导入应用源码失败"),
      );
    } finally {
      setBusy(false);
    }
  };
  const importGitSource = async (app: HostedApplication) => {
    const source = gitSources[app.id];
    if (!source?.repositoryURL.trim()) return;
    const input: Record<string, unknown> = {
      id: app.id,
      expected_revision: app.revision,
      repository_url: source.repositoryURL.trim(),
    };
    if (source.branch.trim()) input.branch = source.branch.trim();
    if (source.hostKeySHA256.trim())
      input.host_key_sha256 = source.hostKeySHA256.trim();
    await submitTask("hosted_apps_git_import", input);
  };

  const items = apps.data?.items ?? [];
  const siteItems = sites.data?.items ?? [];
  const canCreate = allowed("hosted_apps_create");
  const projectRequirements = (value: HostedApplication["runtime"]) =>
    value === "go"
      ? "go.mod + go.sum"
      : value === "nextjs"
        ? "package.json + package-lock.json/npm-shrinkwrap.json + scripts.build/start"
        : "package.json + package-lock.json/npm-shrinkwrap.json + scripts.start";
  const runtimeName = (value: HostedApplication["runtime"]) =>
    value === "nextjs" ? "Next.js" : value === "nodejs" ? "Node.js" : "Go";
  if (!allowed("hosted_apps_list") && !canCreate)
    return (
      <div className="notice warning">
        {translate("当前身份没有应用服务管理权限。")}
      </div>
    );

  const matching = items.filter(
    (app) =>
      (!statusFilter || applicationState(app) === statusFilter) &&
      app.name.toLowerCase().includes(query.toLowerCase()),
  );
  const paged = pageItems(matching, offset, limit);
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <div>
          <h2>{translate("应用服务")}</h2>{" "}
        </div>
        {canCreate && (
          <Button
            label={translate("部署应用")}
            variant="primary"
            onClick={() => setCreating(true)}
          />
        )}
        <Button
          label={translate("刷新")}
          variant="ghost"
          isDisabled={busy}
          onClick={refresh}
        />
      </div>
      {(error || apps.error || sites.error) && (
        <div className="notice danger" role="alert">
          {error || apps.error || sites.error}
        </div>
      )}
      {canCreate && (
        <ResourceDrawer
          open={creating}
          onClose={() => setCreating(false)}
          title={translate("部署应用")}
        >
          {error && <ApplicationActionError message={error} />}
          <section className="stack" aria-label={translate("创建应用服务")}>
            <h3>{translate("从模板创建")}</h3>
            <TextInput
              label={translate("应用编号（小写字母开头）")}
              value={id}
              onChange={setID}
              autoComplete="off"
            />
            <TextInput
              label={translate("应用名称")}
              value={name}
              onChange={setName}
              autoComplete="off"
            />
            <label className="native-field">
              {translate("运行时")}
              <select
                value={runtime}
                onChange={(event) =>
                  setRuntime(event.target.value as "nodejs" | "nextjs" | "go")
                }
              >
                <option value="nodejs">Node.js</option>
                <option value="nextjs">Next.js（Node.js）</option>
                <option value="go">Go</option>
              </select>
            </label>
            <Button
              label={translate("创建应用模板")}
              variant="primary"
              isDisabled={busy || !id || !name}
              onClick={() =>
                void submitTask("hosted_apps_create", { id, name, runtime })
              }
            />
          </section>
        </ResourceDrawer>
      )}
      <div className="list-controls">
        <TextInput
          label={translate("搜索应用")}
          isLabelHidden
          value={query}
          onChange={(value) => {
            setQuery(value);
            setOffset(0);
          }}
        />
        <select
          aria-label={translate("应用状态筛选")}
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value);
            setOffset(0);
          }}
        >
          <option value="">{translate("全部")}</option>
          <option value="running">{translate("运行中")}</option>
          <option value="stopped">{translate("已停止")}</option>
          <option value="unknown">{translate("需检查")}</option>
        </select>
      </div>
      <div ref={listRef} className="resource-table-wrap">
        <table className="resource-table">
          <thead>
            <tr>
              <th>{translate("应用")}</th>
              <th>{translate("状态")}</th>
              <th>{translate("运行环境")}</th>
            </tr>
          </thead>
          <tbody>
            {paged.items.map((app) => (
              <tr key={app.id}>
                <td>
                  <button
                    className="resource-name"
                    onClick={() => {
                      setSelectedID(app.id);
                      setDetailGroup("overview");
                    }}
                  >
                    {app.name}
                  </button>
                </td>
                <td>
                  <State value={applicationState(app)} />
                </td>
                <td>{runtimeName(app.runtime)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <Pager
        offset={paged.offset}
        limit={limit}
        total={paged.total}
        count={paged.items.length}
        loading={apps.loading}
        onOffset={setOffset}
      />
      {!matching.length && !apps.loading && !apps.error && (
        <p className="muted">{translate("暂无应用")}</p>
      )}
      {apps.loading && !apps.data ? (
        <p role="status">{translate("正在读取应用服务…")}</p>
      ) : items.length ? (
        <div className="instance-list">
          {items
            .filter((app) => app.id === selectedID)
            .map((app) => (
              <ResourceDrawer
                key={app.id}
                open={!!selectedID}
                title={app.name}
                onClose={() => setSelectedID("")}
              >
                {error && <ApplicationActionError message={error} />}
                <article className="instance-row" key={app.id}>
                  <div className="stack">
                    <strong>{app.name}</strong>
                    <p>
                      {runtimeName(app.runtime)}· {app.user}· {app.upstream}
                    </p>
                    {app.source_commit && (
                      <p className="muted">
                        Git · {app.source_repository}· {app.source_branch}·{" "}
                        {app.source_commit}
                      </p>
                    )}
                    <State
                      value={
                        app.running
                          ? "running"
                          : app.status === "running"
                            ? "stopped"
                            : app.status
                      }
                    />
                    <div
                      className="section-tabs"
                      role="tablist"
                      aria-label={translate("应用详情分类")}
                    >
                      {[
                        { id: "overview", label: translate("概况") },
                        { id: "config", label: translate("配置") },
                        ...(allowed("hosted_apps_logs")
                          ? [{ id: "logs", label: translate("日志") }]
                          : []),
                      ].map((group) => (
                        <button
                          key={group.id}
                          role="tab"
                          aria-selected={detailGroup === group.id}
                          onClick={() => setDetailGroup(group.id)}
                        >
                          {group.label}
                        </button>
                      ))}
                    </div>
                    {allowed("hosted_apps_control") &&
                      detailGroup === "config" &&
                      app.status !== "retained" &&
                      app.status !== "purging" && (
                        <details>
                          <summary>{translate("健康检查配置")}</summary>
                          <HostedHealthEditor
                            key={`${app.id}:${app.revision}`}
                            app={app}
                            busy={busy}
                            onSave={(health_check) =>
                              void submitTask("hosted_apps_control", {
                                id: app.id,
                                expected_revision: app.revision,
                                command: "health",
                                health_check,
                              })
                            }
                          />
                        </details>
                      )}
                    <div className="button-row">
                      <div className="stack" hidden={detailGroup !== "config"}>
                        {allowed("hosted_apps_source_upload") &&
                          app.status !== "retained" &&
                          app.status !== "purging" && (
                            <>
                              <label className="native-field">
                                {translate("导入项目归档（tar.gz，根目录需含")}
                                {projectRequirements(app.runtime)}）
                                <input
                                  type="file"
                                  accept=".tar.gz,.tgz,application/gzip"
                                  disabled={busy || app.running}
                                  onChange={(event) => {
                                    const file = event.currentTarget.files?.[0];
                                    setSourceFiles((files) =>
                                      file
                                        ? { ...files, [app.id]: file }
                                        : Object.fromEntries(
                                            Object.entries(files).filter(
                                              ([id]) => id !== app.id,
                                            ),
                                          ),
                                    );
                                  }}
                                />
                              </label>
                              <Button
                                label={translate("导入源码")}
                                isDisabled={
                                  busy || app.running || !sourceFiles[app.id]
                                }
                                onClick={() => void importSource(app)}
                              />
                            </>
                          )}
                        {allowed("hosted_apps_git_import") &&
                          app.status !== "retained" &&
                          app.status !== "purging" && (
                            <div
                              className="stack"
                              aria-label={translate("{0} Git 源码导入", [
                                app.name,
                              ])}
                            >
                              <p className="muted">
                                {translate(
                                  "先在软件目录安装 Git/OpenSSH，再导入公开 HTTPS 或 SSH 仓库；根目录需含",
                                )}
                                {projectRequirements(app.runtime)}
                                {translate(
                                  "。私有仓库凭据暂不支持；SSH 使用 ssh://git@host/owner/repo.git 格式，并填写服务器 SHA256 主机指纹。",
                                )}
                              </p>
                              <TextInput
                                label={translate("Git 仓库 HTTPS / SSH 地址")}
                                value={gitSources[app.id]?.repositoryURL ?? ""}
                                onChange={(repositoryURL) =>
                                  setGitSources((current) => ({
                                    ...current,
                                    [app.id]: {
                                      repositoryURL,
                                      branch: current[app.id]?.branch ?? "",
                                      hostKeySHA256:
                                        current[app.id]?.hostKeySHA256 ?? "",
                                    },
                                  }))
                                }
                                autoComplete="off"
                              />
                              <TextInput
                                label={translate("分支（留空使用 main）")}
                                value={gitSources[app.id]?.branch ?? ""}
                                onChange={(branch) =>
                                  setGitSources((current) => ({
                                    ...current,
                                    [app.id]: {
                                      repositoryURL:
                                        current[app.id]?.repositoryURL ?? "",
                                      branch,
                                      hostKeySHA256:
                                        current[app.id]?.hostKeySHA256 ?? "",
                                    },
                                  }))
                                }
                                autoComplete="off"
                              />
                              <TextInput
                                label={translate(
                                  "SSH 主机 SHA256 指纹（HTTPS 留空）",
                                )}
                                value={gitSources[app.id]?.hostKeySHA256 ?? ""}
                                onChange={(hostKeySHA256) =>
                                  setGitSources((current) => ({
                                    ...current,
                                    [app.id]: {
                                      repositoryURL:
                                        current[app.id]?.repositoryURL ?? "",
                                      branch: current[app.id]?.branch ?? "",
                                      hostKeySHA256,
                                    },
                                  }))
                                }
                                autoComplete="off"
                              />
                              <Button
                                label={translate("从 Git 导入源码")}
                                isDisabled={
                                  busy ||
                                  app.running ||
                                  !gitSources[app.id]?.repositoryURL.trim()
                                }
                                onClick={() => void importGitSource(app)}
                              />
                            </div>
                          )}
                      </div>
                      <div
                        className="stack"
                        hidden={detailGroup !== "overview"}
                      >
                        {allowed("hosted_apps_build") &&
                          app.status !== "retained" &&
                          app.status !== "purging" && (
                            <Button
                              label={translate("构建 / 更新")}
                              isDisabled={busy || app.running}
                              onClick={() =>
                                void submitTask("hosted_apps_build", {
                                  id: app.id,
                                  expected_revision: app.revision,
                                })
                              }
                            />
                          )}
                        {allowed("hosted_apps_deploy") &&
                          app.status !== "retained" &&
                          app.status !== "purging" &&
                          !app.running && (
                            <Button
                              label={translate("部署（构建并启动）")}
                              variant="primary"
                              isDisabled={busy}
                              onClick={() =>
                                void submitTask("hosted_apps_deploy", {
                                  id: app.id,
                                  expected_revision: app.revision,
                                })
                              }
                            />
                          )}
                        {allowed("hosted_apps_control") &&
                          app.status !== "retained" &&
                          app.status !== "purging" && (
                            <>
                              <Button
                                label={
                                  app.running
                                    ? translate("停止")
                                    : translate("启动")
                                }
                                isDisabled={
                                  busy ||
                                  (app.status !== "built" && !app.running)
                                }
                                onClick={() =>
                                  void submitTask("hosted_apps_control", {
                                    id: app.id,
                                    command: app.running ? "stop" : "start",
                                    expected_revision: app.revision,
                                  })
                                }
                              />
                              <Button
                                label={translate("重启")}
                                isDisabled={busy || !app.running}
                                onClick={() =>
                                  void submitTask("hosted_apps_control", {
                                    id: app.id,
                                    command: "restart",
                                    expected_revision: app.revision,
                                  })
                                }
                              />
                              <Button
                                label={translate("回退上一版本")}
                                variant="ghost"
                                isDisabled={busy || app.running}
                                onClick={() =>
                                  void submitTask("hosted_apps_control", {
                                    id: app.id,
                                    command: "rollback",
                                    expected_revision: app.revision,
                                  })
                                }
                              />
                            </>
                          )}
                      </div>
                      {detailGroup === "logs" &&
                        allowed("hosted_apps_logs") && (
                          <HostedApplicationLogs id={app.id} active={active} />
                        )}
                      <details hidden={detailGroup !== "config"}>
                        <summary>{translate("高级操作")}</summary>
                        {allowed("hosted_apps_delete") &&
                          app.status !== "retained" &&
                          app.status !== "purging" && (
                            <Button
                              label={translate("停用并保留文件")}
                              variant="ghost"
                              isDisabled={busy}
                              onClick={() =>
                                void submitTask("hosted_apps_delete", {
                                  id: app.id,
                                  expected_revision: app.revision,
                                })
                              }
                            />
                          )}
                        {allowed("hosted_apps_purge") &&
                          (app.status === "retained" ||
                            app.status === "purging") && (
                            <div
                              className="stack"
                              aria-label={translate("{0} 永久删除确认", [
                                app.name,
                              ])}
                            >
                              <p className="muted">
                                {app.status === "purging"
                                  ? translate(
                                      "上次永久清理未完成。再次输入确认文本可以继续清理。",
                                    )
                                  : translate(
                                      "永久删除会清除该应用的源码、数据和隔离账户，且无法撤销。请先单独停用并保留应用。",
                                    )}
                              </p>
                              <TextInput
                                label={translate("输入 DELETE {0} 以永久删除", [
                                  app.id,
                                ])}
                                value={purgeConfirmations[app.id] ?? ""}
                                onChange={(confirmation) =>
                                  setPurgeConfirmations((current) => ({
                                    ...current,
                                    [app.id]: confirmation,
                                  }))
                                }
                                autoComplete="off"
                              />
                              <Button
                                label={translate("永久删除应用和数据")}
                                variant="ghost"
                                isDisabled={
                                  busy ||
                                  purgeConfirmations[app.id] !==
                                    `DELETE ${app.id}`
                                }
                                onClick={() =>
                                  void submitTask("hosted_apps_purge", {
                                    id: app.id,
                                    expected_revision: app.revision,
                                    confirmation: purgeConfirmations[app.id],
                                  })
                                }
                              />
                            </div>
                          )}
                      </details>
                    </div>
                    {detailGroup === "config" &&
                      allowed("sites_update") &&
                      allowed("sites_get") &&
                      siteItems.length > 0 &&
                      app.status !== "retained" &&
                      app.status !== "purging" && (
                        <div className="button-row">
                          <label className="native-field">
                            {translate("网站")}
                            <select
                              value={siteID}
                              onChange={(event) =>
                                setSiteID(event.target.value)
                              }
                            >
                              <option value="">{translate("选择网站")}</option>
                              {siteItems.map((site) => (
                                <option value={site.id} key={site.id}>
                                  {site.name}· {site.domains.join(", ")}
                                </option>
                              ))}
                            </select>
                          </label>
                          <TextInput
                            label={translate("代理路径")}
                            value={proxyPath}
                            onChange={setProxyPath}
                            autoComplete="off"
                          />
                          <Button
                            label={translate("绑定反向代理")}
                            isDisabled={busy || !siteID}
                            onClick={() => void bind(app)}
                          />
                          <Button
                            label={translate("解除反向代理")}
                            variant="ghost"
                            isDisabled={busy || !siteID}
                            onClick={() => void unbind(app)}
                          />
                        </div>
                      )}
                  </div>
                </article>
              </ResourceDrawer>
            ))}
        </div>
      ) : (
        <p className="muted">
          {translate("还没有应用。创建模板前，请先安装对应运行时。")}
        </p>
      )}
      {submitted !== undefined && (
        <ResourceDrawer
          open={submitted !== undefined}
          title={translate("任务进度")}
          onClose={() => setSubmitted(undefined)}
        >
          <TaskResult
            submitted={submitted}
            capabilities={capabilities}
            onFinished={(task: Task) => {
              if (task.status === "succeeded" || task.state === "succeeded") {
                if (task.operation === "hosted_app_create") {
                  setID("");
                  setName("");
                }
                setError("");
              } else if (task.status === "failed" || task.state === "failed") {
                setError(task.error || translate("任务失败，请检查任务日志。"));
              }
              refresh();
            }}
          />
        </ResourceDrawer>
      )}
    </Card>
  );
}

function ApplicationActionError({ message }: { message: string }) {
  return (
    <div className="notice danger" role="alert">
      <strong>{translate("操作未完成")}</strong>
      <p>{translate("请检查权限与配置后重试。")}</p>
      <details>
        <summary>{translate("技术详情")}</summary>
        <pre>{message}</pre>
      </details>
    </div>
  );
}
function HostedApplicationLogs({
  id,
  active,
}: {
  id: string;
  active: boolean;
}) {
  const logs = useResource<{ id: string; text: string }>(
    "hosted_apps_logs",
    { id, limit: 200 },
    active,
    0,
  );
  return (
    <section className="stack" aria-label={translate("应用日志")}>
      <div className="card-heading">
        <h2>{translate("日志")}</h2>
        <Button
          label={translate("刷新")}
          variant="ghost"
          isLoading={logs.loading}
          onClick={() => void logs.reload()}
        />
      </div>
      {logs.error && (
        <ResourceError
          error={logs.error}
          state={logs.state}
          retry={() => void logs.reload()}
        />
      )}
      {logs.loading && !logs.data ? (
        <p role="status">{translate("正在读取…")}</p>
      ) : logs.data ? (
        <pre className="application-log">
          {logs.data.text || translate("暂无日志")}
        </pre>
      ) : null}
    </section>
  );
}

function HostedHealthEditor({
  app,
  busy,
  onSave,
}: {
  app: HostedApplication;
  busy: boolean;
  onSave: (check: NonNullable<HostedApplication["health_check"]>) => void;
}) {
  const [path, setPath] = useState(app.health_check?.path || "/");
  const [codes, setCodes] = useState(
    app.health_check?.status_codes?.join(",") || "",
  );
  const [seconds, setSeconds] = useState(
    String(app.health_check?.timeout_seconds || 20),
  );
  const statuses = codes.trim()
    ? codes.split(",").map((value) => Number(value.trim()))
    : [];
  const timeout = Number(seconds);
  const valid =
    path.startsWith("/") &&
    !path.startsWith("//") &&
    Number.isInteger(timeout) &&
    timeout >= 1 &&
    timeout <= 300 &&
    statuses.length <= 20 &&
    statuses.every(
      (code) => Number.isInteger(code) && code >= 200 && code <= 499,
    );
  return (
    <details>
      <summary>{translate("启动健康检查")}</summary>
      <p className="muted">
        {translate(
          "默认检查 /，HTTP 2xx/3xx 通过；5xx 会等待重试，超时后报告启动失败。",
        )}
      </p>
      <TextInput
        label={translate("检查路径")}
        value={path}
        onChange={setPath}
      />
      <TextInput
        label={translate("预期状态码（逗号分隔，留空为 2xx/3xx）")}
        value={codes}
        onChange={setCodes}
      />
      <TextInput
        label={translate("启动等待时间（1–300 秒）")}
        value={seconds}
        onChange={setSeconds}
      />
      <Button
        label={translate("保存健康检查")}
        isDisabled={busy || !valid}
        onClick={() =>
          onSave({
            path,
            timeout_seconds: timeout,
            ...(statuses.length ? { status_codes: statuses } : {}),
          })
        }
      />
    </details>
  );
}
