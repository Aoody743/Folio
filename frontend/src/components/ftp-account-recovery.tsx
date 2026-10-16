"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { action, apiErrorMessage, Capability } from "@/lib/api";
import {
  ftpAccountStateCapability,
  ftpLegacyRecoveryInput,
  parseFTPAccountInspection,
} from "@/lib/ftp-account-state";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { State, Unavailable } from "./resource-ui";

const finalTaskStates = new Set(["succeeded", "failed", "canceled", "cancelled", "interrupted", "completed"]);
const isTaskPending = (value: unknown) => {
  if (!value || typeof value !== "object" || Array.isArray(value)) return true;
  const task = value as Record<string, unknown>;
  return !finalTaskStates.has(String(task.status ?? task.state ?? ""));
};

export function FTPAccountRecovery({
  id,
  name,
  capabilities,
  onExecuted,
  active = true,
}: {
  id: string;
  name: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const inspectCapability = ftpAccountStateCapability(capabilities, "ftp_account_state_inspect");
  const recoverCapability = ftpAccountStateCapability(capabilities, "ftp_account_state_legacy_recover");
  const inspectionResource = useResource<unknown>(
    "ftp_account_state_inspect",
    { id, name },
    active && !!inspectCapability,
    15000,
  );
  const inspection = parseFTPAccountInspection(inspectionResource.data, id, name);
  const [busy, setBusy] = useState(false);
  const [taskActive, setTaskActive] = useState(false);
  const [error, setError] = useState("");
  const [task, setTask] = useState<unknown>();
  const input = ftpLegacyRecoveryInput(inspection, id, name);
  const refresh = () => {
    void inspectionResource.reload();
    onExecuted();
  };
  const recover = async () => {
    if (!input || !recoverCapability || busy || taskActive) return;
    const pending = inspection?.phase === "legacy_recovery_pending";
    if (!window.confirm(
      pending
        ? `确认使用同一账号修复记录，继续恢复 ${name}？当前旧版数据不能证明此前暂停已完全生效。恢复后，若仍需要 IP 访问限制，请重新配置；旧版操作覆盖的自定义拒绝规则无法自动还原。`
        : `确认恢复 ${name}？当前旧版数据不能证明此前暂停已完全生效。恢复会清除可精确识别的旧版生成错误记录并重新生成账号数据库。旧版操作覆盖的自定义 IP 拒绝规则无法自动还原；如仍需要访问限制，请在恢复后重新配置。`,
    )) return;
    setBusy(true);
    setError("");
    setTask(undefined);
    try {
      const accepted = await action<unknown>(recoverCapability.name, input);
      setTask(accepted);
      setTaskActive(isTaskPending(accepted));
      refresh();
    } catch (cause) {
      setError(apiErrorMessage(cause));
      refresh();
    } finally {
      setBusy(false);
    }
  };

  if (!inspectCapability) return null;
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <div>
          <h4>FTP 账号状态恢复</h4>
          <p className="muted">按账号检查受保护的暂停记录与允许的恢复操作。</p>
        </div>
        <Button label="重新检查" variant="ghost" isDisabled={inspectionResource.loading || busy} onClick={() => void inspectionResource.reload()} />
      </div>
      {(error || inspectionResource.error) && <div className="notice danger" role="alert">{error || inspectionResource.error}</div>}
      {inspectionResource.loading && !inspection ? (
        <p role="status">正在检查账号状态…</p>
      ) : inspection ? (
        <>
          <div className="detail-grid">
            <div><small>账号</small><strong>{inspection.name}</strong></div>
            <div><small>状态</small><State value={inspection.phase} /></div>
            <div><small>修订</small><strong>{inspection.revision}</strong></div>
          </div>
          {inspection.phase === "legacy_recovery_required" && input ? (
            <div className="notice warning">
              这是旧版恢复状态；当前记录不能证明此前暂停已完全禁止登录。恢复只会清除可精确识别的旧版生成错误记录并重建 Pure-FTP 账号数据库，密码校验值和其他字段保持原样。旧版暂停可能已覆盖原有自定义 IP 拒绝规则，系统无法还原该规则；如仍需要访问限制，请在恢复后重新配置。
              {recoverCapability ? (
                <Button label="确认恢复旧版 FTP 账号" isDisabled={busy || taskActive} onClick={() => void recover()} />
              ) : <p>当前没有恢复权限。</p>}
            </div>
          ) : inspection.phase === "legacy_recovery_pending" && input ? (
            <div className="notice warning">
              上次恢复留下了受保护的事务记录。重试会使用同一账号修订继续该操作，不会另行修改其他账号字段。旧版操作覆盖的自定义 IP 拒绝规则无法自动还原；如仍需要访问限制，请在恢复后重新配置。
              {recoverCapability ? (
                <Button label="继续已记录的恢复" isDisabled={busy || taskActive} onClick={() => void recover()} />
              ) : <p>当前没有恢复权限。</p>}
            </div>
          ) : inspection.phase === "legacy_unrecoverable" ? (
            <Unavailable title="此旧版状态无法安全自动恢复" detail="账号包含未知或自定义的旧访问规则；本面板不会改写它们。" />
          ) : inspection.recovery_required ? (
            <div className="notice warning">当前恢复状态：{inspection.phase}。只提供后端检查返回的恢复命令。</div>
          ) : (
            <p className="muted">账号不处于旧版暂停恢复状态。此卡片不会提交其他 FTP 账号操作。</p>
          )}
        </>
      ) : (
        <Unavailable title="账号状态响应无效" detail="未启用恢复操作；请刷新服务状态或检查读取权限。" />
      )}
      {task !== undefined && <TaskResult key={JSON.stringify(task)} submitted={task} capabilities={capabilities} onFinished={() => { setTaskActive(false); refresh(); }} />}
    </Card>
  );
}
