"use client";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { action, apiErrorMessage, Capability, intentKey } from "@/lib/api";
import {
  supervisorCapability,
  supervisorJobs,
  supervisorLogClearInput,
  supervisorLogClearPreview,
  supervisorLogRead,
} from "@/lib/supervisor-logs";
import { SoftwareInstance } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { Unavailable } from "./resource-ui";

const taskID = (value: unknown) => {
  if (!value || typeof value !== "object" || Array.isArray(value)) return "";
  const result = value as { id?: unknown; task_id?: unknown };
  return typeof result.id === "string"
    ? result.id
    : typeof result.task_id === "string"
      ? result.task_id
      : "";
};

export function SupervisorLogs({
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
  const jobsCapability = supervisorCapability(
    capabilities,
    "supervisor_list",
    "supervisor:read",
    ["id"],
  );
  const logCapability = supervisorCapability(
    capabilities,
    "supervisor_log_read",
    "supervisor:read",
    ["id", "name", "stream"],
  );
  const previewCapability = supervisorCapability(
    capabilities,
    "supervisor_log_clear_preview",
    "supervisor:read",
    ["id", "name"],
  );
  const clearCapability = supervisorCapability(
    capabilities,
    "supervisor_log_clear",
    "supervisor:manage",
    ["id", "name", "expected_revision", "expected_stdout", "expected_stderr"],
    true,
  );
  const jobsResource = useResource<unknown>(
    "supervisor_list",
    { id },
    active && !!jobsCapability,
  );
  const inventory = supervisorJobs(jobsResource.data);
  const [selected, setSelected] = useState("");
  const [stream, setStream] = useState<"stdout" | "stderr">("stdout");
  const [log, setLog] = useState<ReturnType<typeof supervisorLogRead>>();
  const [preview, setPreview] =
    useState<ReturnType<typeof supervisorLogClearPreview>>();
  const [task, setTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const selectedJob = inventory?.items.find((job) => job.name === selected);

  useEffect(() => {
    if (
      selected &&
      inventory &&
      !inventory.items.some((job) => job.name === selected)
    ) {
      setSelected("");
      setLog(undefined);
      setPreview(undefined);
    }
  }, [selected, inventory]);

  const refresh = () => {
    void jobsResource.reload();
    onExecuted();
  };
  const selectJob = (name: string) => {
    setSelected(name);
    setStream("stdout");
    setLog(undefined);
    setPreview(undefined);
    setTask(undefined);
    setError("");
  };
  const readLog = async () => {
    if (!selectedJob) return;
    setBusy(true);
    setError("");
    setLog(undefined);
    try {
      const value = await action<unknown>("supervisor_log_read", {
        id,
        name: selectedJob.name,
        stream,
      });
      const parsed = supervisorLogRead(value, id, selectedJob.name, stream);
      if (!parsed)
        throw new Error(translate("守护程序日志响应无效，请重新读取。"));
      setLog(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const makePreview = async () => {
    if (!selectedJob) return;
    setBusy(true);
    setError("");
    setPreview(undefined);
    try {
      const value = await action<unknown>("supervisor_log_clear_preview", {
        id,
        name: selectedJob.name,
      });
      const parsed = supervisorLogClearPreview(value, id, selectedJob.name);
      if (!parsed)
        throw new Error(
          translate("清理预览未同时包含标准输出和标准错误，请重新读取。"),
        );
      setPreview(parsed);
    } catch (e) {
      setError(apiErrorMessage(e));
      refresh();
    } finally {
      setBusy(false);
    }
  };
  const clearLogs = async () => {
    if (
      !preview ||
      !selectedJob ||
      preview.id !== id ||
      preview.name !== selectedJob.name
    )
      return;
    const stale = inventory && inventory.revision !== preview.revision;
    if (stale) {
      setError(
        translate("守护程序配置版本已变化，请重新读取任务并预览两路日志。"),
      );
      setPreview(undefined);
      refresh();
      return;
    }
    const stdoutSize = preview.stdout.exists
      ? preview.stdout.size
      : translate("无文件");
    const stderrSize = preview.stderr.exists
      ? preview.stderr.size
      : translate("无文件");
    const confirmed = window.confirm(
      translate(
        "确认清空已注册任务“{0}”的 stdout 和 stderr 两路日志？预览状态：stdout {1} 字节，stderr {2} 字节。",
        [selectedJob.name, stdoutSize, stderrSize],
      ),
    );
    if (!confirmed) return;

    setBusy(true);
    setError("");
    setPreview(undefined);
    setLog(undefined);
    setTask(undefined);
    try {
      const value = await action<unknown>(
        "supervisor_log_clear",
        supervisorLogClearInput(preview),
        { idempotencyKey: intentKey() },
      );
      if (!taskID(value))
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
        translate("{0} 未自动重试；请重新读取任务并预览两路日志后再确认。", [
          apiErrorMessage(e),
        ]),
      );
      refresh();
    } finally {
      setBusy(false);
    }
  };

  const previewStale =
    !!preview &&
    (preview.id !== id ||
      !inventory ||
      !!jobsResource.error ||
      inventory.revision !== preview.revision);
  const canReadLog = !!selectedJob && !!logCapability && !busy && !taskPending;
  const canPreview =
    !!selectedJob && !!previewCapability && !busy && !taskPending;
  const canClear =
    !!selectedJob &&
    !!clearCapability &&
    !!preview &&
    preview.id === id &&
    !previewStale &&
    !busy &&
    !taskPending;

  return (
    <section className="stack" aria-label={translate("守护程序日志")}>
      <div className="card-heading">
        <h3>{translate("已注册任务日志")}</h3>
        {jobsCapability && (
          <Button
            label={translate("刷新任务列表")}
            variant="ghost"
            isDisabled={jobsResource.loading || busy}
            onClick={() => void jobsResource.reload()}
          />
        )}
      </div>
      {jobsResource.error && (
        <div className="notice danger" role="alert">
          {jobsResource.error}
        </div>
      )}
      {!jobsCapability ? (
        <Unavailable title={translate("当前没有守护程序任务读取权限")} />
      ) : jobsResource.data !== undefined && !inventory ? (
        <Unavailable title={translate("任务列表响应无效，请重新读取")} />
      ) : !inventory ? (
        <p role="status">{translate("正在读取已注册任务…")}</p>
      ) : !inventory.items.length ? (
        <Unavailable title={translate("暂无已注册守护程序任务")} />
      ) : (
        <div className="instance-list">
          {inventory.items.map((job) => (
            <button
              key={job.name}
              className="instance-row"
              disabled={busy || taskPending}
              aria-pressed={selected === job.name}
              onClick={() => selectJob(job.name)}
            >
              <div>
                <strong>{job.program_id || job.name}</strong>
                <p>
                  {job.name}
                  {job.state ? ` · ${job.state}` : ""}
                </p>
              </div>
            </button>
          ))}
        </div>
      )}
      {selectedJob && (
        <div className="stack">
          <div className="card-heading">
            <h3>{selectedJob.name}</h3>
            {selectedJob.state && (
              <span className="muted">{selectedJob.state}</span>
            )}
          </div>
          {!logCapability ? (
            <Unavailable title={translate("当前没有任务日志读取权限")} />
          ) : (
            <>
              <p className="muted">
                {translate("每次读取最多返回末尾 64 KiB，不接受日志路径输入。")}
              </p>
              <div
                className="section-tabs"
                role="tablist"
                aria-label={translate("守护程序日志流")}
              >
                {(["stdout", "stderr"] as const).map((item) => (
                  <button
                    key={item}
                    role="tab"
                    aria-selected={stream === item}
                    onClick={() => {
                      setStream(item);
                      setLog(undefined);
                      setError("");
                    }}
                  >
                    {item}
                  </button>
                ))}
              </div>
              <Button
                label={translate("读取 {0} 末尾日志", [stream])}
                isDisabled={!canReadLog}
                onClick={() => void readLog()}
              />
              {log &&
                log.id === id &&
                log.name === selectedJob.name &&
                log.stream === stream &&
                (log.file_identity.exists && log.text ? (
                  <>
                    <p className="muted">
                      {translate("读取 ")}
                      {log.bytes}
                      {translate(" 字节")}
                      {log.truncated ? translate(" · 内容已截断") : ""}
                    </p>
                    <pre className="json-result">{log.text}</pre>
                  </>
                ) : (
                  <p className="muted">{translate("此日志流当前没有内容。")}</p>
                ))}
            </>
          )}
          <h3>{translate("清空任务日志")}</h3>
          <p className="muted">
            {translate(
              "预览和确认操作都会同时覆盖该任务的 stdout 与 stderr 两路日志。",
            )}
          </p>
          {!previewCapability ? (
            <Unavailable title={translate("当前没有双流清理预览权限")} />
          ) : (
            <Button
              label={translate("预览清空两路日志")}
              isDisabled={!canPreview}
              onClick={() => void makePreview()}
            />
          )}
          {preview &&
            preview.id === id &&
            preview.name === selectedJob.name && (
              <div
                className="stack notice warning"
                aria-label={translate("双流日志清理预览")}
              >
                <strong>{translate("清空范围：stdout 和 stderr")}</strong>
                <p>
                  stdout：
                  {preview.stdout.exists
                    ? translate("{0} 字节", [preview.stdout.size])
                    : translate("无文件")}
                  · stderr：
                  {preview.stderr.exists
                    ? translate("{0} 字节", [preview.stderr.size])
                    : translate("无文件")}
                </p>
                <p className="muted">
                  {translate("预览版本 ")}
                  {preview.revision}
                  {translate("。提交会要求两路文件身份和版本都与预览相符。")}
                </p>
                {previewStale && (
                  <p role="alert">
                    {translate("任务配置版本已变化，请重新读取并预览。")}
                  </p>
                )}
                <p className="muted">
                  {translate("任务进程运行时可能继续产生新日志。")}
                </p>
                {!clearCapability && (
                  <p className="muted">
                    {translate("当前账户没有清空任务权限。")}
                  </p>
                )}
                {clearCapability && (
                  <Button
                    label={translate("确认清空 stdout 和 stderr")}
                    isDisabled={!canClear}
                    onClick={() => void clearLogs()}
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
          {translate("双流清理任务正在运行；完成前不会重复提交。")}
        </p>
      )}
    </section>
  );
}
