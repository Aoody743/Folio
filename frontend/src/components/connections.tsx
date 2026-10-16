"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Capability, list } from "@/lib/api";
import { choiceLabel } from "@/lib/labels";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { projectInput, Schema } from "@/lib/schema";
import { ActionForm } from "./actions";
import { ResourceActions, ResourceDrawer, Unavailable } from "./resource-ui";
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
  const [selected, setSelected] = useState<Connection>();
  const [open, setOpen] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
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
  const rows = list(resources.data);
  const detail = useResource<Connection>(
    `${kind}_connection_get`,
    { id: selected?.id },
    active &&
      open &&
      !!selected &&
      capabilities.some((c) => c.name === `${kind}_connection_get`),
  );
  const current =
    detail.data ?? rows.find((r) => r.id === selected?.id) ?? selected;
  const register = capabilities.find(
    (c) => c.name === `${kind}_connection_register`,
  );
  const title = kind === "database" ? "数据库连接" : "缓存空间";
  const refresh = () => {
    void resources.reload();
    void detail.reload();
    onExecuted();
  };
  const accessLabel = (access?: string) =>
    access === "instance" || access === "database"
      ? choiceLabel(access, "access")
      : "授权范围未提供";
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
  const inGroup = (name: string) =>
    group === "cluster"
      ? name.startsWith("cache_cluster_")
      : group === "status"
      ? /status|get$|tables_list$|scan$|type$/.test(name)
      : group === "query"
        ? /query|documents_/.test(name)
        : group === "data"
          ? /key_|keys_|hash_|list_|set_/.test(name)
          : group === "config"
            ? /connection_register$|connection_delete$/.test(name)
            : true;
  const groups =
    kind === "database"
      ? [
          { id: "status", label: "状态与表" },
          { id: "query", label: "查询与数据" },
          { id: "config", label: "连接设置" },
        ]
      : [
          { id: "status", label: "状态与键列表" },
          { id: "cluster", label: "集群状态" },
          { id: "data", label: "键与集合" },
          { id: "config", label: "连接设置" },
        ];
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
            label={kind === "database" ? "添加数据库连接" : "添加缓存空间"}
            variant="primary"
            onClick={() => setCreateOpen(true)}
          />
        )}
      </div>
      <p className="section-caption">
        {kind === "database"
          ? "连接凭据由服务器保护；单库连接必须通过真实权限检查。实例管理员连接拥有独立授权范围。"
          : "每个连接绑定明确的数据库与键前缀。网页和MCP使用同一缓存空间权限。"}
      </p>
      {resources.error && (
        <div className="notice danger" role="alert">
          {resources.error}
        </div>
      )}
      {!capabilities.some((c) => c.name === name) ? (
        <Unavailable title={`${title}服务暂不可用`} />
      ) : (
        <>
          <div className="resource-table-wrap">
            <table className="resource-table">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>引擎 / 地址</th>
                  <th>
                    {kind === "database" ? "数据库 / 权限" : "数据库 / 键前缀"}
                  </th>
                  <th>TLS</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
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
                      <small>{row.id}</small>
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
                    <td>{row.tls ? "启用TLS校验" : "未启用TLS"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {!rows.length && !resources.loading && !resources.error && (
            <Unavailable
              title={`暂无${title}`}
              detail="添加已有服务的连接后，服务器会验证可用性与授权。不会自动安装软件或创建数据库。"
            />
          )}
        </>
      )}
      {register && (
        <ResourceDrawer
          open={createOpen}
          title={kind === "database" ? "添加数据库连接" : "添加缓存空间"}
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
                label="重试连接详情"
                variant="ghost"
                onClick={() => void detail.reload()}
              />
            </div>
          )}
          {detail.loading && <p role="status">正在读取连接的实际配置与版本…</p>}
          <div className="detail-grid">
            <div>
              <small>服务器</small>
              <strong>
                {current.host}:{current.port}
              </strong>
              <span>{current.engine}</span>
            </div>
            <div>
              <small>
                {kind === "database" ? "数据库与角色" : "数据库与键前缀"}
              </small>
              <strong>
                {current.database} {current.prefix ?? ""}
              </strong>
              <span>
                {kind === "database"
                  ? accessLabel(current.access)
                  : current.tls
                    ? "启用TLS校验"
                    : "未启用TLS"}
              </span>
            </div>
          </div>
          {typeof current.resource === "string" && (
            <p className="muted">
              授权资源 <code>{current.resource}</code>
            </p>
          )}
          {kind === "database" && (
            <p className="muted">
              这里的范围控制连接使用哪个身份。数据库账户的只读、数据读写、完整库权限在所属软件实例中管理；客户端仍需获授相应动作和资源。
            </p>
          )}
          {kind === "cache" && (
            <p className="muted">
              填写键时不需要添加前缀；此空间使用 {current.prefix ?? "未提供"}:
              前缀。当前客户端的读写范围由所授动作和资源决定。
            </p>
          )}
          {current.engine === "memcached" && (
            <p className="muted">
              Memcached提供键的读取、写入与删除，不提供键扫描或集合数据类型。
            </p>
          )}
          <div
            className="section-tabs"
            role="tablist"
            aria-label={`${title}分类`}
          >
            {groups.map((tab) => (
              <button
                role="tab"
                aria-selected={group === tab.id}
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
          {group === "config" && register ? (
            <>
              {kind === "database" && current.instance_id ? (
                <p className="notice warning">
                  此连接由软件实例 {current.instance_id}{" "}
                  的数据库账户管理。请在实例数据库中修改密码或账户权限；这里可以删除连接记录，数据库内容保留。
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
                  title="请先读取当前连接版本"
                  detail="实际版本读取后才能更新连接。"
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
                inGroup(name) &&
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
