"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import dynamic from "next/dynamic";
const ManagedDatabaseResources = dynamic(
  () => import("./managed-databases").then((m) => m.ManagedDatabaseResources),
  { ssr: false },
);
const DatabaseBinlogManagement = dynamic(
  () => import("./database-binlog").then((m) => m.DatabaseBinlogManagement),
  { ssr: false },
);
const DockerExecManagement = dynamic(
  () => import("./docker-exec").then((m) => m.DockerExecManagement),
  { ssr: false },
);
const DockerTerminalManagement = dynamic(
  () => import("./docker-terminal").then((m) => m.DockerTerminalManagement),
  { ssr: false },
);
const FTPAccountRecovery = dynamic(
  () => import("./ftp-account-recovery").then((m) => m.FTPAccountRecovery),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { Capability, list } from "@/lib/api";
import { SoftwareInstance } from "@/lib/resources";
import { choiceLabel } from "@/lib/labels";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { DockerManagement } from "./docker-management";
import { DatabaseAdminCredentials } from "./database-admin-credentials";
import { DatabaseRecreation } from "./database-recreation";
import {
  Pager,
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
import { usePageState } from "@/lib/navigation";
import { usePageSize } from "@/lib/collection";
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
  const supportsBinlog = ["mysql", "mysql-community", "mariadb"].includes(
    instance.plugin_id,
  );
  const [collection, setCollection] = useState("containers");
  const [databaseSection, setDatabaseSection] = useState("databases");
  const [offset, setOffset] = useState(0);
  const [resourceID, setResourceID] = usePageState(
    "software",
    "resource",
    "",
    true,
  );
  const selected = resourceID || undefined;
  const setSelected = (value?: string) => setResourceID(value ?? "");
  const { ref: listRef, limit } = usePageSize(76);
  const [operation, setOperation] = useState<string>();
  const [creating, setCreating] = usePageState("software", "new", false, true);
  const name =
    kind === "database"
      ? "managed_databases_list"
      : kind === "docker"
        ? `docker_${collection}_list`
        : `${kind}_list`;
  const data = useResource<Inventory>(
    name,
    kind === "database" ? { instance_id: instance.id } : { id: instance.id },
    active &&
      !(kind === "docker" && collection === "images") &&
      !(kind === "database" && databaseSection === "binlog") &&
      capabilities.some((c) => c.name === name),
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
    <section className="stack" aria-label={translate("软件实例资源")}>
      <div className="card-heading">
        <h3>
          {kind === "database" && databaseSection === "binlog"
            ? translate("Binlog 管理")
            : kind === "database"
              ? translate("实例数据库")
              : kind === "docker"
                ? translate("容器资源")
                : kind === "supervisor"
                  ? translate("守护程序")
                  : translate("服务账户")}
        </h3>
        {(!supportsBinlog || databaseSection === "databases") &&
          create &&
          !(kind === "docker" && collection === "images") &&
          (kind !== "database" || databaseReceipt) && (
            <Button
              label={
                kind === "database"
                  ? translate("查询后新建或重建数据库")
                  : kind === "docker"
                    ? collection === "containers"
                      ? translate("创建容器")
                      : collection === "images"
                        ? translate("拉取镜像")
                        : collection === "networks"
                          ? translate("创建网络")
                          : translate("创建数据卷")
                    : kind === "supervisor"
                      ? translate("添加守护程序")
                      : translate("添加服务账户")
              }
              onClick={() => {
                setCreating(!creating);
                setOperation(undefined);
              }}
            />
          )}
      </div>
      {supportsBinlog && (
        <div
          className="section-tabs"
          role="tablist"
          aria-label={translate("数据库实例工具")}
        >
          {[
            { id: "databases", label: translate("数据库") },
            { id: "binlog", label: "Binlog" },
          ].map((tab) => (
            <button
              key={tab.id}
              role="tab"
              aria-selected={databaseSection === tab.id}
              onClick={() => {
                setDatabaseSection(tab.id);
                setSelected(undefined);
                setOperation(undefined);
                setCreating(false);
              }}
            >
              {tab.label}
            </button>
          ))}
        </div>
      )}
      {kind === "docker" && (
        <div
          className="section-tabs"
          role="tablist"
          aria-label={translate("Docker资源类型")}
        >
          {[
            { id: "containers", label: translate("容器") },
            { id: "images", label: translate("镜像") },
            { id: "networks", label: translate("网络") },
            { id: "volumes", label: translate("数据卷") },
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
      {kind === "database" && databaseSection === "binlog" ? (
        <DatabaseBinlogManagement
          key={instance.id}
          instanceID={instance.id}
          capabilities={capabilities}
          onExecuted={refresh}
          active={active}
        />
      ) : kind === "docker" && collection === "images" ? (
        <DockerManagement
          key={instance.id}
          instanceID={instance.id}
          capabilities={capabilities}
          onExecuted={refresh}
          active={active}
        />
      ) : (
        <>
          {kind === "database" && databaseSection === "databases" && (
            <DatabaseAdminCredentials
              key={instance.id}
              instanceID={instance.id}
              engine={instance.plugin_id}
              capabilities={capabilities}
              onExecuted={refresh}
              active={active}
            />
          )}
          {data.error && (
            <div className="notice danger" role="alert">
              {data.error}
            </div>
          )}
          {kind === "database" && create && !databaseReceipt && (
            <Unavailable
              title={translate("缺少数据库管理记录读取权限")}
              detail={translate("安全创建流程需要先读取指定名称的权威记录。")}
            />
          )}
          {!capabilities.some((c) => c.name === name) ? (
            <Unavailable title={translate("当前没有此资源的读取权限")} />
          ) : (
            <>
              <div ref={listRef} className="instance-list">
                {rows.slice(offset, offset + limit).map((row, index) => {
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
                        kind === "docker" &&
                        (!target || collection === "images")
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
                            String(
                              row.Name ?? row.ID ?? translate("未提供名称"),
                            )}
                        </strong>
                        <p>
                          {kind === "database"
                            ? `${row.username ?? ""} · ${row.engine ?? ""} · ${choiceLabel(row.permission, "permission")}`
                            : kind === "docker"
                              ? String(row.Status ?? row.Driver ?? image)
                              : kind === "supervisor"
                                ? String(row.program_id ?? "")
                                : row.read_only === true
                                  ? translate("只读")
                                  : translate("读写")}
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
                <Unavailable title={translate("暂无可见资源")} />
              )}
              <Pager
                offset={offset}
                limit={limit}
                total={rows.length}
                count={Math.max(0, Math.min(limit, rows.length - offset))}
                loading={data.loading}
                onOffset={setOffset}
              />
            </>
          )}
          {creating && create && kind === "database" && databaseReceipt && (
            <ResourceDrawer
              open={creating}
              title={translate("创建数据库")}
              onClose={() => setCreating(false)}
            >
              <DatabaseRecreation
                instanceID={instance.id}
                receiptCapability={databaseReceipt}
                createCapability={create}
                onBack={() => setCreating(false)}
                onExecuted={refresh}
              />
            </ResourceDrawer>
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
          {current && !creating && (
            <ResourceDrawer
              open={!!current}
              title={targetName(current) || translate("资源详情")}
              onClose={() => setSelected(undefined)}
            >
              {kind === "database" ? (
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
                    title={translate("数据库详情缺少状态或有效版本")}
                    detail={translate(
                      "请重新读取资源；记录状态和精确版本确认后才能进行管理操作。",
                    )}
                  />
                )
              ) : (
                <>
                  {kind === "ftp" && targetName(current) && (
                    <FTPAccountRecovery
                      key={`ftp-account-state:${instance.id}:${targetName(current)}`}
                      id={instance.id}
                      name={targetName(current)}
                      capabilities={capabilities}
                      onExecuted={refresh}
                      active={active}
                    />
                  )}
                  <ResourceActions
                    capabilities={capabilities}
                    names={(name) =>
                      kind === "docker"
                        ? name !== "docker_container_exec" &&
                          name !== "docker_container_exec_status" &&
                          name.startsWith(
                            `docker_${collection === "containers" ? "container" : collection === "networks" ? "network" : "volume"}_`,
                          ) &&
                          !name.endsWith("_create")
                        : name.startsWith(kind + "_") &&
                          !(
                            kind === "ftp" &&
                            name.startsWith("ftp_account_state_")
                          ) &&
                          !name.startsWith("supervisor_log_") &&
                          !/_list|_status|_create/.test(name)
                    }
                    preset={preset}
                    selected={operation}
                    onSelect={setOperation}
                    onExecuted={refresh}
                  />
                  {kind === "docker" &&
                    collection === "containers" &&
                    targetName(current) && (
                      <>
                        <DockerExecManagement
                          key={`${instance.id}:${targetName(current)}`}
                          instanceID={instance.id}
                          containerName={targetName(current)}
                          running={
                            String(current.State ?? "").toLowerCase() ===
                            "running"
                          }
                          capabilities={capabilities}
                          onExecuted={refresh}
                        />
                        <DockerTerminalManagement
                          key={`terminal:${instance.id}:${targetName(current)}`}
                          instanceID={instance.id}
                          containerName={targetName(current)}
                          running={
                            String(current.State ?? "").toLowerCase() ===
                            "running"
                          }
                          capabilities={capabilities}
                          onExecuted={refresh}
                          active={active}
                        />
                      </>
                    )}
                </>
              )}
            </ResourceDrawer>
          )}
        </>
      )}
    </section>
  );
}
