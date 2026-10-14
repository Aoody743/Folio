"use client";
import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import {
  action,
  Capability,
  intentKey,
} from "@/lib/api";
import {
  hasSoftwareRecoveryActionSchema,
  hasSoftwareRecoveryInspectSchema,
  isSoftwareInstanceID,
  parseSoftwareRecoveryInspection,
  softwareRecoveryActionInput,
  SoftwareRecoveryCommand,
  SoftwareRecoveryInspection,
} from "@/lib/software-recovery";
import { TaskResult } from "./task-result";

const commandLabels: Record<SoftwareRecoveryCommand, string> = {
  continue: "继续完成已验证的安装",
  stop_preserve: "停止服务并保留数据",
};

const stateLabels: Record<string, string> = {
  journal_missing: "没有恢复记录",
  receipt_present: "安装收据已存在",
  in_progress: "安装恢复中",
  inspection_only: "仅可检查",
  journal_conflict: "归属状态冲突",
  preserved_stopped: "已停止并保留",
  completed: "恢复完成",
};

const reasonLabels: Record<string, string> = {
  "This checkpoint has no verified running-service proof. Automatic recovery is unavailable; preserve existing state for operator review.":
    "安装在服务归属验证前中断，当前仅可检查。请保留现场，由管理员核对后处理；无法自动恢复。",
  "No protected install journal exists; automatic recovery is unavailable.":
    "没有受保护的安装记录，因此当前没有可执行的恢复操作。",
  "A protected software receipt exists; no incomplete-install recovery action is available.":
    "已有受保护的软件收据；当前没有未完成安装恢复操作。",
  "The protected install journal proves this attempt; recovery is limited to its verified checkpoint.":
    "受保护的日志证明这是本次安装；可用操作仅限已验证的检查点。",
  "The protected install journal and current receipt do not prove a safe recovery state.":
    "受保护的日志与当前收据无法证明安全恢复状态。",
  "The journal exists, but current ownership proofs do not permit a recovery action.":
    "日志存在，但当前归属证明不允许执行恢复操作。",
  "The install receipt is committed; continue only to finalize its journal checkpoint.":
    "安装收据已经写入；继续操作只会完成日志检查点。",
  "The verified owned service is stopped and disabled; its package, data, configuration, and runtime were preserved.":
    "已验证的服务已停止并禁用；软件包、数据、配置和运行时均已保留。",
  "The install receipt is committed.": "安装收据已经写入。",
  "The completed journal has no current software receipt; automatic recovery is denied.":
    "恢复日志已完成，但当前没有软件收据；不能自动恢复。",
};

function RecoveryState({ state }: { state: string }) {
  const tone =
    state === "journal_conflict"
      ? "danger"
      : state === "in_progress"
        ? "warning"
        : "neutral";
  return (
    <span className={`status-chip ${tone}`}>
      {stateLabels[state] ?? state}
    </span>
  );
}

function message(error: unknown) {
  return error instanceof Error ? error.message : "软件恢复请求失败。";
}

