"use client";
import { SectionHeader } from "./section-header";
import { ResourceTabs } from "./resource-tabs";
import { ResourceError } from "./resource-ui";
import { t as translate } from "@/lib/i18n";
import { useEffect, useRef, useState } from "react";
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
import { BrowserApps } from "./browser-apps";
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
import { Icon, type IconName } from "./icon";
import { ChangeSummary } from "./change-summary";
import { TaskResult } from "./task-result";
import { SoftwareRecovery } from "./software-recovery";
import {
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
const categoryName: Record<string, string> = {
  get database() {
    return translate("数据库");
  },
  get "database-ui"() {
    return translate("数据库管理");
  },
  get container() {
    return translate("容器");
  },
  get containers() {
    return translate("容器");
  },
  get runtime() {
    return translate("运行环境");
  },
  get web() {
    return translate("网站服务");
  },
  get storage() {
    return translate("存储");
  },
  get security() {
    return translate("安全");
  },
  get mail() {
    return translate("邮件");
  },
};
const hasInstallableDefault = (item: CatalogItem) => {
  const selected = item.versions.find((version) => version.default);
  if (!selected) return false;
  const source = selected.source;
  return !(
    source &&
    typeof source === "object" &&
    source.kind === "unavailable"
  );
};
import { usePageState } from "@/lib/navigation";
import { pageItems, usePageSize } from "@/lib/collection";
import { Pager } from "./resource-ui";
function installedCatalogItem(target: SoftwareInstance): CatalogItem {
  return { id: target.plugin_id, name: target.plugin_id, description: "", category: "",
    integration_version: "", versions: [], platforms: [], capabilities: [] };
}
function categoryIcon(category: string): IconName {
  return category === "database" || category === "database-ui" ? "database"
    : ["container", "containers"].includes(category) ? "package"
    : category === "runtime" ? "terminal" : category === "web" ? "sites"
    : category === "storage" ? "disk" : category === "security" ? "shield" : "software";
}
export function SoftwareView({
  capabilities,
  onExecuted,
  active = true,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [filter, setFilter] = usePageState("software", "filter", "all");
  const [offset, setOffset] = usePageState("software", "offset", 0);
  const { ref: listRef, limit } = usePageSize(76);
  const [resourcePage, setResourcePage] = usePageState(
    "software",
    "page",
    "installed",
    true,
  );
  const [query, setQuery] = usePageState("software", "q", "");
  const [selectedID, setSelectedID] = usePageState("software", "id", "", true);
  const [selected, setSelected] = useState<CatalogItem>();
  const [instance, setInstance] = useState<SoftwareInstance>();
  const open = !!selectedID && ["installed", "catalog"].includes(resourcePage);
  const setOpen = (value: boolean) => {
    if (!value) setSelectedID("");
  };
  const [tab, setTab] = usePageState("software", "group", "instances");
  const [appGroup, setAppGroup] = usePageState(
    "software",
    "subgroup",
    "hosted",
  );
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
  const associated = (item: CatalogItem) =>
    instances.filter((s) => s.plugin_id === item.id || s.id === item.id);
  const visibleItems = items.filter(
    (item) =>
      isFirstReleaseCatalogPlugin(item.id) && hasInstallableDefault(item),
  );
  const matchingItems = visibleItems.filter(
    (item) =>
      (item.name.includes(query) ||
        item.description?.includes(query) ||
        translate(item.description ?? "")
          .toLowerCase()
          .includes(query.toLowerCase()) ||
        item.id.includes(query)) &&
      (filter === "all" || filter === "installed"
        ? filter !== "installed" || associated(item).length > 0
        : filter === "containers"
          ? ["container", "containers"].includes(item.category)
          : filter === "other"
            ? !["database", "container", "containers", "runtime"].includes(
                item.category,
              )
            : item.category === filter),
  );
  const softwareTabs = [
    ...(capabilities.some((c) => c.name === "plugins_list")
      ? [{ id: "installed", label: translate("已安装服务") }]
      : []),
    ...(capabilities.some((capability) => capability.name === "plugins_catalog")
      ? [{ id: "catalog", label: translate("软件库") }]
      : []),
    ...(capabilities.some(
      (capability) => capability.name === "database_connections_list",
    )
      ? [{ id: "database", label: translate("外部数据库") }]
      : []),
    ...(capabilities.some(
      (capability) => capability.name === "cache_connections_list",
    )
      ? [{ id: "cache", label: translate("缓存连接") }]
      : []),
    ...(capabilities.some(c => c.name === "hosted_apps_list")
      ? [{ id: "applications", label: translate("已部署应用") }] : []),
    ...(capabilities.some(c => c.name === "browser_apps_list")
      ? [{ id: "browsers", label: translate("浏览器应用") }] : []),
  ];
  // Preserve existing bookmarked browser-install URLs.
  const requestedPage = resourcePage === "applications" && appGroup === "browsers" ? "browsers" : resourcePage;
  const activeResourcePage = softwareTabs.some(tab => tab.id === requestedPage)
    ? requestedPage : softwareTabs[0]?.id;
  useEffect(() => {
    if (!selectedID) { setSelected(undefined); setInstance(undefined); return; }
    const target = instances.find(item => item.id === selectedID);
    const item = visibleItems.find(item => item.id === (target?.plugin_id ?? selectedID))
      ?? (target ? installedCatalogItem(target) : undefined);
    setSelected(item);
    setInstance(target ?? (item ? associated(item)[0] : undefined));
  }, [selectedID, catalog.data, installed.data]);
  const openSoftware = (item: CatalogItem, target?: SoftwareInstance) => {
    setSelected(item);
    setInstance(target ?? associated(item)[0]);
    setTab(
      target ? "operations" : "instances",
    );
    setOperation(undefined);
    setPlan(undefined);
    setSelectedID(target?.id ?? item.id);
  };
  const installedMatches = instances.filter(
    (item) =>
      isFirstReleaseCatalogPlugin(item.plugin_id) &&
      item.status !== "uninstalled" &&
      `${item.plugin_id} ${item.instance_id} ${item.installed_version}`
        .toLowerCase()
        .includes(query.toLowerCase()),
  );
  const installedPage = pageItems(installedMatches, offset, limit);
  const catalogPage = pageItems(matchingItems, offset, limit);
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
      (["mysql", "mysql-community", "mariadb", "postgresql"].includes(
        selected.id,
      ) &&
        /^managed_database/.test(name)));
  const detailTabs = selected
    ? [
        { id: "instances", label: translate("实例") },
        ...(planAction ||
        capabilities.some(
          (capability) =>
            compatible(capability.name) &&
            !/config|setting|backup|restore|dump|import/.test(capability.name),
        ) ||
        (!!instance && hasInstanceResources(instance.plugin_id))
          ? [{ id: "operations", label: translate("管理操作") }]
          : []),
        ...(capabilities.some(
          (capability) =>
            compatible(capability.name) &&
            /config|setting/.test(capability.name),
        )
          ? [{ id: "config", label: translate("配置") }]
          : []),
        ...(capabilities.some(
          (capability) =>
            compatible(capability.name) &&
            /backup|restore|dump|import/.test(capability.name),
        )
          ? [{ id: "backup", label: translate("备份恢复") }]
          : []),
        ...(selected.versions?.length
          ? [{ id: "source", label: translate("版本与来源") }]
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
      setRecoverySeed({
        id: next.instance_id,
        source: "失败的软件计划",
      });
  };
  return (
    <div className="stack">
      <ResourceTabs
        aria-label={translate("软件资源类型")}
      >
        {softwareTabs.map((tab) => (
          <button
            role="tab"
            aria-selected={activeResourcePage === tab.id}
            key={tab.id}
            onClick={() => {
              setSelectedID("");
              setOffset(0);
              setResourcePage(tab.id);
              setAppGroup(tab.id === "browsers" ? "browsers" : "hosted");
            }}
          >
            {tab.label}
          </button>
        ))}
      </ResourceTabs>
      {!activeResourcePage ? (
        <Unavailable title={translate("当前没有可用的软件管理能力")} />
      ) : activeResourcePage === "browsers" ? (
        <BrowserApps capabilities={capabilities} onExecuted={onExecuted} active={active} />
      ) : activeResourcePage === "applications" ? (
        <HostedApplications capabilities={capabilities} onExecuted={onExecuted} active={active} />
      ) : activeResourcePage === "installed" ? (
        <Card className="panel-card">
          <SectionHeader icon="software" className="card-heading">
            <h2>{translate("已安装服务")}</h2>
            <div className="button-row">
              <TextInput
                label={translate("搜索软件")}
                isLabelHidden
                value={query}
                onChange={(value) => {
                  setQuery(value);
                  setOffset(0);
                }}
                placeholder={translate("名称或功能")}
              />
              <Button
                label={translate("安装软件")}
                icon={<Icon name="plus" />}
                isDisabled={!capabilities.some(c => c.name === "plugins_catalog")}
                variant="primary"
                onClick={() => {
                  setOffset(0);
                  setResourcePage("catalog");
                }}
              />
            </div>
          </SectionHeader>
          {installed.error ? (
            <ResourceError error={installed.error} state={installed.state} retry={() => void installed.reload()} />
          ) : installed.data?.availability === "agent_not_configured" ? (
            <Unavailable
              title={translate("主机特权执行器未配置")}
              detail={translate("配置执行器后可读取安装状态和管理服务。")}
            />
          ) : installed.loading && !installed.data ? (
            <p role="status">{translate("正在读取…")}</p>
          ) : (
            <>
              <div ref={listRef} className="resource-table-wrap">
                <table className="resource-table">
                  <thead>
                    <tr>
                      <th>{translate("服务")}</th>
                      <th>{translate("状态")}</th>
                      <th>{translate("版本")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {installedPage.items.map((target) => (
                      <tr key={target.id}>
                        <td>
                          <button
                            className="resource-name"
                            onClick={() => {
                              const item = items.find(
                                (item) => item.id === target.plugin_id,
                              );
                              openSoftware(item ?? installedCatalogItem(target), target);
                            }}
                          >
                            {items.find((item) => item.id === target.plugin_id)
                              ?.name ?? target.plugin_id}
                            <small>{target.instance_id}</small>
                          </button>
                        </td>
                        <td>
                          <State value={target.status} />
                        </td>
                        <td>{target.installed_version || "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {!installedMatches.length && (
                <Unavailable
                  title={translate("暂无已安装服务")}
                  detail={translate("从软件库安装所需服务。")}
                />
              )}
              <Pager
                offset={installedPage.offset}
                limit={limit}
                total={installedPage.total}
                count={installedPage.items.length}
                loading={installed.loading}
                onOffset={setOffset}
              />
            </>
          )}
        </Card>
      ) : ["database", "cache"].includes(activeResourcePage ?? "") ? (
        <ConnectionsView
          key={activeResourcePage}
          kind={activeResourcePage as "database" | "cache"}
          capabilities={capabilities}
          onExecuted={onExecuted}
          active={active}
        />
      ) : (
        <Card className="panel-card">
          <SectionHeader icon="software" className="card-heading">
            <h2>{translate("软件库")}</h2>
            <TextInput
              label={translate("搜索软件")}
              isLabelHidden
              value={query}
              onChange={(value) => {
                setQuery(value);
                setOffset(0);
              }}
              placeholder={translate("名称或功能")}
              startIcon={<Icon name="search" />}
            />
          </SectionHeader>
          <ResourceTabs
            aria-label={translate("软件分类")}
          >
            {[
              { id: "all", label: translate("全部") },
              { id: "installed", label: translate("已安装") },
              { id: "database", label: translate("数据库") },
              { id: "containers", label: translate("容器") },
              { id: "runtime", label: translate("运行环境") },
              { id: "other", label: translate("其他") },
            ].map((t) => (
              <button
                role="tab"
                aria-selected={filter === t.id}
                key={t.id}
                onClick={() => { setFilter(t.id); setOffset(0); }}
              >
                {t.label}
              </button>
            ))}
          </ResourceTabs>
          {catalog.error && (
            <ResourceError error={catalog.error} state={catalog.state} retry={() => void catalog.reload()} />
          )}
          {installed.error && (
            <details className="notice warning"><summary>{translate("安装状态读取失败：")}</summary><pre>{installed.error}</pre></details>
          )}
          {installed.data?.availability === "agent_not_configured" && (
            <div className="notice warning">
              {translate(
                "当前未配置软件管理主机特权执行器，无法读取真实安装状态或执行安装变更。",
              )}
            </div>
          )}
          {catalog.error ? null : !capabilities.some((c) => c.name === "plugins_catalog") ? (
            <Unavailable title={translate("软件目录服务暂不可用")} />
          ) : catalog.loading && !catalog.data ? (
            <p role="status">{translate("正在读取软件目录…")}</p>
          ) : (
            <div ref={listRef} className="software-grid software-list">
              {!matchingItems.length && (
                <div className="empty software-empty" role="status">
                  <strong>{translate("没有匹配的软件")}</strong>
                  <p>{translate("试试其他名称，或清除筛选查看全部软件。")}</p>
                  <Button
                    label={translate("清除筛选")}
                    variant="ghost"
                    onClick={() => {
                      setFilter("all");
                      setQuery("");
                    }}
                  />
                </div>
              )}
              {catalogPage.items.map((item) => (
                <button
                  className="software-card"
                  key={item.id}
                  onClick={() => openSoftware(item)}
                >
                  <div className="software-card-title">
                    <span className="heading-icon">
                      <Icon name={categoryIcon(item.category)} />
                    </span>
                    <div>
                      <strong>{item.name}</strong>
                      <small>
                        {categoryName[item.category] ?? item.category}
                      </small>
                    </div>
                  </div>
                  <p>{translate(item.description ?? "")}</p>
                  <div className="software-card-foot">
                    <span>
                      {!installed.data || installed.error || installed.data.availability === "agent_not_configured"
                        ? translate("安装状态未知")
                        : associated(item).length
                        ? translate("{0} 个可见实例", [associated(item).length])
                        : translate("可安装")}
                    </span>
                    <Icon name="arrow" size={18} />
                  </div>
                </button>
              ))}
            </div>
          )}
          {!catalog.error && <Pager
            offset={catalogPage.offset}
            limit={limit}
            total={catalogPage.total}
            count={catalogPage.items.length}
            loading={catalog.loading}
            onOffset={setOffset}
          />}
        </Card>
      )}
      {activeResourcePage === "catalog" &&
        capabilities.some((c) => c.name === "software_recovery_inspect") && (
          <details className="advanced-actions">
            <summary>{translate("安装恢复工具")}</summary>
            <SoftwareRecovery capabilities={capabilities} seed={recoverySeed} />
          </details>
        )}
      {selected && (
        <ResourceDrawer
          open={open}
          title={translate("软件 · {0}", [selected.name])}
          onClose={() => setOpen(false)}
        >
          <div className="resource-summary">
            {instance ? <><State value={detail.data?.status ?? instance.status} /><span>{instance.installed_version || "—"}</span><span className="muted">{instance.instance_id}</span></> : <span>{translate(selected.description ?? "")}</span>}
          </div>
          <ResourceTabs
            aria-label={translate("软件详情分类")}
          >
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
          </ResourceTabs>
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
                        {s.installed_version}· {fullDate(s.updated_at)}
                      </p>
                    </div>
                    <State value={s.status} />
                    <Icon name="arrow" size={16} />
                  </button>
                ))}
                {!associated(selected).length && (
                  <Unavailable
                    title={translate("暂无可见安装实例")}
                    detail={translate(
                      "核心面板不会预装此软件。可在下方生成安装计划。",
                    )}
                  />
                )}
              </div>
              {planAction && hasInstallableDefault(selected) && (
                <ActionForm
                  capability={planAction}
                  presentation="business"
                  lockedPresetFields={["plugin_id", "operation"]}
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
              <div className="resource-summary"><span>{translate("软件来源验证")}</span><State value={selected.verification?.status} reason={selected.verification?.reason} /></div>
              {selected.verification?.reason && <div className="notice warning">{selected.verification.reason}</div>}
              <p className="muted">
                {translate("集成版本")}
                {selected.integration_version}
                {translate(" · 平台")}{" "}
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
                    !(
                      source &&
                      typeof source === "object" &&
                      source.kind === "unavailable"
                    )
                  );
                })
                .map((v) => (
                  <div className="version-row" key={v.id}>
                    <strong>
                      {v.version}
                      {v.default ? translate(" · 默认") : ""}
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
              {instance &&
                ["phpmyadmin", "pgadmin"].includes(instance.plugin_id) &&
                instance.config &&
                typeof instance.config === "object" && (
                  <div className="notice warning">
                    {translate("此管理页面只监听主机本机回环地址")}{" "}
                    <code>
                      127.0.0.1:
                      {String(
                        (instance.config as Record<string, unknown>).port ??
                          (instance.plugin_id === "phpmyadmin" ? 19050 : 19051),
                      )}
                    </code>
                    {translate(
                      "。需要从域名访问时，请在“网站”页面添加反向代理到该地址；不要把管理端口直接开放到公网。",
                    )}
                  </div>
                )}
              {detail.error && (
                <ResourceError error={detail.error} state={detail.state} retry={() => void detail.reload()} />
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
              {instance &&
                activeDetailTab === "operations" &&
                ["php", "php-apt", "php-yum"].includes(instance.plugin_id) && (
                  <PHPMaintenance
                    key={instance.id}
                    instance={instance}
                    capabilities={capabilities}
                    onExecuted={refresh}
                    active={active && open}
                  />
                )}
              {instance &&
                activeDetailTab === "operations" &&
                instance.plugin_id === "supervisor" && (
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
                  <summary>
                    {translate("安装 / 升级 / 卸载 / 回滚计划")}
                  </summary>
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
                  {translate("未完成任务的实例")}
                  <code>{recoverySeed.id}</code>
                  {translate("已填入安装恢复检查。")}
                  <Button
                    label={translate("前往恢复检查")}
                    variant="ghost"
                    onClick={() => {
                      setOpen(false);
                      document
                        .getElementById("software-install-recovery")
                        ?.scrollIntoView({
                          behavior: "smooth",
                          block: "center",
                        });
                    }}
                  />
                </div>
              )}
            </>
          )}
          {plan && (
            <section
              className="plan-review"
              aria-label={translate("软件变更计划")}
            >
              <h3>{translate("变更计划")}</h3>
              <p className="muted">
                {translate(
                  "核对产物校验、依赖、目录与服务变更。确认后进入真实任务执行与健康验证。",
                )}
              </p>
              <ChangeSummary plan={plan} />
              {planError && <div className="notice danger">{planError}</div>}
              {typeof plan.instance_id === "string" &&
                (["failed", "recovery_required"].includes(
                  String(plan.state ?? ""),
                ) ||
                  !!plan.failure ||
                  !!plan.error ||
                  !!planError) && (
                  <Button
                    label={translate("将此失败计划的实例 ID 填入恢复检查")}
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
                  label={translate("确认应用软件计划")}
                  variant="primary"
                  isLoading={applying}
                  onClick={() => void apply()}
                />
              ) : (
                <p className="muted">
                  {translate("当前计划未提供可应用的版本标识或权限。")}
                </p>
              )}
            </section>
          )}
        </ResourceDrawer>
      )}
    </div>
  );
}
