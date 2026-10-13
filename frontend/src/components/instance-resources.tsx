"use client";
import { useState } from "react";
import dynamic from "next/dynamic";
const ManagedDatabaseResources = dynamic(
  () => import("./managed-databases").then((m) => m.ManagedDatabaseResources),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { Capability, list } from "@/lib/api";
import { SoftwareInstance } from "@/lib/resources";
import { choiceLabel } from "@/lib/labels";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { DatabaseRecreation } from "./database-recreation";
import { Pager, ResourceActions, State, Unavailable } from "./resource-ui";
type Row = Record<string, unknown>;
type Inventory = { items: unknown; total?: number; revision?: number };
const databases = new Set([
  "mysql",
  "mysql-community",
  "mariadb",
  "postgresql",
  "mongodb",
]);
export const hasInstanceResources = (id: string) =>
  databases.has(id) ||
  ["docker", "pureftp", "rsyncd", "supervisor"].includes(id);
export function InstanceResources({
  instance,
  capabilities,
  onExecuted,
  active = true,
}: {
  instance: SoftwareInstance;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const kind = databases.has(instance.plugin_id)
    ? "database"
    : instance.plugin_id === "pureftp"
      ? "ftp"
      : instance.plugin_id === "rsyncd"
        ? "rsync"
        : instance.plugin_id;
  const [collection, setCollection] = useState("containers");
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<string>();
  const [operation, setOperation] = useState<string>();
  const [creating, setCreating] = useState(false);
  const name =
    kind === "database"
      ? "managed_databases_list"
      : kind === "docker"
        ? `docker_${collection}_list`
        : `${kind}_list`;
  const data = useResource<Inventory>(
    name,
    kind === "database" ? { instance_id: instance.id } : { id: instance.id },
    active && capabilities.some((c) => c.name === name),
  );
  const raw = data.data?.items;
  const rows = (
    Array.isArray(raw)
      ? raw
      : raw && typeof raw === "object" && "Volumes" in raw
        ? ((raw as { Volumes?: unknown[] }).Volumes ?? [])
        : []
  ).filter(
    (row): row is Row =>
      !!row && typeof row === "object" && !Array.isArray(row),
  );
  const ownedPrefix = `powerlinux-${instance.id}-`;
  const targetName = (row: Row) => {
    if (kind === "database")
      return typeof row.database === "string" ? row.database : "";
    if (kind !== "docker") return typeof row.name === "string" ? row.name : "";
    const full = String(
      collection === "containers" && Array.isArray(row.Names)
        ? (row.Names[0] ?? "")
        : (row.Name ?? ""),
    ).replace(/^\//, "");
    return full.startsWith(ownedPrefix) ? full.slice(ownedPrefix.length) : "";
  };
  const rowID = (row: Row, index: number) =>
    String(row.Id ?? row.ID ?? targetName(row) ?? index);
  const current = rows.find((row, index) => rowID(row, index) === selected);
  const createName =
    kind === "database"
      ? "managed_database_create"
      : kind === "docker"
        ? collection === "containers"
          ? "docker_container_create"
          : collection === "images"
            ? "docker_image_pull"
            : `docker_${collection === "networks" ? "network" : "volume"}_create`
        : `${kind}_create`;
  const create = capabilities.find((c) => c.name === createName);
  const databaseReceipt = capabilities.find(
    (c) => c.name === "managed_database_receipt",
  );
  const base =
    kind === "database" ? { instance_id: instance.id } : { id: instance.id };
  const revision =
    kind === "database" ? current?.revision : data.data?.revision;
  const preset = {
    ...base,
    ...(current
      ? kind === "database"
        ? { database: targetName(current) }
        : { name: targetName(current) }
      : {}),
    ...(typeof revision === "number" ? { expected_revision: revision } : {}),
  };
  const refresh = () => {
    void data.reload();
    onExecuted();
  };
  return (
    <section className="stack" aria-label="软件实例资源">
      <div className="card-heading">
        <h3>
          {kind === "database"
            ? "实例数据库"
            : kind === "docker"
              ? "容器资源"
              : kind === "supervisor"
                ? "守护程序"
                : "服务账户"}
        </h3>
        {create && (kind !== "database" || databaseReceipt) && (
          <Button
            label={
              kind === "database"
                ? "查询后新建或重建数据库"
                : kind === "docker"
                  ? collection === "containers"
                    ? "创建容器"
                    : collection === "images"
                      ? "拉取镜像"
                      : collection === "networks"
                        ? "创建网络"
                        : "创建数据卷"
                  : kind === "supervisor"
                    ? "添加守护程序"
                    : "添加服务账户"
            }
            onClick={() => {
              setCreating(!creating);
              setOperation(undefined);
            }}
          />
        )}
      </div>
      {kind === "docker" && (
        <div
          className="section-tabs"
          role="tablist"
          aria-label="Docker资源类型"
        >
          {[
            { id: "containers", label: "容器" },
            { id: "images", label: "镜像" },
            { id: "networks", label: "网络" },
            { id: "volumes", label: "数据卷" },
          ].map((tab) => (
            <button
              key={tab.id}
              role="tab"
              aria-selected={collection === tab.id}
              onClick={() => {
                setCollection(tab.id);
                setSelected(undefined);
                setOperation(undefined);
                setOffset(0);
                setCreating(false);
              }}
            >
              {tab.label}
            </button>
          ))}
        </div>
      )}
      {data.error && (
        <div className="notice danger" role="alert">
          {data.error}
        </div>
      )}
      {kind === "database" && create && !databaseReceipt && (
        <Unavailable
          title="缺少数据库管理记录读取权限"
          detail="安全创建流程需要先读取指定名称的权威记录。"
        />
      )}
      {!capabilities.some((c) => c.name === name) ? (
        <Unavailable title="当前没有此资源的读取权限" />
      ) : (
        <>
          <div className="instance-list">
            {rows.slice(offset, offset + 20).map((row, index) => {
              const target = targetName(row);
              const id = rowID(row, offset + index);
              const image = Array.isArray(row.RepoDigests)
                ? row.RepoDigests.join(" · ")
                : String(row.Image ?? row.Id ?? "");
              return (
                <button
                  key={id || index}
                  className="instance-row"
                  disabled={
                    kind === "docker" && (!target || collection === "images")
                  }
                  aria-pressed={selected === id}
                  onClick={() => {
                    setSelected(id);
                    setOperation(undefined);
                    setCreating(false);
                  }}
                >
                  <div>
                    <strong>
                      {target ||
                        image ||
                        String(row.Name ?? row.ID ?? "未提供名称")}
                    </strong>
                    <p>
                      {kind === "database"
                        ? `${row.username ?? ""} · ${row.engine ?? ""} · ${choiceLabel(row.permission, "permission")}`
                        : kind === "docker"
                          ? String(row.Status ?? row.Driver ?? image)
                          : kind === "supervisor"
                            ? String(row.program_id ?? "")
                            : row.read_only === true
                              ? "只读"
                              : "读写"}
                    </p>
                  </div>
                  {typeof row.state === "string" ? (
                    <State value={row.state} />
                  ) : typeof row.State === "string" ? (
                    <State value={row.State} />
                  ) : typeof row.enabled === "boolean" ? (
                    <State value={row.enabled ? "active" : "disabled"} />
                  ) : null}
                </button>
              );
            })}
          </div>
          {!rows.length && !data.loading && !data.error && (
            <Unavailable title="暂无可见资源" />
          )}
          <Pager
            offset={offset}
            limit={20}
            total={rows.length}
            count={Math.min(20, rows.length - offset)}
            loading={data.loading}
            onOffset={setOffset}
          />
        </>
      )}
      {creating && create && kind === "database" && databaseReceipt && (
        <DatabaseRecreation
          instanceID={instance.id}
          receiptCapability={databaseReceipt}
          createCapability={create}
          onBack={() => setCreating(false)}
          onExecuted={refresh}
        />
      )}
      {creating && create && kind !== "database" && (
        <ActionForm
          key={create.name + instance.id}
          capability={create}
          preset={{
            ...base,
            ...(typeof data.data?.revision === "number"
              ? { expected_revision: data.data.revision }
              : {}),
          }}
          onBack={() => setCreating(false)}
          onExecuted={refresh}
        />
      )}
      {current &&
        !creating &&
        (kind === "database" ? (
          typeof current.database === "string" &&
          typeof current.username === "string" &&
          typeof current.engine === "string" &&
          typeof current.permission === "string" &&
          typeof current.revision === "number" &&
          Number.isSafeInteger(current.revision) &&
          typeof current.state === "string" ? (
            <ManagedDatabaseResources
              key={instance.id + ":" + current.database}
              instanceID={instance.id}
              current={{
                database: current.database,
                username: current.username,
                engine: current.engine,
                permission: current.permission,
                revision: current.revision,
                state: current.state,
              }}
              capabilities={capabilities}
              onExecuted={refresh}
              active={active}
            />
          ) : (
            <Unavailable
              title="数据库详情缺少状态或有效版本"
              detail="请重新读取资源；记录状态和精确版本确认后才能进行管理操作。"
            />
          )
        ) : (
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              kind === "docker"
                ? name.startsWith(
                    `docker_${collection === "containers" ? "container" : collection === "networks" ? "network" : "volume"}_`,
                  ) && !name.endsWith("_create")
                : name.startsWith(kind + "_") &&
                  !/_list|_status|_create/.test(name)
            }
            preset={preset}
            selected={operation}
            onSelect={setOperation}
            onExecuted={refresh}
          />
        ))}
    </section>
  );
}
