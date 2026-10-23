"use client";
import { ResourceError } from "./resource-ui";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Capability, list } from "@/lib/api";
import { choiceLabel } from "@/lib/labels";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { projectInput, Schema } from "@/lib/schema";
import { ActionForm } from "./actions";
import {
  ResourceActions,
  ResourceDrawer,
  Unavailable,
  Pager,
} from "./resource-ui";
import { usePageState } from "@/lib/navigation";
import { usePageSize, pageItems } from "@/lib/collection";
type Connection = {
  id: string;
  name?: string;
  engine: string;
  host: string;
  port: number;
  database: string | number;
  username?: string;
  tls: boolean;
  access?: string;
  prefix?: string;
  revision: number;
  instance_id?: string;
  [key: string]: unknown;
};
export function ConnectionsView({
  kind,
  capabilities,
  onExecuted,
  active = true,
}: {
  kind: "database" | "cache";
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [selectedID, setSelectedID] = usePageState("software", "id", "", true);
  const [selectedLocal, setSelectedLocal] = useState<Connection>();
  const selected =
    selectedLocal?.id === selectedID
      ? selectedLocal
      : selectedID
        ? ({ id: selectedID } as Connection)
        : undefined;
  const setSelected = (item: Connection) => {
    setSelectedLocal(item);
    setSelectedID(item.id);
  };
  const open = !!selectedID;
  const setOpen = (value: boolean) => {
    if (!value) setSelectedID("");
  };
  const [createOpen, setCreateOpen] = usePageState(
    "software",
    "create",
    false,
    true,
  );
  const [offset, setOffset] = usePageState("software", "offset", 0);
  const { ref: listRef, limit } = usePageSize(76);
  const [group, setGroup] = useState("status");
  const [operation, setOperation] = useState<string>();
  const name =
    kind === "database"
      ? "database_connections_list"
      : "cache_connections_list";
  const resources = useResource<Page<Connection>>(
    name,
    {},
    active && capabilities.some((c) => c.name === name),
  );
  const rows = list(resources.data).filter(
    (row) => kind !== "database" || row.engine !== "mongodb",
  );
  const paged = pageItems(rows, offset, limit);
  const detail = useResource<Connection>(
    `${kind}_connection_get`,
    { id: selected?.id },
    active &&
      open &&
      !!selected &&
      !(kind === "database" && selected.engine === "mongodb") &&
      capabilities.some((c) => c.name === `${kind}_connection_get`),
  );
  const current =
    detail.data ??
    rows.find((r) => r.id === selected?.id) ??
    (kind === "database" && selected?.engine === "mongodb"
      ? undefined
      : selected);
  const rawRegister = capabilities.find(
    (c) => c.name === `${kind}_connection_register`,
  );
  const registerProperties = rawRegister?.input_schema?.properties as
    | Record<string, unknown>
    | undefined;
  const engineSchema = registerProperties?.engine as
    | Record<string, unknown>
    | undefined;
  const register = rawRegister
    ? kind === "database"
      ? {
          ...rawRegister,
          input_schema: {
            ...rawRegister.input_schema,
            properties: {
              ...registerProperties,
              engine: {
                ...engineSchema,
                enum: ["mysql", "mariadb", "postgresql"],
              },
            },
          },
        }
      : rawRegister
    : undefined;
  const title =
    kind === "database" ? translate("数据库连接") : translate("缓存空间");
  const refresh = () => {
    void resources.reload();
    void detail.reload();
    onExecuted();
  };
  const accessLabel = (access?: string) =>
    access === "instance" || access === "database"
      ? choiceLabel(access, "access")
      : translate("授权范围未提供");
  const allowed = (name: string) => {
    if (kind === "database") {
      if (name.startsWith("mongodb_")) return current?.engine === "mongodb";
      if (name === "database_query")
        return current?.engine !== "mongodb" && current?.access === "database";
      if (name === "database_instance_query")
        return current?.engine !== "mongodb" && current?.access === "instance";
      return name.startsWith("database_");
    }
    if (
      current?.engine === "memcached" &&
      /hash_|list_|set_|keys_scan|key_type/.test(name)
    )
      return false;
    if (name.startsWith("cache_cluster_"))
      return current?.engine === "redis" || current?.engine === "valkey";
    return name.startsWith("cache_");
  };
  const inGroup = (name: string, selectedGroup = group) =>
    selectedGroup === "cluster"
      ? name.startsWith("cache_cluster_")
      : selectedGroup === "status"
        ? /status|get$|tables_list$|scan$|type$/.test(name)
        : selectedGroup === "query"
          ? /query|documents_/.test(name)
          : selectedGroup === "data"
            ? /key_|keys_|hash_|list_|set_/.test(name)
            : selectedGroup === "config"
              ? /connection_register$|connection_delete$/.test(name)
              : true;
  const groups =
    kind === "database"
      ? [
          { id: "status", label: translate("状态与表") },
          { id: "query", label: translate("查询与数据") },
          { id: "config", label: translate("连接设置") },
        ]
      : [
          { id: "status", label: translate("状态与键列表") },
          { id: "cluster", label: translate("集群状态") },
          { id: "data", label: translate("键与集合") },
          { id: "config", label: translate("连接设置") },
        ];
  const visibleGroups = current
    ? groups.filter((tab) =>
        tab.id === "config"
          ? !!register ||
            capabilities.some(
              (capability) =>
                allowed(capability.name) &&
                /connection_delete$/.test(capability.name),
            )
          : capabilities.some(
              (capability) =>
                allowed(capability.name) && inGroup(capability.name, tab.id),
            ),
      )
    : [];
  const activeGroup = visibleGroups.some((tab) => tab.id === group)
    ? group
    : visibleGroups[0]?.id;
  const preset = current
    ? {
        id: current.id,
        database: current.database,
        expected_revision: current.revision,
      }
    : {};
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <h2>{title}</h2>
        {register && (
          <Button
            label={
              kind === "database"
                ? translate("添加数据库连接")
                : translate("添加缓存空间")
            }
            variant="primary"
            onClick={() => setCreateOpen(true)}
          />
        )}
      </div>

      {resources.error && (
        <ResourceError error={resources.error} state={resources.state} retry={() => void resources.reload()} />
      )}
      {!capabilities.some((c) => c.name === name) ? (
        <Unavailable title={translate("{0}服务暂不可用", [title])} />
      ) : (
        <>
          <div ref={listRef} className="resource-table-wrap">
            <table className="resource-table">
              <thead>
                <tr>
                  <th>{translate("名称")}</th>
                  <th>{translate("引擎 / 地址")}</th>
                  <th>
                    {kind === "database"
                      ? translate("数据库 / 权限")
                      : translate("数据库 / 键前缀")}
                  </th>
                  <th>TLS</th>
                </tr>
              </thead>
              <tbody>
                {paged.items.map((row) => (
                  <tr key={row.id}>
                    <td>
                      <button
                        className="resource-name"
                        onClick={() => {
                          setSelected(row);
                          setOpen(true);
                          setGroup("status");
                          setOperation(undefined);
                        }}
                      >
                        {row.name ?? row.id}
                      </button>
                    </td>
                    <td>
                      {row.engine}
                      <small>
                        {row.host}:{row.port}
                      </small>
                    </td>
                    <td>
                      {row.database}
                      <small>
                        {kind === "database"
                          ? accessLabel(row.access)
                          : row.prefix}
                      </small>
                    </td>
                    <td>
                      {row.tls
                        ? translate("启用TLS校验")
                        : translate("未启用TLS")}
                    </td>
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
            loading={resources.loading}
            onOffset={setOffset}
          />
          {!rows.length && !resources.loading && !resources.error && (
            <Unavailable
              title={translate("暂无{0}", [title])}
              detail={translate(
                "添加已有服务的连接后，服务器会验证可用性与授权。不会自动安装软件或创建数据库。",
              )}
            />
          )}
        </>
      )}
      {register && (
        <ResourceDrawer
          open={createOpen}
          title={
            kind === "database"
              ? translate("添加数据库连接")
              : translate("添加缓存空间")
          }
          onClose={() => setCreateOpen(false)}
        >
          <ActionForm
            capability={register}
            preset={{ expected_revision: 0 }}
            onExecuted={refresh}
          />
        </ResourceDrawer>
      )}
      {current && (
        <ResourceDrawer
          open={open}
          title={`${title} · ${current.name ?? current.id}`}
          onClose={() => setOpen(false)}
        >
          {detail.error && (
            <div className="notice danger" role="alert">
              {detail.error}
              <Button
                label={translate("重试连接详情")}
                variant="ghost"
                onClick={() => void detail.reload()}
              />
            </div>
          )}
          {detail.loading && (
            <p role="status">{translate("正在读取连接的实际配置与版本…")}</p>
          )}
          <div className="detail-grid">
            <div>
              <small>{translate("服务器")}</small>
              <strong>
                {current.host}:{current.port}
              </strong>
              <span>{current.engine}</span>
            </div>
            <div>
              <small>
                {kind === "database"
                  ? translate("数据库与角色")
                  : translate("数据库与键前缀")}
              </small>
              <strong>
                {current.database}
                {current.prefix ?? ""}
              </strong>
              <span>
                {kind === "database"
                  ? accessLabel(current.access)
                  : current.tls
                    ? translate("启用TLS校验")
                    : translate("未启用TLS")}
              </span>
            </div>
          </div>
          {typeof current.resource === "string" && (
            <p className="muted">
              {translate("授权资源")}
              <code>{current.resource}</code>
            </p>
          )}
          {kind === "database" && (
            <p className="muted">
              {translate(
                "这里的范围控制连接使用哪个身份。数据库账户的只读、数据读写、完整库权限在所属软件实例中管理；客户端仍需获授相应动作和资源。",
              )}
            </p>
          )}
          {kind === "cache" && (
            <p className="muted">
              {translate("填写键时不需要添加前缀；此空间使用")}
              {current.prefix ?? translate("未提供")}
              {translate(": 前缀。当前客户端的读写范围由所授动作和资源决定。")}
            </p>
          )}
          {current.engine === "memcached" && (
            <p className="muted">
              {translate(
                "Memcached提供键的读取、写入与删除，不提供键扫描或集合数据类型。",
              )}
            </p>
          )}
          <div
            className="section-tabs"
            role="tablist"
            aria-label={translate("{0}分类", [title])}
          >
            {visibleGroups.map((tab) => (
              <button
                role="tab"
                aria-selected={activeGroup === tab.id}
                key={tab.id}
                onClick={() => {
                  setGroup(tab.id);
                  setOperation(undefined);
                }}
              >
                {tab.label}
              </button>
            ))}
          </div>
          {activeGroup === "config" && register ? (
            <>
              {kind === "database" && current.instance_id ? (
                <p className="notice warning">
                  {translate("此连接由软件实例")}
                  {current.instance_id}{" "}
                  {translate(
                    "的数据库账户管理。请在实例数据库中修改密码或账户权限；这里可以删除连接记录，数据库内容保留。",
                  )}
                </p>
              ) : detail.data ? (
                <ActionForm
                  capability={register}
                  preset={
                    projectInput(register.input_schema as Schema, {
                      ...current,
                      expected_revision: current.revision,
                    }) as Record<string, unknown>
                  }
                  onExecuted={refresh}
                />
              ) : (
                <Unavailable
                  title={translate("请先读取当前连接版本")}
                  detail={translate("实际版本读取后才能更新连接。")}
                />
              )}
              <ResourceActions
                capabilities={capabilities}
                names={(name) =>
                  allowed(name) && /connection_delete$/.test(name)
                }
                preset={preset}
                selected={operation}
                onSelect={setOperation}
                onExecuted={refresh}
              />
            </>
          ) : (
            <ResourceActions
              capabilities={capabilities}
              names={(name) =>
                allowed(name) &&
                inGroup(name, activeGroup ?? "status") &&
                !name.endsWith("connections_list") &&
                !name.endsWith("connection_register")
              }
              preset={preset}
              selected={operation}
              onSelect={setOperation}
              onExecuted={refresh}
            />
          )}
        </ResourceDrawer>
      )}
    </Card>
  );
}
