"use client";
import { useState } from "react";
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
  phpMaintenanceCapability,
  phpMaintenanceLog,
  phpWorkerCleanupInput,
  phpWorkerPreviewExpired,
  phpWorkerSessionPreview,
  phpWorkerSessionStats,
} from "@/lib/php-maintenance";
import { SoftwareInstance } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { Unavailable } from "./resource-ui";

const ageLabel = (seconds: number) =>
  seconds % 86400 === 0
    ? `${seconds / 86400} 天`
    : seconds % 3600 === 0
      ? `${seconds / 3600} 小时`
      : `${seconds} 秒`;

export function PHPMaintenance({
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
  const id = instance.id;
  const logCapability = phpMaintenanceCapability(
    capabilities,
    "php_maintenance_log_read",
    "php:read",
    ["id", "stream"],
    false,
  );
  const statsCapability = phpMaintenanceCapability(
    capabilities,
    "php_maintenance_session_stats",
    "php:read",
    ["id"],
    false,
  );
  const previewCapability = phpMaintenanceCapability(
    capabilities,
    "php_maintenance_session_preview",
    "php:read",
    ["id", "expected_revision", "older_than_seconds"],
    false,
    false,
  );
  const cleanupCapability = phpMaintenanceCapability(
    capabilities,
    "php_maintenance_session_cleanup",
    "php:manage",
    ["id", "expected_revision", "preview_id"],
    true,
  );
  const statsResource = useResource<unknown>(
    "php_maintenance_session_stats",
    { id },
    active && !!statsCapability,
  );
  const stats = phpWorkerSessionStats(statsResource.data, id);
  const [log, setLog] = useState<ReturnType<typeof phpMaintenanceLog>>();
  const [preview, setPreview] =
    useState<ReturnType<typeof phpWorkerSessionPreview>>();
  const [task, setTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const refresh = () => {
    void statsResource.reload();
    onExecuted();
  };
  const readLog = async () => {
    setBusy(true);
    setError("");
    setLog(undefined);
    try {
      const value = await action<unknown>("php_maintenance_log_read", {
        id,
        stream: "fpm_error",
      });
      const parsed = phpMaintenanceLog(value, id);
      if (!parsed) throw new Error("PHP日志响应无效，请重新读取。");
      setLog(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const makePreview = async () => {
    if (!stats || !stats.allow_actions.includes("preview")) return;
    setBusy(true);
    setError("");
    setPreview(undefined);
    try {
      const value = await action<unknown>("php_maintenance_session_preview", {
        id,
        expected_revision: stats.revision,
        older_than_seconds: stats.older_than_seconds,
      }, { idempotencyKey: intentKey() });
      const parsed = phpWorkerSessionPreview(value, id, stats.revision);
      if (!parsed) throw new Error("会话清理预览无效，请重新读取状态后预览。");
      setPreview(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
      refresh();
    } finally {
      setBusy(false);
    }
  };
  const cleanup = async () => {
    if (!preview) return;
    let input: ReturnType<typeof phpWorkerCleanupInput>;
    try {
      input = phpWorkerCleanupInput(preview);
    } catch (e) {
      setError(apiErrorMessage(e));
      setPreview(undefined);
      refresh();
      return;
    }
    const confirm = window.confirm(
      `确认清理 ${preview.candidate_count} 个超过 ${ageLabel(preview.older_than_seconds)} 的PHP会话文件（约 ${bytes(preview.candidate_bytes)}）？任务只会删除预览中匹配的受管文件。`,
    );
    if (!confirm) return;

    setBusy(true);
    setError("");
    setPreview(undefined);
    setTask(undefined);
    try {
      const value = await action<unknown>(
        "php_maintenance_session_cleanup",
        input,
        { idempotencyKey: intentKey() },
      );
      const submitted = value as { id?: unknown; task_id?: unknown } | null;
      if (
        !submitted ||
        (typeof submitted.id !== "string" &&
          typeof submitted.task_id !== "string") ||
        (submitted.id === "" && submitted.task_id === "")
      )
        throw new Error("清理任务未返回任务编号；请检查任务状态，系统不会自动重试。");
      setTask(value);
      setTaskPending(true);
      refresh();
    } catch (e) {
      setError(`${apiErrorMessage(e)} 未自动重试；请重新读取状态并重新预览后再确认。`);
      refresh();
    } finally {
      setBusy(false);
    }
  };

  const previewExpired = preview ? phpWorkerPreviewExpired(preview) : false;
  const previewStale =
    !!preview &&
    (preview.id !== id ||
      !stats ||
      !!statsResource.error ||
      preview.revision !== stats.revision);
  const canPreview =
    !!previewCapability &&
    !!stats?.allow_actions.includes("preview") &&
    !busy &&
    !taskPending;
  const canCleanup =
    !!cleanupCapability &&
    !!preview?.allow_actions.includes("cleanup") &&
    !!stats &&
    !previewExpired &&
    !previewStale &&
    !busy &&
    !taskPending;

  return (
    <section className="stack" aria-label="PHP日志与会话维护">
      <h3>PHP错误日志</h3>
      {!logCapability ? (
        <Unavailable title="当前没有PHP错误日志读取权限" />
      ) : (
        <>
          <p className="muted">只读取受管 FPM error log，最多显示末尾 100 行。</p>
          <Button
            label={busy ? "正在读取日志…" : "读取PHP错误日志"}
            isDisabled={busy || taskPending}
            onClick={() => void readLog()}
          />
          {log && (
            log.generated && log.lines.length ? (
              <>
                {log.truncated && <p className="muted">日志内容已按行数或大小截断。</p>}
                <pre className="json-result">{log.lines.join("\n")}</pre>
              </>
            ) : (
              <p className="muted">当前尚无PHP错误日志内容。</p>
            )
          )}
        </>
      )}

      <div className="card-heading">
        <h3>PHP会话统计</h3>
        {statsCapability && (
          <Button
            label="刷新统计"
            variant="ghost"
            isDisabled={statsResource.loading || busy}
            onClick={() => void statsResource.reload()}
          />
        )}
      </div>
      {!statsCapability ? (
        <Unavailable title="当前没有PHP会话统计权限" />
      ) : statsResource.error ? (
        <div className="notice danger" role="alert">{statsResource.error}</div>
      ) : !stats ? (
        statsResource.data !== undefined ? (
          <Unavailable title="会话统计响应无效，请重新读取状态" />
        ) : (
          <p role="status">正在读取会话统计…</p>
        )
      ) : (
        <>
          <p>
            {stats.generated ? "受管会话目录" : "尚无受管会话目录"} · 共 {stats.total_sessions} 个 · 超过 {ageLabel(stats.older_than_seconds)} {stats.expired_sessions} 个
            {stats.oldest_age_seconds !== undefined
              ? ` · 最早约 ${ageLabel(stats.oldest_age_seconds)}`
              : ""}
          </p>
          {stats.allow_actions.includes("preview") && !previewCapability && (
            <Unavailable title="服务允许预览，但当前账户没有预览权限" />
          )}
          {stats.allow_actions.includes("preview") && previewCapability && (
            <Button
              label="预览过期会话清理"
              isDisabled={!canPreview}
              onClick={() => void makePreview()}
            />
          )}
          {stats.allow_actions.includes("preview") &&
            !cleanupCapability && (
              <p className="muted">当前账户没有会话清理任务权限。</p>
            )}
        </>
      )}

      {preview?.id === id && (
        <div className="stack notice warning" aria-label="会话清理预览">
          <strong>清理预览 · 版本 {preview.revision}</strong>
          <p>
            将清理 {preview.candidate_count} 个文件，约 {bytes(preview.candidate_bytes)}；筛选时间为超过 {ageLabel(preview.older_than_seconds)}。
          </p>
          <p className="muted">
            预览于 {fullDate(preview.captured_at)} 生成，有效至 {fullDate(preview.expires_at)}。只会处理预览中仍匹配的受管文件。
          </p>
          {previewExpired && <p role="alert">预览已过期，请重新读取并预览。</p>}
          {previewStale && <p role="alert">会话版本已变化，此预览已过期，请重新预览。</p>}
          {preview.candidate_count > 0 && !cleanupCapability && (
            <p className="muted">当前账户没有提交清理任务的权限。</p>
          )}
          {preview.allow_actions.includes("cleanup") && cleanupCapability && (
            <Button
              label={`确认清理 ${preview.candidate_count} 个会话文件`}
              isDisabled={!canCleanup}
              onClick={() => void cleanup()}
            />
          )}
          <Button
            label="取消预览"
            variant="ghost"
            isDisabled={busy}
            onClick={() => setPreview(undefined)}
          />
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
      {taskPending && <p role="status">清理任务正在运行；完成前不会重复提交。</p>}
    </section>
  );
}
