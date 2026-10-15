"use client";
import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import {
  action,
  apiErrorMessage,
  bytes,
  Capability,
  fullDate,
  intentKey,
} from "@/lib/api";
import {
  sitePHPCapability,
  sitePHPSessionCleanupInput,
  sitePHPSessionPreview,
  sitePHPSessionPreviewExpired,
  sitePHPSessionStats,
  sitePHPSlowlog,
} from "@/lib/site-php-maintenance";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { Unavailable } from "./resource-ui";

const ageLabel = (seconds: number) =>
  seconds % 86400 === 0
    ? `${seconds / 86400} 天`
    : seconds % 3600 === 0
      ? `${seconds / 3600} 小时`
      : `${seconds} 秒`;

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

export function SitePHPMaintenance({
  siteID,
  siteRevision,
  phpInstance,
  capabilities,
  onExecuted,
  active = true,
}: {
  siteID: string;
  siteRevision: number;
  phpInstance: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const slowlogCapability = sitePHPCapability(
    capabilities,
    "sites_php_slowlog_read",
    "sites:read",
    ["id"],
    false,
    true,
    ["id", "limit"],
  );
  const statsCapability = sitePHPCapability(
    capabilities,
    "sites_php_session_stats",
    "sites:read",
    ["id"],
    false,
    true,
    ["id", "older_than_seconds"],
  );
  const previewCapability = sitePHPCapability(
    capabilities,
    "sites_php_session_preview",
    "sites:read",
    ["id", "expected_revision", "older_than_seconds"],
    false,
    false,
  );
  const cleanupCapability = sitePHPCapability(
    capabilities,
    "sites_php_session_cleanup",
    "sites:write",
    ["id", "expected_revision", "preview_id"],
    true,
    false,
  );
  const ageSeconds = 86400;
  const statsResource = useResource<unknown>(
    "sites_php_session_stats",
    { id: siteID, older_than_seconds: ageSeconds },
    active && !!phpInstance && !!statsCapability,
    0,
  );
  const stats = sitePHPSessionStats(
    statsResource.data,
    siteID,
    siteRevision,
    phpInstance,
  );
  const [slowlog, setSlowlog] = useState<ReturnType<typeof sitePHPSlowlog>>();
  const [preview, setPreview] = useState<ReturnType<typeof sitePHPSessionPreview>>();
  const [task, setTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const previewIntent = useRef<{ body: string; key: string } | undefined>(undefined);
  const cleanupIntent = useRef<{ previewID: string; key: string } | undefined>(undefined);

  const refresh = () => {
    void statsResource.reload();
    onExecuted();
  };

  useEffect(() => {
    setPreview(undefined);
    previewIntent.current = undefined;
    cleanupIntent.current = undefined;
    void statsResource.reload();
  }, [siteRevision]);

  const readSlowlog = async () => {
    if (!slowlogCapability || !phpInstance) return;
    setBusy(true);
    setError("");
    setSlowlog(undefined);
    try {
      const value = await action<unknown>("sites_php_slowlog_read", {
        id: siteID,
        limit: 100,
      });
      const parsed = sitePHPSlowlog(value, siteID, siteRevision, phpInstance);
      if (!parsed) throw new Error("网站慢日志响应未匹配当前网站版本或PHP实例。");
      setSlowlog(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const makePreview = async () => {
    if (!stats || !previewCapability || !stats.allow_actions.includes("preview")) return;
    const input = {
      id: siteID,
      expected_revision: siteRevision,
      older_than_seconds: stats.older_than_seconds,
    };
    const body = JSON.stringify(input);
    if (previewIntent.current?.body !== body)
      previewIntent.current = { body, key: intentKey() };
    setBusy(true);
    setError("");
    setPreview(undefined);
    try {
      const value = await action<unknown>(
        "sites_php_session_preview",
        input,
        { idempotencyKey: previewIntent.current.key },
      );
      const parsed = sitePHPSessionPreview(
        value,
        siteID,
        siteRevision,
        phpInstance,
      );
      if (!parsed) throw new Error("网站会话清理预览无效，请重新读取统计后预览。");
      setPreview(parsed);
      previewIntent.current = undefined;
      cleanupIntent.current = undefined;
    } catch (e) {
      setError(`${apiErrorMessage(e)} 未自动重试；同一预览请求重试会复用请求编号。`);
    } finally {
      setBusy(false);
    }
  };

  const cleanup = async () => {
    if (!preview || !cleanupCapability) return;
    let input: ReturnType<typeof sitePHPSessionCleanupInput>;
    try {
      input = sitePHPSessionCleanupInput(preview, siteRevision);
    } catch (e) {
      setError(apiErrorMessage(e));
      setPreview(undefined);
      refresh();
      return;
    }
    if (
      !window.confirm(
        `确认清理网站 ${siteID} 中超过 ${ageLabel(preview.older_than_seconds)} 的 ${preview.candidate_count} 个受管 PHP 会话文件（约 ${bytes(preview.candidate_bytes)}）？任务只处理此次预览捕获的文件，不显示会话编号或磁盘路径。`,
      )
    )
      return;
    if (cleanupIntent.current?.previewID !== preview.preview_id)
      cleanupIntent.current = { previewID: preview.preview_id, key: intentKey() };
    setBusy(true);
    setError("");
    setTask(undefined);
    try {
      const value = await action<unknown>(
        "sites_php_session_cleanup",
        input,
        { idempotencyKey: cleanupIntent.current.key },
      );
      if (!isRecord(value) || (typeof value.id !== "string" && typeof value.task_id !== "string"))
        throw new Error("清理任务未返回任务编号；请先检查任务状态，不会自动重试。");
      setTask(value);
      setTaskPending(true);
      setPreview(undefined);
      cleanupIntent.current = undefined;
      refresh();
    } catch (e) {
      setError(`${apiErrorMessage(e)} 未自动重试；如需重试，仅会复用本次预览和请求编号。`);
      refresh();
    } finally {
      setBusy(false);
    }
  };

  const previewExpired = preview ? sitePHPSessionPreviewExpired(preview) : false;
  const previewStale =
    !!preview &&
    (preview.site_id !== siteID ||
      preview.site_revision !== siteRevision ||
      preview.php_instance !== phpInstance ||
      !stats ||
      !!statsResource.error ||
      stats.site_revision !== preview.site_revision);
  const canPreview =
    !!stats &&
    !!previewCapability &&
    stats.allow_actions.includes("preview") &&
    !busy &&
    !taskPending;
  const canCleanup =
    !!preview &&
    !!cleanupCapability &&
    preview.allow_actions.includes("cleanup") &&
    !previewExpired &&
    !previewStale &&
    !busy &&
    !taskPending;

  return (
    <section className="stack" aria-label="网站PHP慢日志与会话维护">
      <h3>此网站 PHP 池慢日志</h3>
      {!phpInstance ? (
        <Unavailable title="此网站没有绑定受管 PHP 实例" detail="分配受管 PHP 运行环境后，才能读取独立的网站慢日志。" />
      ) : !slowlogCapability ? (
        <Unavailable title="当前账户或后端未提供网站慢日志读取能力" />
      ) : (
        <>
          <p className="muted">只读取当前网站 PHP 池的受管慢日志，最多 100 行；路径由服务端从网站日志目录解析。</p>
          <Button label={busy ? "正在读取慢日志…" : "读取网站慢日志"} isDisabled={busy || taskPending} onClick={() => void readSlowlog()} />
          {slowlog && !slowlog.configured && (
            <p className="muted">此网站的慢日志未启用。可在 PHP 选项中打开 `slowlog_enabled` 并设置阈值。</p>
          )}
          {slowlog?.configured && !slowlog.generated && (
            <p className="muted">慢日志已配置，当前尚未生成日志文件。</p>
          )}
          {slowlog?.generated && (
            <>
              {slowlog.truncated && <p className="muted">日志内容已按行数或大小截断。</p>}
              {slowlog.lines.length ? <pre className="json-result">{slowlog.lines.join("\n")}</pre> : <p className="muted">当前慢日志为空。</p>}
            </>
          )}
        </>
      )}

      <div className="card-heading">
        <h3>网站 PHP 会话</h3>
        {statsCapability && phpInstance && (
          <Button label="刷新统计" variant="ghost" isDisabled={statsResource.loading || busy} onClick={() => void statsResource.reload()} />
        )}
      </div>
      {!phpInstance ? (
        <Unavailable title="此网站没有绑定受管 PHP 实例" />
      ) : !statsCapability ? (
        <Unavailable title="当前账户没有网站会话统计权限" />
      ) : statsResource.error ? (
        <div className="notice danger" role="alert">{statsResource.error}</div>
      ) : !stats ? (
        statsResource.data !== undefined ? (
          <Unavailable title="网站会话统计版本与当前站点不匹配，请重新读取状态" />
        ) : (
          <p role="status">正在读取网站会话统计…</p>
        )
      ) : (
        <>
          <p>
            {stats.generated ? "受管网站会话目录" : "尚无受管会话目录"} · 共 {stats.total_sessions} 个 · 超过 {ageLabel(stats.older_than_seconds)} {stats.expired_sessions} 个
            {stats.oldest_age_seconds !== undefined ? ` · 最早约 ${ageLabel(stats.oldest_age_seconds)}` : ""}
          </p>
          <p className="muted">仅统计并清理此网站的文件会话；其他临时文件、会话编号和路径不会返回。</p>
          {stats.allow_actions.includes("preview") && !previewCapability && (
            <Unavailable title="服务允许生成预览，但当前账户没有预览权限" />
          )}
          {stats.allow_actions.includes("preview") && previewCapability && (
            <Button label={preview ? "重新生成清理预览" : "预览过期会话清理"} isDisabled={!canPreview} onClick={() => void makePreview()} />
          )}
          {stats.allow_actions.includes("preview") && !cleanupCapability && (
            <p className="muted">当前账户没有网站会话清理任务权限。</p>
          )}
        </>
      )}

      {preview?.site_id === siteID && (
        <div className="stack notice warning" aria-label="网站会话清理预览">
          <strong>清理预览 · 网站版本 {preview.site_revision}</strong>
          <p>将清理 {preview.candidate_count} 个超过 {ageLabel(preview.older_than_seconds)} 的文件，约 {bytes(preview.candidate_bytes)}。</p>
          <p className="muted">预览于 {fullDate(preview.captured_at)} 生成，有效至 {fullDate(preview.expires_at)}。只处理预览捕获且仍匹配的受管会话文件。</p>
          {previewExpired && <p role="alert">预览已过期，请重新读取统计并预览。</p>}
          {previewStale && <p role="alert">网站或PHP绑定版本已变化，此预览不能提交。</p>}
          {preview.candidate_count > 0 && preview.allow_actions.includes("cleanup") && !cleanupCapability && (
            <p className="muted">当前账户没有提交清理任务的权限。</p>
          )}
          {preview.candidate_count > 0 && cleanupCapability && (
            <Button label={`确认清理 ${preview.candidate_count} 个会话文件`} isDisabled={!canCleanup} onClick={() => void cleanup()} />
          )}
          <Button label="取消预览" variant="ghost" isDisabled={busy} onClick={() => setPreview(undefined)} />
        </div>
      )}
      {error && <div className="notice danger" role="alert">{error}</div>}
      {task !== undefined && (
        <TaskResult
          key={JSON.stringify(task)}
          submitted={task}
          capabilities={capabilities}
          onFinished={() => {
            setTaskPending(false);
            refresh();
          }}
        />
      )}
      {taskPending && <p role="status">网站会话清理任务正在运行；完成前不会重复提交。</p>}
    </section>
  );
}
