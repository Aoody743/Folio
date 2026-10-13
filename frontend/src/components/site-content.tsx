"use client";
import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import {
  action,
  APIError,
  Capability,
  bytes,
  fullDate,
  intentKey,
  list,
  Task,
} from "@/lib/api";
import { Page, Site } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { readDraft, saveDraft, forgetDraft } from "@/lib/draft-cache";
import { verifiedSiteBackupDownloadHref } from "@/lib/site-backup-download";
import { ActionForm } from "./actions";
import { Pager, ResourceActions, Unavailable } from "./resource-ui";
import { TaskResult } from "./task-result";

type DefaultSelection = { selected: Site; previous: Site | null };
type DefaultIntent = { body: string; key: string };
const isFinished = (task?: Partial<Task>) =>
  ["succeeded", "failed", "canceled", "cancelled", "interrupted"].includes(
    task?.status ?? task?.state ?? "",
  );
export function SiteDefault({
  id,
  capabilities,
  onExecuted,
  active = true,
}: {
  id: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const cacheKey = `site-default:${id}`;
  const [recovery, setRecovery] = useState(
    () => !!readDraft(cacheKey + ":recovery"),
  );
  const [submitted, setSubmitted] = useState<unknown>(() =>
    readDraft(cacheKey + ":task"),
  );
  const [pending, setPending] = useState(() => {
    const original = readDraft(cacheKey + ":task") as Partial<Task> | undefined;
    return !!original && !isFinished(original);
  });
  const request = useRef<DefaultIntent | undefined>(
    readDraft(cacheKey + ":intent") as DefaultIntent | undefined,
  );
  const canRead = capabilities.some((c) => c.name === "sites_default_get");
  const canWrite = capabilities.some((c) => c.name === "sites_default_set");
  const selection = useResource<DefaultSelection>(
    "sites_default_get",
    { id },
    active && expanded && canRead,
    0,
  );
  const current = selection.data;
  useEffect(() => {
    setConfirm(false);
  }, [
    current?.selected.id,
    current?.selected.revision,
    current?.previous?.id,
    current?.previous?.revision,
  ]);
  if (!canWrite) return null;
  const advanced =
    current?.selected.configuration_mode === "advanced" ||
    current?.previous?.configuration_mode === "advanced";
  const submit = async () => {
    if (!current || recovery || pending || busy || advanced) return;
    const input = {
      id: current.selected.id,
      expected_revision: current.selected.revision,
      previous_site_id: current.previous?.id ?? "",
      previous_revision: current.previous?.revision ?? 0,
    };
    const body = JSON.stringify(input);
    if (!request.current || request.current.body !== body)
      request.current = { body, key: intentKey() };
    saveDraft(cacheKey + ":intent", request.current);
    setBusy(true);
    setError("");
    try {
      const result = await action("sites_default_set", input, {
        idempotencyKey: request.current.key,
      });
      setSubmitted(result);
      saveDraft(cacheKey + ":task", result);
      setPending(true);
      setConfirm(false);
      request.current = undefined;
      forgetDraft(cacheKey + ":intent");
      onExecuted();
    } catch (e) {
      setError((e as Error).message);
      if (e instanceof APIError) {
        if (e.code === "operation_recovery_required") {
          setRecovery(true);
          saveDraft(cacheKey + ":recovery", true);
          const details = e.details as { task_id?: string } | undefined;
          if (details?.task_id) {
            const task = { task_id: details.task_id };
            setSubmitted(task);
            saveDraft(cacheKey + ":task", task);
          }
        } else if (e.status === 409 || e.status === 412) {
          setConfirm(false);
          void selection.reload();
        }
      }
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="stack" aria-label="默认网站切换">
      <div className="card-heading">
        <h3>默认网站</h3>
        <Button
          label={expanded ? "收起默认网站设置" : "设置默认网站"}
          variant="ghost"
          onClick={() => {
            setExpanded(!expanded);
            setConfirm(false);
          }}
        />
      </div>
      {expanded && (
        <>
          {!canRead ? (
            <Unavailable title="当前没有默认网站读取权限" />
          ) : (
            <>
              {selection.loading && (
                <p role="status">正在读取当前默认网站与版本…</p>
              )}
              {selection.error && (
                <div className="notice danger" role="alert">
                  {selection.error}
                </div>
              )}
              {current && (
                <>
                  <div className="detail-grid">
                    <div>
                      <small>将设为默认的网站</small>
                      <strong>{current.selected.name}</strong>
                      <span className="muted">
                        版本 {current.selected.revision} ·{" "}
                        {current.selected.web_instance}
                      </span>
                    </div>
                    <div>
                      <small>原默认网站</small>
                      <strong>
                        {current.previous?.name ??
                          (current.selected.default
                            ? "当前网站已是默认网站"
                            : "此实例未设置默认网站")}
                      </strong>
                      {current.previous && (
                        <span className="muted">
                          版本 {current.previous.revision}
                        </span>
                      )}
                    </div>
                  </div>
                  {advanced && (
                    <div className="notice warning">
                      涉及的网站有手工配置。请在配置中明确恢复面板管理后再切换默认网站。
                    </div>
                  )}
                  {!current.selected.default && (
                    <>
                      {confirm && (
                        <div className="notice warning">
                          <strong>确认切换默认网站</strong>
                          <p>
                            {current.selected.name}{" "}
                            将接收该网站服务实例中未匹配域名的请求。
                            {current.previous
                              ? `${current.previous.name} 将取消默认身份，网站内容保留。`
                              : ""}
                          </p>
                          <p>
                            两份网站版本都会检查；配置验证和实际生效后任务才完成。
                          </p>
                        </div>
                      )}
                      <div className="button-row">
                        <Button
                          label={confirm ? "确认切换默认网站" : "设为默认网站"}
                          variant="primary"
                          isLoading={busy}
                          isDisabled={
                            recovery || pending || advanced || selection.loading
                          }
                          onClick={() =>
                            confirm ? void submit() : setConfirm(true)
                          }
                        />
                        {confirm && (
                          <Button
                            label="取消默认网站切换"
                            onClick={() => setConfirm(false)}
                          />
                        )}
                      </div>
                    </>
                  )}
                </>
              )}
              <Button
                label="重新读取默认网站版本"
                variant="ghost"
                isDisabled={busy || selection.loading}
                onClick={() => {
                  setConfirm(false);
                  void selection.reload();
                }}
              />
            </>
          )}
          {error && (
            <div className="notice danger" role="alert">
              {error}
            </div>
          )}
          {recovery && (
            <div className="notice warning">
              上次操作状态尚未确认，请核对任务和实际默认网站；此请求暂停写入。
            </div>
          )}
          {submitted !== undefined && (
            <TaskResult
              submitted={submitted}
              capabilities={capabilities}
              onFinished={(task) => {
                setPending(false);
                if (task.status === "succeeded" || task.state === "succeeded")
                  setError("");
                saveDraft(cacheKey + ":task", task);
                void selection.reload();
                onExecuted();
              }}
            />
          )}
        </>
      )}
    </section>
  );
}
type Backup = {
  id: string;
  site_id: string;
  created_at: string;
  size: number;
  sha256: string;
  includes_tls: boolean;
};
export function SiteBackups({
  id,
  revision,
  capabilities,
  onExecuted,
  active = true,
}: {
  id: string;
  revision: number;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [selected, setSelected] = useState<string>();
  const [operation, setOperation] = useState<string>();
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const [creating, setCreating] = useState(false);
  const [offset, setOffset] = useState(0);
  const records = useResource<Backup[] | Page<Backup>>(
    "sites_backups_list",
    { id },
    active && capabilities.some((c) => c.name === "sites_backups_list"),
  );
  const rows = list(records.data);
  const current = rows.find((row) => row.id === selected);
  const create = capabilities.find((c) => c.name === "sites_backup_create");
  const download = capabilities.some(
    (c) => c.name === "sites_backup_download",
  );
  const refresh = () => {
    void records.reload();
    onExecuted();
  };
  const downloadBackup = async (backup: Backup) => {
    setDownloading(true);
    setDownloadError("");
    try {
      const result = await action<unknown>("sites_backup_download", {
        id,
        backup_id: backup.id,
      });
      const href = verifiedSiteBackupDownloadHref(
        result,
        {
          siteID: id,
          backupID: backup.id,
          size: backup.size,
          sha256: backup.sha256,
        },
        window.location.origin,
      );
      const anchor = document.createElement("a");
      anchor.href = href;
      anchor.download = `${backup.id}.tar.gz`;
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
    } catch (error) {
      setDownloadError(
        error instanceof APIError && error.code === "forbidden"
          ? "下载被服务器拒绝：请检查网站备份与站点文件读取权限。"
          : error instanceof Error
            ? error.message
            : "网站备份下载准备失败。",
      );
    } finally {
      setDownloading(false);
    }
  };
  return (
    <section className="stack" aria-label="网站备份">
      <div className="card-heading">
        <h3>网站备份</h3>
        {create && (
          <Button
            label="创建网站备份"
            isDisabled={downloading}
            onClick={() => {
              setDownloadError("");
              setCreating(!creating);
              setOperation(undefined);
            }}
          />
        )}
      </div>
      {records.error && (
        <div className="notice danger" role="alert">
          {records.error}
        </div>
      )}
      {!capabilities.some((c) => c.name === "sites_backups_list") ? (
        <Unavailable title="当前没有备份读取权限" />
      ) : (
        <>
          <div className="instance-list">
            {rows.slice(offset, offset + 20).map((row) => (
              <button
                key={row.id}
                className="instance-row"
                aria-pressed={selected === row.id}
                disabled={downloading}
                onClick={() => {
                  setSelected(row.id);
                  setDownloadError("");
                  setOperation(undefined);
                  setCreating(false);
                }}
              >
                <div>
                  <strong>{fullDate(row.created_at)}</strong>
                  <p>
                    {bytes(row.size)} ·{" "}
                    {row.includes_tls ? "包含TLS配置" : "不含TLS配置"}
                  </p>
                  <small className="monospace">SHA-256 {row.sha256}</small>
                </div>
              </button>
            ))}
          </div>
          {!rows.length && !records.loading && !records.error && (
            <Unavailable
              title="暂无网站备份"
              detail="备份任务完成并校验实际产物后，这里会显示备份。"
            />
          )}
          <Pager
            offset={offset}
            limit={20}
            total={rows.length}
            count={Math.min(20, rows.length - offset)}
            loading={records.loading}
            onOffset={setOffset}
          />
        </>
      )}
      {creating && create && (
        <ActionForm
          capability={create}
          preset={{ id, expected_revision: revision }}
          onBack={() => setCreating(false)}
          onExecuted={refresh}
        />
      )}
      {current && !creating && (
        <>
          <p className="muted">
            选定备份 {current.id}。恢复会使用当前网站版本检查，并验证备份产物。
          </p>
          {download && (
            <div className="stack">
              <Button
                label="下载已验证网站备份"
                variant="primary"
                isLoading={downloading}
                isDisabled={downloading}
                onClick={() => void downloadBackup(current)}
              />
              {downloading && (
                <p className="muted" role="status">
                  正在验证备份并准备下载…
                </p>
              )}
              {downloadError && (
                <div className="notice danger" role="alert">
                  {downloadError}
                </div>
              )}
            </div>
          )}
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              name === "sites_backup_restore" || name === "sites_backup_delete"
            }
            preset={{ id, backup_id: current.id, expected_revision: revision }}
            selected={operation}
            onSelect={setOperation}
            onExecuted={refresh}
          />
        </>
      )}
    </section>
  );
}
type Retained = {
  id: string;
  name: string;
  revision: number;
  root_id: string;
  retained_at: string;
};
export function RetainedSites({
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
  const [selected, setSelected] = useState<string>();
  const [operation, setOperation] = useState<string>();
  const [offset, setOffset] = useState(0);
  const records = useResource<Page<Retained>>(
    "sites_retained_list",
    {},
    active && capabilities.some((c) => c.name === "sites_retained_list"),
  );
  const rows = list(records.data);
  const current = rows.find((row) => row.id === selected);
  const refresh = () => {
    void records.reload();
    onExecuted();
  };
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <h2>保留的网站内容</h2>
      </div>
      <p className="muted">
        这里是删除网站后仍保留的内容，保留内容不代表网站正在提供服务。
      </p>
      {records.error && (
        <div className="notice danger" role="alert">
          {records.error}
        </div>
      )}
      {!capabilities.some((c) => c.name === "sites_retained_list") ? (
        <Unavailable title="当前没有保留内容读取权限" />
      ) : (
        <>
          <div className="instance-list">
            {rows.slice(offset, offset + 20).map((row) => (
              <button
                className="instance-row"
                key={row.id}
                aria-pressed={selected === row.id}
                onClick={() => {
                  setSelected(row.id);
                  setOperation(undefined);
                }}
              >
                <div>
                  <strong>{row.name}</strong>
                  <p>保留于 {fullDate(row.retained_at)}</p>
                  <code>{row.root_id}</code>
                </div>
                <span>已删除网站</span>
              </button>
            ))}
          </div>
          {!rows.length && !records.loading && !records.error && (
            <Unavailable title="暂无保留的网站内容" />
          )}
          <Pager
            offset={offset}
            limit={20}
            total={rows.length}
            count={Math.min(20, rows.length - offset)}
            loading={records.loading}
            onOffset={setOffset}
          />
        </>
      )}
      {current && (
        <section className="stack">
          {onFiles && capabilities.some((c) => c.name === "fs_roots") && (
            <Button
              label="查看保留文件"
              onClick={() => onFiles(current.root_id)}
            />
          )}
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              /^sites_retained_/.test(name) && name !== "sites_retained_list"
            }
            preset={{ id: current.id, expected_revision: current.revision }}
            selected={operation}
            onSelect={setOperation}
            onExecuted={refresh}
          />
        </section>
      )}
    </Card>
  );
}
