"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";
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
    ? translate("{0} 天", [seconds / 86400])
    : seconds % 3600 === 0
      ? translate("{0} 小时", [seconds / 3600])
      : translate("{0} 秒", [seconds]);

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
    true,
    "fpm_error",
  );
  const slowLogCapability = phpMaintenanceCapability(
    capabilities,
    "php_maintenance_log_read",
    "php:read",
    ["id", "stream"],
    false,
    true,
    "slow",
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
  const [logs, setLogs] = useState<
    Partial<Record<"fpm_error" | "slow", ReturnType<typeof phpMaintenanceLog>>>
  >({});
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
  const readLog = async (stream: "fpm_error" | "slow") => {
    const capability = stream === "slow" ? slowLogCapability : logCapability;
    if (!capability) return;
    setBusy(true);
    setError("");
    setLogs((current) => ({ ...current, [stream]: undefined }));
    try {
      const properties = capability.input_schema?.properties as
        | Record<string, unknown>
        | undefined;
      const value = await action<unknown>("php_maintenance_log_read", {
        id,
        stream,
        ...(properties &&
        Object.prototype.hasOwnProperty.call(properties, "limit")
          ? { limit: 100 }
          : {}),
      });
      const parsed = phpMaintenanceLog(value, id, stream);
      if (!parsed) throw new Error(translate("PHP日志响应无效，请重新读取。"));
      setLogs((current) => ({ ...current, [stream]: parsed }));
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
      const value = await action<unknown>(
        "php_maintenance_session_preview",
        {
          id,
          expected_revision: stats.revision,
          older_than_seconds: stats.older_than_seconds,
        },
        { idempotencyKey: intentKey() },
      );
      const parsed = phpWorkerSessionPreview(value, id, stats.revision);
      if (!parsed)
        throw new Error(translate("会话清理预览无效，请重新读取状态后预览。"));
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
      translate(
        "确认清理 {0} 个超过 {1} 的PHP会话文件（约 {2}）？任务只会删除预览中匹配的受管文件。",
        [
          preview.candidate_count,
          ageLabel(preview.older_than_seconds),
          bytes(preview.candidate_bytes),
        ],
      ),
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
        throw new Error(
          translate(
            "清理任务未返回任务编号；请检查任务状态，系统不会自动重试。",
          ),
        );
      setTask(value);
      setTaskPending(true);
      refresh();
    } catch (e) {
      setError(
        translate("{0} 未自动重试；请重新读取状态并重新预览后再确认。", [
          apiErrorMessage(e),
        ]),
      );
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
    <section className="stack" aria-label={translate("PHP日志与会话维护")}>
      <h3>{translate("PHP错误日志")}</h3>
      {!logCapability ? (
        <Unavailable title={translate("当前没有PHP错误日志读取权限")} />
      ) : (
        <>
          <p className="muted">
            {translate("只读取受管 FPM error log，最多显示末尾 100 行。")}
          </p>
          <Button
            label={
              busy ? translate("正在读取日志…") : translate("读取PHP错误日志")
            }
            isDisabled={busy || taskPending}
            onClick={() => void readLog("fpm_error")}
          />
          {logs.fpm_error &&
            (logs.fpm_error.generated && logs.fpm_error.lines.length ? (
              <>
                {logs.fpm_error.truncated && (
                  <p className="muted">
                    {translate("日志内容已按行数或大小截断。")}
                  </p>
                )}
                <pre className="json-result">
                  {logs.fpm_error.lines.join("\n")}
                </pre>
              </>
            ) : (
              <p className="muted">{translate("当前尚无PHP错误日志内容。")}</p>
            ))}
        </>
      )}
      <h3>{translate("FPM 慢日志")}</h3>
      {!slowLogCapability ? (
        <Unavailable
          title={translate("当前后端或账户未提供受管 FPM 慢日志读取能力")}
        />
      ) : (
        <>
          <p className="muted">
            {translate(
              "慢日志由受管 FPM 池设置控制，只读取末尾最多 100 行；文件路径不会传入请求或显示。",
            )}
          </p>
          <Button
            label={
              busy ? translate("正在读取慢日志…") : translate("读取FPM慢日志")
            }
            isDisabled={busy || taskPending}
            onClick={() => void readLog("slow")}
          />
          {logs.slow && !logs.slow.configured && (
            <p className="muted">
              {translate(
                "当前运行参数未启用慢日志。可在 PHP 运行参数配置中开启 slowlog_enabled 并提交受管应用变更。",
              )}
            </p>
          )}
          {logs.slow?.configured &&
            logs.slow.generated &&
            logs.slow.lines.length > 0 && (
              <>
                {logs.slow.truncated && (
                  <p className="muted">
                    {translate("慢日志内容已按行数或大小截断。")}
                  </p>
                )}
                <pre className="json-result">{logs.slow.lines.join("\n")}</pre>
              </>
            )}
          {logs.slow?.configured && !logs.slow.generated && (
            <p className="muted">
              {translate("慢日志已配置，当前还没有生成日志文件。")}
            </p>
          )}
          {logs.slow?.configured &&
            logs.slow.generated &&
            !logs.slow.lines.length && (
              <p className="muted">{translate("当前慢日志为空。")}</p>
            )}
        </>
      )}
      <SectionHeader icon="settings" className="card-heading">
        <h3>{translate("PHP会话统计")}</h3>
        {statsCapability && (
          <Button
            label={translate("刷新统计")}
            variant="ghost"
            isDisabled={statsResource.loading || busy}
            onClick={() => void statsResource.reload()}
          />
        )}
      </SectionHeader>
      {!statsCapability ? (
        <Unavailable title={translate("当前没有PHP会话统计权限")} />
      ) : statsResource.error ? (
        <div className="notice danger" role="alert">
          {statsResource.error}
        </div>
      ) : !stats ? (
        statsResource.data !== undefined ? (
          <Unavailable title={translate("会话统计响应无效，请重新读取状态")} />
        ) : (
          <p role="status">{translate("正在读取会话统计…")}</p>
        )
      ) : (
        <>
          <p>
            {stats.generated
              ? translate("受管会话目录")
              : translate("尚无受管会话目录")}
            {translate(" · 共 ")}
            {stats.total_sessions}
            {translate(" 个 · 超过 ")}
            {ageLabel(stats.older_than_seconds)}
            {stats.expired_sessions}
            {translate("个")}
            {stats.oldest_age_seconds !== undefined
              ? translate(" · 最早约 {0}", [ageLabel(stats.oldest_age_seconds)])
              : ""}
          </p>
          {stats.allow_actions.includes("preview") && !previewCapability && (
            <Unavailable
              title={translate("服务允许预览，但当前账户没有预览权限")}
            />
          )}
          {stats.allow_actions.includes("preview") && previewCapability && (
            <Button
              label={translate("预览过期会话清理")}
              isDisabled={!canPreview}
              onClick={() => void makePreview()}
            />
          )}
          {stats.allow_actions.includes("preview") && !cleanupCapability && (
            <p className="muted">
              {translate("当前账户没有会话清理任务权限。")}
            </p>
          )}
        </>
      )}
      {preview?.id === id && (
        <div
          className="stack notice warning"
          aria-label={translate("会话清理预览")}
        >
          <strong>
            {translate("清理预览 · 版本 ")}
            {preview.revision}
          </strong>
          <p>
            {translate("将清理")}
            {preview.candidate_count}
            {translate(" 个文件，约 ")}
            {bytes(preview.candidate_bytes)}
            {translate("；筛选时间为超过 ")}
            {ageLabel(preview.older_than_seconds)}。
          </p>
          <p className="muted">
            {translate("预览于")}
            {fullDate(preview.captured_at)}
            {translate(" 生成，有效至 ")}
            {fullDate(preview.expires_at)}
            {translate("。只会处理预览中仍匹配的受管文件。")}
          </p>
          {previewExpired && (
            <p role="alert">{translate("预览已过期，请重新读取并预览。")}</p>
          )}
          {previewStale && (
            <p role="alert">
              {translate("会话版本已变化，此预览已过期，请重新预览。")}
            </p>
          )}
          {preview.candidate_count > 0 && !cleanupCapability && (
            <p className="muted">
              {translate("当前账户没有提交清理任务的权限。")}
            </p>
          )}
          {preview.allow_actions.includes("cleanup") && cleanupCapability && (
            <Button
              label={translate("确认清理 {0} 个会话文件", [
                preview.candidate_count,
              ])}
              isDisabled={!canCleanup}
              onClick={() => void cleanup()}
            />
          )}
          <Button
            label={translate("取消预览")}
            variant="ghost"
            isDisabled={busy}
            onClick={() => setPreview(undefined)}
          />
        </div>
      )}
      {error && (
        <div className="notice danger" role="alert">
          {error}
        </div>
      )}
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
      {taskPending && (
        <p role="status">
          {translate("清理任务正在运行；完成前不会重复提交。")}
        </p>
      )}
    </section>
  );
}
