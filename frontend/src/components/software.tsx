"use client";
import { useRef, useState } from "react";
import dynamic from "next/dynamic";
const ConnectionsView = dynamic(
  () => import("./connections").then((m) => m.ConnectionsView),
  { ssr: false },
);
const InstanceResources = dynamic(
  () => import("./instance-resources").then((m) => m.InstanceResources),
  { ssr: false },
);
const PHPResources = dynamic(
  () => import("./php-resources").then((m) => m.PHPResources),
  { ssr: false },
);
const HostedApplications = dynamic(
  () => import("./hosted-applications").then((m) => m.HostedApplications),
  { ssr: false },
);
import { hasInstanceResources } from "./instance-resources";
const PHPMaintenance = dynamic(
  () => import("./php-maintenance").then((m) => m.PHPMaintenance),
  { ssr: false },
);
const SupervisorLogs = dynamic(
  () => import("./supervisor-logs").then((m) => m.SupervisorLogs),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, Capability, intentKey, list, fullDate } from "@/lib/api";
import { CatalogItem, Page, SoftwareInstance } from "@/lib/resources";
import { isFirstReleaseCatalogPlugin } from "@/lib/first-release-surface";
import { useResource } from "@/lib/resource-hook";
import { ActionBrowser, ActionForm } from "./actions";
import { Icon } from "./icon";
import { TaskResult } from "./task-result";
import { SoftwareRecovery } from "./software-recovery";
import {
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
const categoryName: Record<string, string> = {
  database: "数据库",
  "database-ui": "数据库管理",
  container: "容器",
  containers: "容器",
  runtime: "运行环境",
  web: "网站服务",
  storage: "存储",
  security: "安全",
  mail: "邮件",
};
const hasInstallableDefault = (item: CatalogItem) => {
  const selected = item.versions.find((version) => version.default);
  if (!selected) return false;
  const source = selected.source;
  return !(source && typeof source === "object" && source.kind === "unavailable");
};
export function SoftwareView({
  capabilities,
  onExecuted,
  active = true,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [filter, setFilter] = useState("all");
  const [resourcePage, setResourcePage] = useState("catalog");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<CatalogItem>();
  const [instance, setInstance] = useState<SoftwareInstance>();
  const [open, setOpen] = useState(false);
  const [tab, setTab] = useState("instances");
  const [operation, setOperation] = useState<string>();
  const [other, setOther] = useState<string>();
  const [plan, setPlan] = useState<Record<string, unknown>>();
  const [planError, setPlanError] = useState("");
  const [applying, setApplying] = useState(false);
  const [submittedTask, setSubmittedTask] = useState<unknown>();
  const [submittedRecoveryID, setSubmittedRecoveryID] = useState<string>();
  const [recoverySeed, setRecoverySeed] = useState<
    { id: string; source: string } | undefined
  >();
  const planIntent = useRef<{ id: string; key: string } | undefined>(undefined);
  const catalog = useResource<Page<CatalogItem>>(
    "plugins_catalog",
    {},
    active && capabilities.some((c) => c.name === "plugins_catalog"),
  );
  const installed = useResource<
    Page<SoftwareInstance> & { availability?: string }
  >(
    "plugins_list",
    {},
    active && capabilities.some((c) => c.name === "plugins_list"),
  );
  const detail = useResource<SoftwareInstance>(
    "plugins_get",
    { id: instance?.id },
    active && !!instance && capabilities.some((c) => c.name === "plugins_get"),
  );
  const items = list(catalog.data),
    instances = list(installed.data);
  const visibleItems = items.filter(
    (item) => isFirstReleaseCatalogPlugin(item.id) && hasInstallableDefault(item),
  );
  const softwareTabs = [
    ...(capabilities.some((capability) => capability.name === "plugins_catalog")
      ? [{ id: "catalog", label: "软件目录" }]
      : []),
    ...(capabilities.some((capability) => capability.name === "database_connections_list")
      ? [{ id: "database", label: "数据库连接" }]
      : []),
    ...(capabilities.some((capability) => capability.name === "cache_connections_list")
      ? [{ id: "cache", label: "缓存空间" }]
      : []),
    ...(capabilities.some((capability) => capability.name === "hosted_apps_list")
      ? [{ id: "applications", label: "应用服务" }]
      : []),
  ];
  const activeResourcePage = softwareTabs.some((tab) => tab.id === resourcePage)
    ? resourcePage
    : softwareTabs[0]?.id;
  const planAction = capabilities.find((c) => c.name === "software_plan");
  const applyAction = capabilities.find((c) => c.name === "software_apply");
  const databaseTargetReferences = {
    target_id: instances
      .filter(
        (item) =>
          ["mysql", "mysql-community", "mariadb"].includes(item.plugin_id) &&
          item.status !== "uninstalled",
      )
      .map((item) => ({
        value: item.id,
        label: `${item.plugin_id} · ${item.instance_id ?? item.id} · ${item.status}`,
      })),
  };
  const refresh = () => {
    void catalog.reload();
    void installed.reload();
    void detail.reload();
    onExecuted();
  };
  const associated = (item: CatalogItem) =>
    instances.filter((s) => s.plugin_id === item.id || s.id === item.id);
  const compatible = (name: string) =>
    !!selected &&
    ((name === "software_service_control" && !!instance) ||
      selected.capabilities?.some(
        (c) => (typeof c === "string" ? c : c.name) === name,
      ) ||
      name.startsWith(
        selected.id.toLowerCase().replace(/[^a-z0-9_]/g, "_") + "_",
      ) ||
      name.startsWith(selected.id + "_") ||
      (["php", "php-apt", "php-yum"].includes(selected.id) &&
        /^php_/.test(name) &&
        !/^php_guard/.test(name)) ||
      (selected.id === "php-guard" && /^php_guard/.test(name)) ||
      (selected.id === "pureftp" && /^ftp_/.test(name)) ||
      (selected.id === "rsyncd" && /^rsync_/.test(name)) ||
      (selected.id === "docker" && /^docker_/.test(name)) ||
      (["mysql", "mysql-community", "mariadb", "postgresql"].includes(selected.id) &&
        /^managed_database/.test(name)));
  const detailTabs = selected
    ? [
        { id: "instances", label: "实例" },
        ...(planAction ||
        capabilities.some(
          (capability) =>
            compatible(capability.name) &&
            !/config|setting|backup|restore|dump|import/.test(capability.name),
        ) ||
        (!!instance && hasInstanceResources(instance.plugin_id))
          ? [{ id: "operations", label: "管理操作" }]
          : []),
        ...(capabilities.some(
          (capability) =>
            compatible(capability.name) && /config|setting/.test(capability.name),
        )
          ? [{ id: "config", label: "配置" }]
          : []),
        ...(capabilities.some(
          (capability) =>
            compatible(capability.name) &&
            /backup|restore|dump|import/.test(capability.name),
        )
          ? [{ id: "backup", label: "备份恢复" }]
          : []),
        ...(selected.versions?.length
          ? [{ id: "source", label: "版本与来源" }]
          : []),
      ]
    : [];
  const activeDetailTab = detailTabs.some((item) => item.id === tab)
    ? tab
    : detailTabs[0]?.id;
  const apply = async () => {
    if (!plan) return;
    const id = String(plan.plan_id ?? plan.id);
    setSubmittedRecoveryID(
      plan.operation === "install" && typeof plan.instance_id === "string"
        ? plan.instance_id
        : undefined,
    );
    setSubmittedTask(undefined);
    setRecoverySeed(undefined);
    if (planIntent.current?.id !== id)
      planIntent.current = { id, key: intentKey() };
    const key = planIntent.current.key;
    setApplying(true);
    setPlanError("");
    try {
      const submitted = await action(
        "software_apply",
        {
          plan_id: id,
          expected_revision: plan.expected_revision,
          idempotency_key: key,
        },
        { idempotencyKey: key },
      );
      setSubmittedTask(submitted);
      setPlan(undefined);
      planIntent.current = undefined;
      refresh();
    } catch (e) {
      setSubmittedRecoveryID(undefined);
      setPlanError((e as Error).message);
    } finally {
      setApplying(false);
    }
  };
  const receivePlan = (value: unknown) => {
    const next = value as Record<string, unknown>;
    setPlan(next);
    const failed =
      ["failed", "recovery_required"].includes(String(next.state ?? "")) ||
      !!next.failure ||
      !!next.error;
    if (
      failed &&
      next.operation === "install" &&
      typeof next.instance_id === "string"
    )
      setRecoverySeed({ id: next.instance_id, source: "失败的软件计划" });
  };
  return (
    <div className="stack">
      <div className="section-tabs" role="tablist" aria-label="软件资源类型">
        {softwareTabs.map((tab) => (
          <button
            role="tab"
            aria-selected={activeResourcePage === tab.id}
            key={tab.id}
            onClick={() => setResourcePage(tab.id)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      {!activeResourcePage ? (
        <Unavailable title="当前没有可用的软件管理能力" />
      ) : activeResourcePage === "applications" ? (
        <HostedApplications
          capabilities={capabilities}
          onExecuted={onExecuted}
          active={active}
        />
      ) : activeResourcePage !== "catalog" ? (
        <ConnectionsView
          key={activeResourcePage}
          kind={activeResourcePage as "database" | "cache"}
          capabilities={capabilities}
          onExecuted={onExecuted}
          active={active}
        />
      ) : (
        <Card className="panel-card">
          <div className="card-heading">
            <h2>软件与插件</h2>
            <TextInput
              label="搜索软件"
              isLabelHidden
              value={query}
              onChange={setQuery}
              placeholder="名称或功能"
              startIcon={<Icon name="search" />}
            />
          </div>
          <div className="section-tabs" role="tablist" aria-label="软件分类">
            {[
              { id: "all", label: "全部" },
              { id: "installed", label: "已安装" },
              { id: "database", label: "数据库" },
              { id: "containers", label: "容器" },
              { id: "runtime", label: "运行环境" },
              { id: "other", label: "其他" },
            ].map((t) => (
              <button
                role="tab"
                aria-selected={filter === t.id}
                key={t.id}
                onClick={() => setFilter(t.id)}
              >
                {t.label}
              </button>
            ))}
          </div>
          {catalog.error && (
            <div className="notice danger">{catalog.error}</div>
          )}
          {installed.error && (
            <div className="notice warning">
              安装状态读取失败：{installed.error}
            </div>
          )}
          {installed.data?.availability === "agent_not_configured" && (
            <div className="notice warning">
              当前未配置软件管理主机特权执行器，无法读取真实安装状态或执行安装变更。
            </div>
          )}
          {!capabilities.some((c) => c.name === "plugins_catalog") ? (
            <Unavailable title="软件目录服务暂不可用" />
          ) : catalog.loading && !catalog.data ? (
            <p role="status">正在读取软件目录…</p>
          ) : (
            <div className="software-grid">
                {visibleItems
                .filter(
                  (item) =>
                    (item.name.includes(query) ||
                      item.description?.includes(query) ||
                      item.id.includes(query)) &&
                    (filter === "all" || filter === "installed"
                      ? filter !== "installed" || associated(item).length > 0
                      : filter === "containers"
                        ? ["container", "containers"].includes(item.category)
                        : filter === "other"
                          ? ![
                              "database",
                              "container",
                              "containers",
                              "runtime",
                            ].includes(item.category)
                          : item.category === filter),
                )
                .map((item) => (
                  <button
                    className="software-card"
                    key={item.id}
                    onClick={() => {
                      setSelected(item);
                      setInstance(associated(item)[0]);
                      setTab("instances");
                      setOperation(undefined);
                      setPlan(undefined);
                      setOpen(true);
                    }}
                  >
                    <div className="software-card-title">
                      <span className="heading-icon">
                        <Icon name="software" />
                      </span>
                      <div>
                        <strong>{item.name}</strong>
                        <small>
                          {categoryName[item.category] ?? item.category}
                        </small>
                      </div>
                    </div>
                    <p>{item.description}</p>
                    <div className="software-card-foot">
                      <span>
                        {installed.error
                          ? "状态无法读取"
                          : installed.data?.availability ===
                              "agent_not_configured"
                            ? "主机特权执行器未配置"
                            : associated(item).length
                              ? `${associated(item).length} 个可见实例`
                              : "暂无可见实例"}
                      </span>
                      <State
                        value={item.verification?.status}
                        reason={item.verification?.reason}
                      />
                    </div>
                  </button>
                ))}
            </div>
          )}
        </Card>
      )}
      {activeResourcePage === "catalog" &&
        capabilities.some((c) => c.name === "software_recovery_inspect") && (
          <SoftwareRecovery capabilities={capabilities} seed={recoverySeed} />
        )}
      {selected && (
        <ResourceDrawer
          open={open}
          title={`软件 · ${selected.name}`}
          onClose={() => setOpen(false)}
        >
          <div className="resource-summary">
            <span>{selected.description}</span>
            <State
              value={selected.verification?.status}
              reason={selected.verification?.reason}
            />
          </div>
          {selected.verification?.reason && (
            <div className="notice warning">{selected.verification.reason}</div>
          )}
          <div className="section-tabs" role="tablist" aria-label="软件详情分类">
            {detailTabs.map((t) => (
                <button
                  role="tab"
                  aria-selected={activeDetailTab === t.id}
                  key={t.id}
                  onClick={() => {
                    setTab(t.id);
                    setOperation(undefined);
                  }}
                >
                  {t.label}
                </button>
              ))}
          </div>
          {activeDetailTab === "instances" ? (
            <>
              <div className="instance-list">
                {associated(selected).map((s) => (
                  <button
                    key={s.instance_id ?? s.id}
                    className="instance-row"
                    onClick={() => {
                      setInstance(s);
                      setTab("operations");
                    }}
                  >
                    <div>
                      <strong>{s.instance_id ?? s.id}</strong>
                      <p>
                        {s.installed_version} · {fullDate(s.updated_at)}
                      </p>
                    </div>
                    <State value={s.status} />
                    <Icon name="arrow" size={16} />
                  </button>
                ))}
                {!associated(selected).length && (
                  <Unavailable
                    title="暂无可见安装实例"
                    detail="核心面板不会预装此软件。可在下方生成安装计划。"
                  />
                )}
              </div>
              {planAction && (
                <ActionForm
                  capability={planAction}
                  preset={{ plugin_id: selected.id, operation: "install" }}
                  references={
                    selected.id === "phpmyadmin" ? databaseTargetReferences : {}
                  }
                  onExecuted={receivePlan}
                />
              )}
            </>
          ) : activeDetailTab === "source" ? (
            <div className="version-list">
              <p className="muted">
                集成版本 {selected.integration_version} · 平台{" "}
                {selected.platforms
                  ?.map((p) =>
                    typeof p === "string"
                      ? p
                      : [p.os, p.arch, p.distro, p.release]
                          .filter(Boolean)
                          .join("/"),
                  )
                  .join(" · ")}
              </p>
              {selected.versions
                ?.filter((v) => {
                  const source = v.source;
                  return (
                    v.verification?.status !== "not_verified" &&
                    !(source && typeof source === "object" && source.kind === "unavailable")
                  );
                })
                .map((v) => (
                <div className="version-row" key={v.id}>
                  <strong>
                    {v.version}
                    {v.default ? " · 默认" : ""}
                  </strong>
                  <code>
                    {typeof v.source === "string"
                      ? v.source
                      : JSON.stringify(v.source, null, 2)}
                  </code>
                  <State
                    value={v.verification?.status}
                    reason={v.verification?.reason}
                  />
                </div>
                ))}
            </div>
          ) : (
            <>
              {instance && (
                <div className="resource-summary">
                  <span>当前实例 · {instance.instance_id ?? instance.id}</span>
                  <State value={detail.data?.status ?? instance.status} />
                </div>
              )}
              {instance &&
                ["phpmyadmin", "pgadmin"].includes(instance.plugin_id) &&
                instance.config &&
                typeof instance.config === "object" && (
                  <div className="notice warning">
                    此管理页面只监听主机本机回环地址{" "}
                    <code>
                      127.0.0.1:
                      {String(
                        (instance.config as Record<string, unknown>).port ??
                          (instance.plugin_id === "phpmyadmin" ? 19050 : 19051),
                      )}
                    </code>
                    。需要从域名访问时，请在“网站”页面添加反向代理到该地址；不要把管理端口直接开放到公网。
                  </div>
                )}
              {detail.error && (
                <div className="notice danger">{detail.error}</div>
              )}
              {instance &&
                ["php", "php-apt", "php-yum"].includes(instance.plugin_id) &&
                ["operations", "config"].includes(activeDetailTab) && (
                  <PHPResources
                    key={instance.id}
                    id={instance.id}
                    capabilities={capabilities}
                    onExecuted={refresh}
                    active={active && open}
                  />
                )}
              {instance &&
                activeDetailTab === "operations" &&
                hasInstanceResources(instance.plugin_id) && (
                  <InstanceResources
                    key={instance.id}
                    instance={instance}
                    capabilities={capabilities}
                    onExecuted={refresh}
                    active={active && open}
                  />
                )}
              {instance && activeDetailTab === "operations" &&
                ["php", "php-apt", "php-yum"].includes(instance.plugin_id) && (
                  <PHPMaintenance
                    key={instance.id}
                    instance={instance}
                    capabilities={capabilities}
                    onExecuted={refresh}
                    active={active && open}
                  />
                )}
              {instance && activeDetailTab === "operations" && instance.plugin_id === "supervisor" && (
                <SupervisorLogs
                  key={instance.id}
                  instance={instance}
                  capabilities={capabilities}
                  onExecuted={refresh}
                  active={active && open}
                />
              )}
              <ResourceActions
                capabilities={capabilities}
                names={(name) =>
                  compatible(name) &&
                  !name.startsWith("php_maintenance_") &&
                  !(
                    instance &&
                    activeDetailTab === "operations" &&
                    hasInstanceResources(instance.plugin_id) &&
                    /^(docker|ftp|rsync|supervisor|managed_database)_/.test(
                      name,
                    ) &&
                    !/status|diagnostics|replication/.test(name)
                  ) &&
                  (activeDetailTab === "backup"
                    ? /backup|restore|dump|import/.test(name)
                    : activeDetailTab === "config"
                      ? /config|setting/.test(name)
                      : true)
                }
                preset={{
                  plugin_id: selected.id,
                  id: instance?.id,
                  instance_id: instance?.id,
                  expected_revision:
                    detail.data?.revision ?? instance?.revision,
                }}
                selected={operation}
                onSelect={setOperation}
                onExecuted={refresh}
              />
              {planAction && activeDetailTab === "operations" && (
                <details>
                  <summary>安装 / 升级 / 卸载 / 回滚计划</summary>
                  <ActionForm
                    capability={planAction}
                    preset={{
                      plugin_id: selected.id,
                      instance_id: instance?.instance_id,
                    }}
                    references={
                      selected.id === "phpmyadmin"
                        ? databaseTargetReferences
                        : {}
                    }
                    onExecuted={receivePlan}
                  />
                </details>
              )}
            </>
          )}
          {submittedTask !== undefined && (
            <>
              <TaskResult
                submitted={submittedTask}
                capabilities={capabilities}
                onFinished={(task) => {
                  refresh();
                  const state = task.status ?? task.state ?? "";
                  if (
                    ["failed", "interrupted", "canceled", "cancelled"].includes(
                      state,
                    ) &&
                    submittedRecoveryID
                  )
                    setRecoverySeed({
                      id: submittedRecoveryID,
                      source: "未完成的软件安装任务",
                    });
                  setSubmittedRecoveryID(undefined);
                }}
              />
              {recoverySeed?.source === "未完成的软件安装任务" && (
                <div className="notice warning">
                  未完成任务的实例 <code>{recoverySeed.id}</code>
                  已填入安装恢复检查。
                  <Button
                    label="前往恢复检查"
                    variant="ghost"
                    onClick={() => {
                      setOpen(false);
                      document
                        .getElementById("software-install-recovery")
                        ?.scrollIntoView({ behavior: "smooth", block: "center" });
                    }}
                  />
                </div>
              )}
            </>
          )}
          {plan && (
            <section className="plan-review" aria-label="软件变更计划">
              <h3>变更计划</h3>
              <p className="muted">
                核对产物校验、依赖、目录与服务变更。确认后进入真实任务执行与健康验证。
              </p>
              <pre className="json-result">{JSON.stringify(plan, null, 2)}</pre>
              {planError && <div className="notice danger">{planError}</div>}
              {typeof plan.instance_id === "string" &&
                (["failed", "recovery_required"].includes(
                  String(plan.state ?? ""),
                ) ||
                  !!plan.failure ||
                  !!plan.error ||
                  !!planError) && (
                  <Button
                    label="将此失败计划的实例 ID 填入恢复检查"
                    variant="ghost"
                    onClick={() =>
                      setRecoverySeed({
                        id: String(plan.instance_id),
                        source: "失败的软件计划",
                      })
                    }
                  />
                )}
              {applyAction &&
              typeof (plan.plan_id ?? plan.id) === "string" &&
              typeof plan.expected_revision === "number" ? (
                <Button
                  label="确认应用软件计划"
                  variant="primary"
                  isLoading={applying}
                  onClick={() => void apply()}
                />
              ) : (
                <p className="muted">当前计划未提供可应用的版本标识或权限。</p>
              )}
            </section>
          )}
        </ResourceDrawer>
      )}
      <details className="advanced-actions">
        <summary>其他已授权的软件操作</summary>
        <ActionBrowser
          capabilities={capabilities}
          category="software"
          selected={other}
          onSelect={setOther}
          onExecuted={refresh}
        />
      </details>
    </div>
  );
}