export function SoftwareRecovery({
  capabilities,
  seed,
}: {
  capabilities: Capability[];
  seed?: { id: string; source: string };
}) {
  const inspectCapability = capabilities.find(
    (item) => item.name === "software_recovery_inspect",
  );
  const recoveryCapability = capabilities.find(
    (item) => item.name === "software_recovery",
  );
  const canInspect = hasSoftwareRecoveryInspectSchema(inspectCapability);
  const canRecover = hasSoftwareRecoveryActionSchema(recoveryCapability);
  const [id, setID] = useState(seed?.id ?? "");
  const [seedSource, setSeedSource] = useState(seed?.source ?? "");
  const [inspection, setInspection] =
    useState<SoftwareRecoveryInspection>();
  const [inspectionFresh, setInspectionFresh] = useState(false);
  const [inspectLoading, setInspectLoading] = useState(false);
  const [inspectError, setInspectError] = useState("");
  const [actionError, setActionError] = useState("");
  const [actionLoading, setActionLoading] =
    useState<SoftwareRecoveryCommand>();
  const [submittedTask, setSubmittedTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const requestRevision = useRef(0);
  const appliedSeed = useRef<typeof seed>(undefined);
  const recoveryIntent = useRef<{ input: string; key: string } | undefined>(undefined);

  useEffect(() => {
    if (!seed || appliedSeed.current === seed || actionLoading || taskPending) return;
    appliedSeed.current = seed;
    requestRevision.current++;
    setID(seed.id);
    setSeedSource(seed.source);
    setInspectLoading(false);
    setInspectionFresh(false);
    setInspectError("");
    setActionError("");
    setSubmittedTask(undefined);
  }, [seed, actionLoading, taskPending]);

  const changeID = (value: string) => {
    if (actionLoading || taskPending) return;
    requestRevision.current++;
    setID(value);
    setSeedSource("");
    setInspectLoading(false);
    setInspectionFresh(false);
    setInspectError("");
    setActionError("");
    setSubmittedTask(undefined);
  };

  const inspect = async () => {
    const selectedID = id.trim();
    if (!canInspect) {
      setInspectError("当前身份没有符合要求的软件恢复检查能力。");
      return;
    }
    if (!isSoftwareInstanceID(selectedID)) {
      setInspectError("请输入有效的软件实例 ID（小写字母、数字、下划线或连字符）。");
      return;
    }
    const requestID = ++requestRevision.current;
    setInspectLoading(true);
    setInspectionFresh(false);
    setInspectError("");
    try {
      const result = await action<unknown>("software_recovery_inspect", {
        id: selectedID,
      });
      if (requestID !== requestRevision.current) return;
      const parsed = parseSoftwareRecoveryInspection(result, selectedID);
      if (!parsed) throw new Error("恢复状态响应不完整或与请求 ID 不匹配，请重新检查。");
      setID(selectedID);
      setInspection(parsed);
      setInspectionFresh(true);
    } catch (error) {
      if (requestID === requestRevision.current)
        setInspectError(message(error));
    } finally {
      if (requestID === requestRevision.current) setInspectLoading(false);
    }
  };

  const recover = async (command: SoftwareRecoveryCommand) => {
    const selectedID = id.trim();
    const input = softwareRecoveryActionInput(
      recoveryCapability,
      inspection,
      selectedID,
      command,
      inspectionFresh,
    );
    if (!input || taskPending || actionLoading) return;

    // Force a fresh, explicit inspection before any retry, including after a
    // transport error where the server may already have queued the task.
    setInspectionFresh(false);
    setActionError("");
    setActionLoading(command);
    const encodedInput = JSON.stringify(input);
    if (recoveryIntent.current?.input !== encodedInput)
      recoveryIntent.current = { input: encodedInput, key: intentKey() };
    try {
      const submitted = await action<unknown>("software_recovery", input, {
        idempotencyKey: recoveryIntent.current.key,
      });
      setSubmittedTask(submitted);
      setTaskPending(true);
    } catch (error) {
      setActionError(message(error));
    } finally {
      setActionLoading(undefined);
    }
  };

  return (
    <div id="software-install-recovery">
      <Card className="panel-card">
        <div className="card-heading">
          <div>
            <h2>安装恢复检查</h2>
            <p className="muted">
              输入或确认失败安装的实例 ID，再检查受保护的恢复记录。
            </p>
          </div>
        </div>
        {!canInspect ? (
          <div className="notice warning">
            当前身份没有可用的软件恢复检查权限或接口格式不兼容。
          </div>
        ) : (
          <>
          <div className="button-row">
            <TextInput
              label="软件实例 ID"
              value={id}
              onChange={changeID}
              isDisabled={!!actionLoading || taskPending}
              autoComplete="off"
              placeholder="例如 mariadb-default"
            />
            <Button
              label={inspection ? "重新检查恢复状态" : "检查恢复状态"}
              variant="primary"
              isLoading={inspectLoading}
              isDisabled={inspectLoading || !id.trim() || !!actionLoading}
              onClick={() => void inspect()}
            />
          </div>
          {seedSource && (
            <p className="muted" role="status">
              实例 ID 来自：{seedSource}。确认后点击检查。
            </p>
          )}
          {inspection && !inspectionFresh && (
            <div className="notice warning" role="status">
              当前显示的是旧检查结果；执行恢复前请重新检查。
            </div>
          )}
          {inspectError && (
            <div className="notice danger" role="alert">
              {inspectError}
            </div>
          )}
          {inspection && (
            <section className="plan-review" aria-label="安装恢复状态">
              <div className="resource-summary">
                <code>{inspection.id}</code>
                <RecoveryState state={inspection.state} />
              </div>
              <div className="version-list">
                <div className="version-row">
                  <strong>检查点</strong>
                  <code>{inspection.checkpoint ?? "无受保护检查点"}</code>
                </div>
                <div className="version-row">
                  <strong>日志版本</strong>
                  <span>{inspection.revision ?? "—"}</span>
                </div>
                <div className="version-row">
                  <strong>安装收据</strong>
                  <span>{inspection.receipt_present ? "存在" : "不存在"}</span>
                </div>
              </div>
              {inspection.reason && (
                <p className="muted">
                  {reasonLabels[inspection.reason] ?? inspection.reason}
                </p>
              )}
              {inspection.available_actions.length > 0 && (
                <div className="button-row">
                  {inspection.available_actions.map((command) => (
                    <Button
                      key={command}
                      label={commandLabels[command]}
                      variant={command === "continue" ? "primary" : "ghost"}
                      isLoading={actionLoading === command}
                      isDisabled={
                        !canRecover ||
                        !inspectionFresh ||
                        inspectLoading ||
                        taskPending ||
                        !!actionLoading ||
                        id.trim() !== inspection.id
                      }
                      onClick={() => void recover(command)}
                    />
                  ))}
                </div>
              )}
              {inspection.available_actions.length > 0 && !canRecover && (
                <p className="muted" role="status">
                  当前身份没有对应的恢复任务权限，或服务端能力格式不兼容。
                </p>
              )}
              {taskPending && (
                <p className="muted" role="status">
                  恢复任务仍在执行；任务结束后请重新检查状态。
                </p>
              )}
            </section>
          )}
          {actionError && (
            <div className="notice danger" role="alert">
              {actionError}
            </div>
          )}
          {submittedTask !== undefined && (
            <TaskResult
              submitted={submittedTask}
              capabilities={capabilities}
              onFinished={() => setTaskPending(false)}
            />
          )}
          </>
        )}
      </Card>
    </div>
  );
}

