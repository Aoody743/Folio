"use client";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import dynamic from "next/dynamic";
const CertificatesView = dynamic(
  () => import("./certificates").then((m) => m.CertificatesView),
  { ssr: false },
);
const SiteBackups = dynamic(
  () => import("./site-content").then((m) => m.SiteBackups),
  { ssr: false },
);
const SiteDefault = dynamic(
  () => import("./site-content").then((m) => m.SiteDefault),
  { ssr: false },
);
const RetainedSites = dynamic(
  () => import("./site-content").then((m) => m.RetainedSites),
  { ssr: false },
);
const PHPResources = dynamic(
  () => import("./php-resources").then((m) => m.PHPResources),
  { ssr: false },
);
const SitePHPMaintenance = dynamic(
  () => import("./site-php-maintenance").then((m) => m.SitePHPMaintenance),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Capability, fullDate, list } from "@/lib/api";
import { Page, Site, SoftwareInstance } from "@/lib/resources";
import { projectInput, Schema } from "@/lib/schema";
import { useResource } from "@/lib/resource-hook";
import {
  siteCategoryCapabilities,
  siteCategoryReferences,
  siteListInput,
  SiteCategory,
  supportsSiteCategoryFilter,
} from "@/lib/site-categories";
import { Icon } from "./icon";
import { navigate, usePageState } from "@/lib/navigation";
import { SiteCreateFields } from "./site-create-fields";
import { usePageSize } from "@/lib/collection";
import { HostingStackSetup } from "./hosting-stack-setup";
import { ActionForm, ActionBrowser, References } from "./actions";
import { SiteCategoriesManager } from "./site-categories";
import { SiteMigration } from "./site-migration";
import { isDedicatedSiteMigrationAction } from "@/lib/site-migration";
import {
  Pager,
  ResourceError,
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
const groups = [
  {
    id: "overview",
    get label() {
      return translate("总览");
    },
  },
  {
    id: "domains",
    get label() {
      return translate("域名");
    },
  },
  { id: "tls", label: "TLS" },
  { id: "php", label: "PHP" },
  {
    id: "routing",
    get label() {
      return translate("代理与重定向");
    },
  },
  {
    id: "backup",
    get label() {
      return translate("备份");
    },
  },
  {
    id: "logs",
    get label() {
      return translate("日志");
    },
  },
  {
    id: "config",
    get label() {
      return translate("配置");
    },
  },
  {
    id: "migration",
    get label() {
      return translate("迁移");
    },
  },
];
const inGroup = (name: string, group: string) =>
  group === "domains"
    ? /domain/.test(name)
    : group === "tls"
      ? /tls|certificate|acme/.test(name)
      : group === "php"
        ? /php/.test(name)
        : group === "routing"
          ? /proxy|redirect/.test(name)
          : group === "backup"
            ? /backup|restore/.test(name)
            : group === "logs"
              ? /log/.test(name)
              : group === "config"
                ? !/domain|tls|certificate|acme|php|proxy|redirect|backup|restore|log/.test(
                    name,
                  )
                : true;
export function SitesView({
  capabilities,
  onExecuted,
  active = true,
  onFiles,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
  onFiles?: (root: string) => void;
}) {
  const [offset, setOffset] = usePageState("sites", "offset", 0);
  const { ref: listRef, limit } = usePageSize(72);
  const [resourcePage, setResourcePage] = usePageState(
    "sites",
    "page",
    "sites",
  );
  const [query, setQuery] = usePageState("sites", "q", "");
  const [statusFilter, setStatusFilter] = usePageState("sites", "status", "");
  const [categoryFilter, setCategoryFilter] = usePageState(
    "sites",
    "category",
    "",
  );
  const [selectedID, setSelectedID] = usePageState("sites", "id", "", true);
  const [selectedLocal, setSelectedLocal] = useState<Site>();
  const selected =
    selectedLocal?.id === selectedID
      ? selectedLocal
      : selectedID
        ? ({ id: selectedID, name: selectedID, domains: [] } as unknown as Site)
        : undefined;
  const setSelected = (site: Site) => {
    setSelectedLocal(site);
    setSelectedID(site.id);
  };
  const open = !!selectedID;
  const setOpen = (value: boolean) => {
    if (!value) setSelectedID("");
  };
  const [creating, setCreating] = usePageState("sites", "create", false, true);
  const [group, setGroup] = usePageState("sites", "group", "overview");
  const [operation, setOperation] = useState<string>();
  const [other, setOther] = useState<string>();
  const siteListCapability = capabilities.find((c) => c.name === "sites_list");
  const categoryCapabilities = siteCategoryCapabilities(capabilities);
  const available = !!siteListCapability;
  const categoryFilterAvailable =
    !!categoryCapabilities.list &&
    supportsSiteCategoryFilter(siteListCapability);
  const categoryResources = useResource<Page<SiteCategory>>(
    "sites_categories_list",
    {},
    !!categoryCapabilities.list && active,
  );
  useEffect(() => {
    if (categoryResources.error && categoryFilter) {
      setCategoryFilter("");
      setOffset(0);
    }
  }, [categoryResources.error, categoryFilter]);
  const visibleCategories = categoryResources.error
    ? []
    : list(categoryResources.data);
  const resources = useResource<Page<Site>>(
    "sites_list",
    {
      ...siteListInput(siteListCapability, {
        limit,
        offset,
        query,
        categoryID: categoryFilterAvailable ? categoryFilter : "",
      }),
      ...(statusFilter &&
      siteListCapability?.input_schema?.properties &&
      Object.hasOwn(siteListCapability.input_schema.properties, "status")
        ? { status: statusFilter }
        : {}),
    },
    available && active,
  );
  const detail = useResource<Site>(
    "sites_get",
    { id: selected?.id },
    !!selected && capabilities.some((c) => c.name === "sites_get") && active,
  );
  const instances = useResource<Page<SoftwareInstance>>(
    "plugins_list",
    {},
    active &&
      (creating || open) &&
      capabilities.some((c) => c.name === "plugins_list"),
  );
  const certificates = useResource<
    Page<{ id: string; name: string; domains: string[] }>
  >(
    "certificates_list",
    {},
    active &&
      open &&
      group === "tls" &&
      capabilities.some((c) => c.name === "certificates_list"),
  );
  const backups = useResource<Page<{ id: string; created_at: string }>>(
    "sites_backups_list",
    { id: selected?.id },
    active &&
      open &&
      group === "backup" &&
      !!selected &&
      capabilities.some((c) => c.name === "sites_backups_list"),
  );
  const references: References = {
    ...siteCategoryReferences(visibleCategories),
    ...(instances.data
      ? {
          web_instance: list(instances.data)
            .filter(
              (item) =>
                ["nginx", "openresty"].includes(item.plugin_id) &&
                item.status !== "uninstalled",
            )
            .map((item) => ({
              value: item.id,
              label: `${item.plugin_id === "nginx" ? "Nginx" : "OpenResty"} · ${item.instance_id}`,
            })),
          php_instance: [
            { value: "", label: translate("静态内容，不绑定PHP") },
            ...list(instances.data)
              .filter(
                (item) =>
                  ["php", "php-apt", "php-yum"].includes(item.plugin_id) &&
                  item.status !== "uninstalled",
              )
              .map((item) => ({
                value: item.id,
                label: `${item.plugin_id} · ${item.instance_id} · ${item.installed_version}`,
              })),
          ],
        }
      : {}),
    ...(certificates.data
      ? {
          certificate_id: list(certificates.data).map((row) => ({
            value: row.id,
            label: `${row.name} · ${row.domains.join(" · ")}`,
          })),
        }
      : {}),
    ...(backups.data
      ? {
          backup_id: list(backups.data).map((row) => ({
            value: row.id,
            label: `${fullDate(row.created_at)} · ${row.id}`,
          })),
        }
      : {}),
  };
  const rows = list(resources.data);
  const current = detail.data ?? selected;
  const create = capabilities.find((c) => c.name === "sites_create");
  const update = capabilities.find((c) => c.name === "sites_update");
  const allowedFields: Record<string, string[]> = {
    domains: ["domains"],
    php: ["php_instance", "php"],
    routing: ["proxies", "redirects", "directory_bindings"],
    config: [
      "name",
      "web_instance",
      "run_path",
      "category_id",
      "status",
      ...(capabilities.some((c) => c.name === "sites_default_set")
        ? []
        : ["default"]),
      "ipv6",
      "index",
      "description",
      "expires_at",
      "rewrite",
      "access_log",
      "rate_limit",
      "protection",
    ],
  };
  const updateProperties = update?.input_schema?.properties as
    | Record<string, unknown>
    | undefined;
  const updateHasFields = (target: string) =>
    !!allowedFields[target]?.some((field) => updateProperties?.[field]);
  const hasGroupActions = (target: string) =>
    capabilities.some(
      (capability) =>
        /^(sites|tls)_/.test(capability.name) &&
        !/^sites_(retained|categor)/.test(capability.name) &&
        !isDedicatedSiteMigrationAction(capability.name) &&
        ![
          "sites_list",
          "sites_get",
          "sites_create",
          "sites_update",
          "sites_default_get",
          "sites_default_set",
        ].includes(capability.name) &&
        inGroup(capability.name, target),
    );
  const supportedGroups = groups.filter((tab) => {
    if (tab.id === "overview") return true;
    if (tab.id === "migration") return false;
    if (tab.id === "tls")
      return (
        hasGroupActions("tls") ||
        capabilities.some(
          (capability) => capability.name === "certificates_list",
        )
      );
    if (tab.id === "backup")
      return capabilities.some((capability) =>
        /^sites_backup/.test(capability.name),
      );
    if (tab.id === "php")
      return (
        updateHasFields(tab.id) ||
        capabilities.some((capability) => capability.name.startsWith("php_"))
      );
    return updateHasFields(tab.id) || hasGroupActions(tab.id);
  });
  const configGroups = supportedGroups.filter((tab) =>
    ["config", "domains", "tls", "php", "routing"].includes(tab.id),
  );
  const primaryGroups = [
    ...supportedGroups.filter((tab) => tab.id === "overview"),
    ...(configGroups.length
      ? [{ id: "config", label: translate("配置") }]
      : []),
    ...supportedGroups.filter((tab) => tab.id === "logs"),
    ...supportedGroups.filter((tab) => tab.id === "backup"),
  ];
  const activeGroup = supportedGroups.some((tab) => tab.id === group)
    ? group
    : (supportedGroups[0]?.id ?? "overview");
  const groupedUpdate =
    update && allowedFields[activeGroup]
      ? {
          ...update,
          description: translate("修改网站{0}", [
            groups.find((g) => g.id === activeGroup)?.label ??
              translate("设置"),
          ]),
          input_schema: {
            ...update.input_schema,
            properties: Object.fromEntries(
              [
                "id",
                "expected_revision",
                "reset_advanced_config",
                ...allowedFields[activeGroup],
              ]
                .filter((k) => updateProperties?.[k])
                .map((k) => [k, updateProperties![k]]),
            ),
          },
        }
      : undefined;
  const siteResourceTabs = [
    { id: "sites", label: translate("网站") },
    ...(capabilities.some((capability) =>
      ["certificates_list", "certificates_import"].includes(capability.name),
    )
      ? [{ id: "certificates", label: translate("证书") }]
      : []),
    ...(capabilities.some((c) => c.name === "sites_retained_list")
      ? [{ id: "retained", label: translate("保留内容") }]
      : []),
  ];
  const activeResourcePage = siteResourceTabs.some(
    (tab) => tab.id === resourcePage,
  )
    ? resourcePage
    : siteResourceTabs[0]?.id;
  const refresh = () => {
    void resources.reload();
    void detail.reload();
    onExecuted();
  };
  const categoriesChanged = (categoryID?: string) => {
    if (categoryID && categoryFilter === categoryID) {
      setCategoryFilter("");
      setOffset(0);
    }
    void categoryResources.reload();
    void resources.reload();
    onExecuted();
  };
  return (
    <div className="stack">
      <div
        className="section-tabs"
        role="tablist"
        aria-label={translate("网站资源类型")}
      >
        {siteResourceTabs.map((tab) => (
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
      {activeResourcePage === "retained" ? (
        <RetainedSites
          capabilities={capabilities}
          onExecuted={refresh}
          active={active}
          onFiles={onFiles}
        />
      ) : activeResourcePage === "certificates" ? (
        <CertificatesView
          capabilities={capabilities}
          onExecuted={refresh}
          active={active}
        />
      ) : (
        <Card className="panel-card">
          <div className="list-controls site-toolbar">
            <TextInput
              label={translate("筛选网站")}
              isLabelHidden
              value={query}
              onChange={(value) => {
                setQuery(value);
                setOffset(0);
              }}
              placeholder={translate("名称或域名")}
              startIcon={<Icon name="search" />}
            />
            {create && (
              <Button
                label={translate("创建网站")}
                variant="primary"
                onClick={() => setCreating(true)}
              />
            )}
          </div>
          <details className="list-options">
            <summary>
              {translate("筛选与分类")}
              {statusFilter
                ? ` · ${statusFilter === "running" ? translate("已启用") : statusFilter === "stopped" ? translate("已停用") : translate("状态未知")}`
                : ""}
            </summary>
            <div className="list-controls">
              <label className="native-field">
                {translate("状态")}
                <select
                  aria-label={translate("网站状态筛选")}
                  value={statusFilter}
                  onChange={(e) => {
                    setStatusFilter(e.target.value);
                    setOffset(0);
                  }}
                >
                  <option value="">{translate("全部")}</option>
                  <option value="running">{translate("已启用")}</option>
                  <option value="stopped">{translate("已停用")}</option>
                  <option value="unknown">{translate("状态未知")}</option>
                </select>
              </label>
              {categoryFilterAvailable && (
                <label className="native-field">
                  {translate("网站分类筛选")}
                  <select
                    value={categoryFilter}
                    onChange={(e) => {
                      setCategoryFilter(e.target.value);
                      setOffset(0);
                    }}
                  >
                    <option value="">{translate("全部分类")}</option>
                    {visibleCategories.map((category) => (
                      <option key={category.id} value={category.id}>
                        {category.name}
                      </option>
                    ))}
                  </select>
                </label>
              )}
              <SiteCategoriesManager
                capabilities={capabilities}
                categories={visibleCategories}
                loading={categoryResources.loading}
                listError={categoryResources.error}
                onReload={categoryResources.reload}
                onChanged={categoriesChanged}
              />
            </div>
          </details>
          {!available ? (
            <Unavailable
              title={translate("网站资源服务暂不可用")}
              icon="sites"
            />
          ) : (
            <>
              {resources.error && (
                <ResourceError
                  error={resources.error}
                  state={resources.state}
                  retry={() => void resources.reload()}
                />
              )}
              {resources.loading && !resources.data && (
                <p className="muted" role="status">
                  {translate("正在读取网站…")}
                </p>
              )}
              {categoryResources.error && (
                <div className="notice warning" role="status">
                  {translate("分类列表读取失败，网站列表已清除分类筛选条件。")}
                  <Button
                    label={translate("重试分类列表")}
                    variant="ghost"
                    onClick={() => void categoryResources.reload()}
                  />
                </div>
              )}
              <div ref={listRef} className="resource-table-wrap">
                <table className="resource-table sites-table">
                  <thead>
                    <tr>
                      <th>{translate("网站 / 域名")}</th>
                      <th>{translate("状态")}</th>
                      <th className="site-runtime">{translate("运行环境")}</th>
                      <th className="site-tls">TLS</th>
                      <th className="site-updated">{translate("更新")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((site) => (
                      <tr key={site.id}>
                        <td>
                          <button
                            className="resource-name"
                            onClick={() => {
                              setSelected(site);
                              setGroup("overview");
                              setOperation(undefined);
                              setOpen(true);
                            }}
                          >
                            {site.name}
                          </button>
                          {site.default && (
                            <span className="status-chip neutral">
                              {translate("默认网站")}
                            </span>
                          )}
                          <small>
                            {site.domains?.join(" · ") ||
                              translate("未配置域名")}
                          </small>
                        </td>
                        <td>
                          <State
                            value={
                              site.status === "running" ? "active" : site.status
                            }
                          />
                        </td>
                        <td className="site-runtime">
                          {site.php_version ? `PHP ${site.php_version}` : "—"}
                        </td>
                        <td className="site-tls">
                          {site.tls?.enabled ? (
                            <span title={fullDate(site.tls.expires_at)}>
                              {translate("已配置")}
                              <small>{fullDate(site.tls.expires_at)}</small>
                            </span>
                          ) : (
                            translate("未配置")
                          )}
                        </td>
                        <td className="site-updated">{fullDate(site.updated_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {!rows.length && !resources.loading && !resources.error && (
                <Unavailable
                  title={translate("暂无网站")}
                  detail={translate(
                    "先安装受支持的网站服务，再创建域名与站点。",
                  )}
                  icon="sites"
                />
              )}
              <Pager
                offset={offset}
                limit={limit}
                total={resources.data?.total}
                count={rows.length}
                loading={resources.loading}
                onOffset={setOffset}
              />
            </>
          )}
        </Card>
      )}
      {!rows.length &&
        !resources.error &&
        !resources.loading &&
        !query &&
        !statusFilter &&
        activeResourcePage === "sites" && (
          <details className="module-setup">
            <summary>{translate("配置网站运行环境")}</summary>
            <HostingStackSetup capabilities={capabilities} />
          </details>
        )}
      {creating && create && (
        <ResourceDrawer
          open={creating}
          title={translate("创建网站")}
          onClose={() => setCreating(false)}
        >
          {instances.loading && !instances.data ? <p role="status">{translate("正在读取运行环境…")}</p> : instances.error ? (
            <ResourceError error={instances.error} state={instances.state} retry={() => void instances.reload()} />
          ) : instances.state === "unconfigured" || (instances.data && !references.web_instance?.length) ? (
            <section className="stack">
              <p>{translate("创建网站前，请先配置执行器并安装 Nginx 或 OpenResty。")}</p>
              <Button label={translate("前往软件管理")} onClick={() => navigate("software", { page: "installed" })} />
            </section>
          ) : <ActionForm
            capability={create}
            presentation="business"
            showTitle={false}
            renderFields={props => <SiteCreateFields {...props} references={references} />}
            references={references}
            preset={{
              ...(references.web_instance?.[0]
                ? { web_instance: references.web_instance[0].value }
                : {}),
              ...(references.php_instance?.[1]
                ? { php_instance: references.php_instance[1].value }
                : {}),
            }}
            onExecuted={() => {
              refresh();
            }}
          />}
        </ResourceDrawer>
      )}
      {current && (
        <ResourceDrawer
          open={open}
          title={translate("网站 · {0}", [current.name])}
          onClose={() => setOpen(false)}
        >
          {detail.error && <div className="notice danger">{detail.error}</div>}
          <div className="resource-summary">
            <State
              value={current.status === "running" ? "active" : current.status}
            />
            <span className="muted">{current.domains?.join(" · ")}</span>
          </div>
          <div
            className="section-tabs"
            role="tablist"
            aria-label={translate("网站详情分类")}
          >
            {primaryGroups.map((tab) => (
              <button
                role="tab"
                aria-selected={
                  tab.id === activeGroup ||
                  (tab.id === "config" &&
                    ["domains", "tls", "php", "routing"].includes(activeGroup))
                }
                key={tab.id}
                onClick={() => {
                  setGroup(
                    tab.id === "config" &&
                      !supportedGroups.some((group) => group.id === "config")
                      ? configGroups[0].id
                      : tab.id,
                  );
                  setOperation(undefined);
                }}
              >
                {tab.id === "overview" ? translate("概况") : tab.label}
              </button>
            ))}
          </div>
          {["config", "domains", "tls", "php", "routing"].includes(
            activeGroup,
          ) && (
            <label className="native-field">
              {translate("配置分组")}
              <select
                value={activeGroup}
                onChange={(e) => {
                  setGroup(e.target.value);
                  setOperation(undefined);
                }}
              >
                {supportedGroups
                  .filter((tab) =>
                    ["config", "domains", "tls", "php", "routing"].includes(
                      tab.id,
                    ),
                  )
                  .map((tab) => (
                    <option value={tab.id} key={tab.id}>
                      {tab.label}
                    </option>
                  ))}
              </select>
            </label>
          )}
          {current.configuration_mode === "advanced" &&
            activeGroup !== "migration" && (
              <div className="notice warning">
                <strong>{translate("此网站使用手工配置。")}</strong>
                <p>
                  {translate(
                    "修改域名、PHP或路由前，确认是否放弃手工配置并重新生成。原配置不会因切换详情分类而修改。",
                  )}
                </p>
              </div>
            )}
          {activeGroup === "overview" ? (
            <>
              <div className="detail-grid">
                <div>
                  <small>{translate("站点目录")}</small>
                  <code>{current.document_root || translate("未提供")}</code>
                </div>
                <div>
                  <small>{translate("运行环境")}</small>
                  <strong>
                    {current.php_version
                      ? `PHP ${current.php_version}`
                      : translate("未提供")}
                  </strong>
                </div>
                <div>
                  <small>TLS</small>
                  <strong>
                    {current.tls?.enabled
                      ? translate("已配置")
                      : translate("未配置")}
                  </strong>
                  <span className="muted">
                    {current.tls?.expires_at
                      ? translate("到期 {0}", [
                          fullDate(current.tls.expires_at),
                        ])
                      : ""}
                  </span>
                </div>
                <div>
                  <small>{translate("更新")}</small>
                  <span>{fullDate(current.updated_at)}</span>
                </div>
              </div>
              <SiteDefault
                key={current.id}
                id={current.id}
                capabilities={capabilities}
                onExecuted={refresh}
                active={active && open}
              />
            </>
          ) : activeGroup === "backup" ? (
            <SiteBackups
              key={current.id}
              id={current.id}
              revision={current.revision}
              capabilities={capabilities}
              onExecuted={refresh}
              active={active && open}
            />
          ) : activeGroup === "migration" ? (
            <SiteMigration
              key={current.id}
              site={current}
              siteFresh={!!detail.data}
              siteError={detail.error || undefined}
              webInstances={list(instances.data)}
              webInstancesLoaded={
                !!instances.data ||
                !capabilities.some((c) => c.name === "plugins_list")
              }
              webInstancesLoading={instances.loading}
              webInstancesError={instances.error || undefined}
              capabilities={capabilities}
              active={active && open && activeGroup === "migration"}
              onExecuted={refresh}
            />
          ) : (
            <>
              {activeGroup === "config" &&
                typeof current.category_id === "string" &&
                current.category_id !== "" &&
                !categoryResources.loading &&
                !visibleCategories.some(
                  (category) => category.id === current.category_id,
                ) && (
                  <div className="notice warning" role="status">
                    {translate(
                      "当前分类不在可见分类列表中。若要清除已删除或不可见的旧绑定，请在“网站分类”中选择“未分类”；其他配置字段仍会按本表单提交。",
                    )}
                  </div>
                )}
              <ResourceActions
                capabilities={capabilities}
                names={(name) =>
                  /^(sites|tls)_/.test(name) &&
                  !/^sites_(retained|categor)/.test(name) &&
                  !isDedicatedSiteMigrationAction(name) &&
                  ![
                    "sites_list",
                    "sites_get",
                    "sites_create",
                    "sites_update",
                    "sites_default_get",
                    "sites_default_set",
                  ].includes(name) &&
                  inGroup(name, activeGroup)
                }
                preset={{
                  id: current.id,
                  site_id: current.id,
                  expected_revision: current.revision,
                }}
                selected={operation}
                onSelect={setOperation}
                onExecuted={refresh}
                references={references}
              />
              {activeGroup === "tls" &&
                capabilities.some((capability) =>
                  ["certificates_list", "certificates_import"].includes(
                    capability.name,
                  ),
                ) && (
                  <CertificatesView
                    capabilities={capabilities}
                    onExecuted={refresh}
                    active={active && open}
                    siteID={current.id}
                  />
                )}
              {groupedUpdate && (
                <ActionForm
                  key={current.id + activeGroup}
                  capability={groupedUpdate}
                  references={references}
                  preset={
                    projectInput(groupedUpdate.input_schema as Schema, {
                      ...current,
                      expected_revision: current.revision,
                    }) as Record<string, unknown>
                  }
                  onExecuted={refresh}
                />
              )}
              {activeGroup === "php" && current.php_instance && (
                <PHPResources
                  key={current.php_instance}
                  id={current.php_instance}
                  capabilities={capabilities}
                  onExecuted={refresh}
                  active={active && open}
                />
              )}
              {activeGroup === "php" && (
                <SitePHPMaintenance
                  key={`site-php:${current.id}`}
                  siteID={current.id}
                  siteRevision={current.revision}
                  phpInstance={current.php_instance ?? ""}
                  capabilities={capabilities}
                  onExecuted={refresh}
                  active={active && open && activeGroup === "php"}
                />
              )}
            </>
          )}
        </ResourceDrawer>
      )}
    </div>
  );
}
