"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { bytes, Capability, fullDate, list } from "@/lib/api";
import { choiceLabel } from "@/lib/labels";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { Pager, ResourceActions, Unavailable } from "./resource-ui";
type Database = {
  database: string;
  username: string;
  permission: string;
  revision: number;
};
type Backup = {
  id: string;
  filename: string;
  bytes: number;
  sha256: string;
  created_at: string;
  state?: string;
};
export function ManagedDatabaseResources({
  instanceID,
  current,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  current: Database;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [group, setGroup] = useState("accounts");
  const [operation, setOperation] = useState<string>();
  const [selectedBackup, setSelectedBackup] = useState<string>();
  const [creatingBackup, setCreatingBackup] = useState(false);
  const [offset, setOffset] = useState(0);
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const backups = useResource<Page<Backup>>(
    "managed_database_backups_list",
    { instance_id: instanceID, database: current.database },
    active && group === "backups" && has("managed_database_backups_list"),
  );
  const rows = list(backups.data);
  const backup = rows.find((row) => row.id === selectedBackup);
  const create = capabilities.find((c) => c.name === "managed_database_backup");
  const preset = {
    instance_id: instanceID,
    database: current.database,
    username: current.username,
    permission: current.permission,
    expected_revision: current.revision,
  };
  const refresh = () => {
    void backups.reload();
    onExecuted();
  };
  return (
    <section className="stack" aria-label="数据库资源管理">
      <div className="detail-grid">
        <div>
          <small>数据库账户</small>
          <strong>{current.username}</strong>
          <span>{current.database}</span>
        </div>
        <div>
          <small>账户权限</small>
          <strong>{choiceLabel(current.permission, "permission")}</strong>
          <span className="muted">版本 {current.revision}</span>
        </div>
      </div>
      <p className="muted">
        授权资源 <code>{`db:${instanceID}:${current.database}`}</code>
        。网页和MCP检查同一资源；账户权限与客户端获授的动作范围分别生效。
      </p>
      <div className="section-tabs" role="tablist" aria-label="数据库资源分类">
        {[
          { id: "accounts", label: "权限与账户" },
          { id: "backups", label: "备份" },
          { id: "diagnostics", label: "诊断与复制" },
        ].map((tab) => (
          <button
            key={tab.id}
            role="tab"
            aria-selected={group === tab.id}
            onClick={() => {
              setGroup(tab.id);
              setOperation(undefined);
            }}
          >
            {tab.label}
          </button>
        ))}
      </div>
      {group === "backups" ? (
        <>
          <div className="card-heading">
            <h3>实际数据库备份</h3>
            {create && (
              <Button
                label="创建数据库备份"
                onClick={() => {
                  setCreatingBackup(!creatingBackup);
                  setOperation(undefined);
                }}
              />
            )}
          </div>
          {backups.error && (
            <div className="notice danger" role="alert">
              {backups.error}
            </div>
          )}
          {!has("managed_database_backups_list") ? (
            <Unavailable title="当前没有数据库备份读取权限" />
          ) : (
            <>
              {backups.loading && <p role="status">正在读取实际备份…</p>}
              <div className="instance-list">
                {rows.slice(offset, offset + 20).map((row) => (
                  <button
                    key={row.id}
                    className="instance-row"
                    aria-pressed={row.id === selectedBackup}
                    onClick={() => {
                      setSelectedBackup(row.id);
                      setOperation(undefined);
                      setCreatingBackup(false);
                    }}
                  >
                    <div>
                      <strong>{fullDate(row.created_at)}</strong>
                      <p>
                        {bytes(row.bytes)} · {row.filename}
                      </p>
                      <small className="monospace">SHA-256 {row.sha256}</small>
                    </div>
                  </button>
                ))}
              </div>
              {!rows.length && !backups.loading && !backups.error && (
                <Unavailable
                  title="暂无数据库备份"
                  detail="备份任务完成、产物完整校验后才会列出。"
                />
              )}
              <Pager
                offset={offset}
                limit={20}
                total={rows.length}
                count={Math.max(0, Math.min(20, rows.length - offset))}
                loading={backups.loading}
                onOffset={setOffset}
              />
            </>
          )}
          {creatingBackup && create && (
            <ActionForm
              capability={create}
              preset={preset}
              onBack={() => setCreatingBackup(false)}
              onExecuted={refresh}
            />
          )}
          {backup && !creatingBackup && (
            <>
              {has("managed_database_backup_download") && (
                <Button
                  label="下载已验证数据库备份"
                  href={`/api/v1/databases/backups/download?${new URLSearchParams({ instance_id: instanceID, database: current.database, backup_id: backup.id })}`}
                />
              )}
              <ResourceActions
                capabilities={capabilities}
                names={(name) =>
                  name === "managed_database_restore" ||
                  name === "managed_database_backup_delete"
                }
                preset={{ ...preset, backup_id: backup.id }}
                selected={operation}
                onSelect={setOperation}
                onExecuted={refresh}
              />
            </>
          )}
        </>
      ) : (
        <ResourceActions
          capabilities={capabilities}
          names={(name) =>
            group === "diagnostics"
              ? name === "managed_database_diagnostics" ||
                name === "managed_database_replication_status"
              : [
                  "managed_database_set_permission",
                  "managed_database_credentials",
                  "managed_database_set_password",
                  "managed_database_delete",
                ].includes(name)
          }
          preset={
            group === "diagnostics" ? { instance_id: instanceID } : preset
          }
          selected={operation}
          onSelect={setOperation}
          onExecuted={refresh}
        />
      )}
    </section>
  );
}
