"use client";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { action, apiErrorMessage, Capability, intentKey } from "@/lib/api";
import {
  phpExtensionActions,
  phpExtensionApplyInput,
  PHPExtensionOperation,
  PHPExtensionPlan,
  parsePHPExtensionPlan,
} from "@/lib/php-extension-packages";
import { TaskResult } from "./task-result";
import { Unavailable } from "./resource-ui";

const finalTaskStates = new Set(["succeeded", "failed", "canceled", "cancelled", "interrupted", "completed"]);
const isTaskPending = (value: unknown) => {
  if (!value || typeof value !== "object" || Array.isArray(value)) return true;
  const task = value as Record<string, unknown>;
  return !finalTaskStates.has(String(task.status ?? task.state ?? ""));
};

export function PHPExtensionPackages({
  id,
  currentRevision,
  capabilities,
  onExecuted,
  active = true,
}: {
  id: string;
  currentRevision?: number;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const actions = phpExtensionActions(capabilities);
  const [operation, setOperation] = useState<PHPExtensionOperation>("install");
  const [moduleID, setModuleID] = useState<string>(actions.modules[0] ?? "");
  const [plan, setPlan] = useState<PHPExtensionPlan>();
  const [busy, setBusy] = useState(false);
  const [taskActive, setTaskActive] = useState(false);
  const [error, setError] = useState("");
  const [applyKey, setApplyKey] = useState("");
  const [task, setTask] = useState<unknown>();

  useEffect(() => {
    if (!actions.modules.includes(moduleID as (typeof actions.modules)[number]))
      setModuleID(actions.modules[0] ?? "");
  }, [actions.modules, moduleID]);

  if (!capabilities.some((capability) => capability.name === "php_extension_plan"))
    return null;
  const schemaMismatch = !actions.plan || !actions.apply || !actions.modules.length;
  const revision = Number.isSafeInteger(currentRevision) && (currentRevision ?? 0) > 0
    ? currentRevision
    : undefined;
  const applyReady = !!actions.apply && !!plan && !!revision && !!phpExtensionApplyInput(plan, id, revision, Date.now(), applyKey || "retrykey00000000");
  const refreshPlan = async () => {
    if (!active || busy || taskActive || !actions.plan || !revision || !actions.modules.some((module) => module === moduleID)) return;
    setBusy(true);
    setError("");
    setTask(undefined);
    setPlan(undefined);
    setApplyKey("");
    try {
      const response = await action<unknown>(actions.plan.name, {
        id,
        operation,
        module_id: moduleID,
        expected_revision: revision,
      });
      const next = parsePHPExtensionPlan(response, {
        id,
        operation,
        module_id: moduleID,
        expected_revision: revision,
      });
      if (!next) throw new Error("扩展计划响应与当前 PHP 实例或版本不匹配，未启用安装操作。");
      setPlan(next);
    } catch (cause) {
      setError(apiErrorMessage(cause));
      onExecuted();
    } finally {
      setBusy(false);
    }
  };
  const applyPlan = async () => {
    if (!actions.apply || !plan || !revision || busy || taskActive) return;
    const key = applyKey || intentKey();
    const input = phpExtensionApplyInput(plan, id, revision, Date.now(), key);
    if (!input) {
      setError("计划已过期或 PHP 版本已变化。请重新读取并生成计划。");
      setPlan(undefined);
      onExecuted();
      return;
    }
    if (!window.confirm(
      plan.operation === "install"
        ? `确认安装 ${plan.module_id}？计划包含 ${plan.package_pins.length} 个精确软件包版本和 ${plan.affected_profiles.length} 个受影响 PHP 配置。`
        : `确认在当前 PHP 实例停用 ${plan.module_id}？此操作会保留主机软件包，不会从系统卸载它。`,
    )) return;
    setApplyKey(key);
    setBusy(true);
    setError("");
    try {
      const submitted = await action<unknown>(actions.apply.name, input, {
        idempotencyKey: key,
      });
      setTask(submitted);
      setTaskActive(isTaskPending(submitted));
      setPlan(undefined);
      setApplyKey("");
      onExecuted();
    } catch (cause) {
      // Keep this exact key for a retry if the request was accepted but its
      // response was lost. The task action and HTTP header share the key.
      setApplyKey(key);
      setError(apiErrorMessage(cause));
      onExecuted();
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="panel-card">
      <div className="card-heading">
        <div>
          <h4>受信任 PHP 扩展包</h4>
          <p className="muted">先读取精确发行版软件包计划；仅支持后端列出的 ABI 与签名仓库候选。</p>
        </div>
      </div>
      {error && <div className="notice danger" role="alert">{error}</div>}
      {schemaMismatch ? (
        <Unavailable title="扩展包计划能力或模块清单尚不可用" detail="未推测运行时支持范围；请等待管理服务公布真实计划能力。" />
      ) : !revision ? (
        <Unavailable title="没有当前 PHP 版本号" detail="先读取该实例的扩展状态和配置修订，再生成包计划。" />
      ) : (
        <>
          <div className="button-row">
            <label className="native-field">
              操作
              <select disabled={busy || taskActive} value={operation} onChange={(event) => { setOperation(event.target.value as PHPExtensionOperation); setPlan(undefined); setApplyKey(""); }}>
                <option value="install">安装</option>
                <option value="uninstall">停用此实例扩展（保留软件包）</option>
              </select>
            </label>
            <label className="native-field">
              扩展模块
              <select disabled={busy || taskActive} value={moduleID} onChange={(event) => { setModuleID(event.target.value); setPlan(undefined); setApplyKey(""); }}>
                {actions.modules.map((module) => <option value={module} key={module}>{module}</option>)}
              </select>
            </label>
            <Button
              label="生成软件包计划"
              isDisabled={busy || taskActive || !active || !moduleID}
              onClick={() => void refreshPlan()}
            />
          </div>
          {plan && (
            <div className="stack" aria-label="受信任 PHP 扩展包计划">
              <div className="detail-grid">
                <div><small>运行时</small><strong>PHP {plan.php_version} · ABI {plan.php_module_api}</strong></div>
                <div><small>平台</small><strong>{plan.platform.distro} {plan.platform.release} · {plan.platform.os}/{plan.platform.arch}</strong></div>
                <div><small>计划有效期至</small><strong>{new Date(plan.expires_at).toLocaleString()}</strong></div>
              </div>
              {plan.operation === "install" && (plan.already_installed || plan.already_enabled) && (
                <div className="notice">{plan.already_installed ? "软件包已安装；计划会按下方固定版本处理。" : "软件包尚未确认安装。"} {plan.already_enabled ? "该扩展已在此 PHP 实例启用。" : "该扩展当前未启用。"}</div>
              )}
              {plan.operation === "uninstall" && (
                <div className="notice warning">此操作只停用当前 PHP 实例中的扩展配置；系统软件包会保留，不会从主机移除。{plan.package_retained_reason}</div>
              )}
              <div>
                <strong>精确软件包候选</strong>
                {plan.package_pins.length ? (
                  <div className="instance-list">
                    {plan.package_pins.map((pin) => (
                      <div className="instance-row" key={`${pin.name}:${pin.version}:${pin.architecture}`}>
                        <div>
                          <strong>{pin.name} · {pin.version} · {pin.architecture}</strong>
                          <p>{pin.repository}</p>
                          <small className="monospace">SHA-256 {pin.sha256}</small>
                        </div>
                      </div>
                    ))}
                  </div>
                ) : <p className="muted">计划没有新增或移除软件包。</p>}
              </div>
              {plan.affected_profiles.length > 0 && (
                <div>
                  <strong>受影响配置</strong>
                  <ul>
                    {plan.affected_profiles.map((profile) => (
                      <li key={profile.id}>{profile.id} · PHP {profile.php_version} · 扩展{profile.module_enabled ? "已启用" : "未启用"} · 包引用{profile.package_reference ? "存在" : "无"}</li>
                    ))}
                  </ul>
                </div>
              )}
              {plan.changes.length > 0 && (
                <div>
                  <strong>计划变更</strong>
                  <ul>{plan.changes.map((change, index) => <li key={`${change.kind}:${change.target}:${index}`}>{change.description}</li>)}</ul>
                </div>
              )}
              <p className="muted">计划引用 {plan.resources.join(" · ") || "当前实例资源"}；卸载不会移除依赖包。</p>
              <Button
                label={applyKey ? "使用同一请求编号重试" : plan.operation === "uninstall" ? "确认停用当前实例扩展" : "确认执行安装计划"}
                isDisabled={busy || taskActive || !active || !applyReady || plan.changes.length === 0}
                onClick={() => void applyPlan()}
              />
            </div>
          )}
        </>
      )}
      {task !== undefined && <TaskResult key={JSON.stringify(task)} submitted={task} capabilities={capabilities} onFinished={() => { setTaskActive(false); onExecuted(); }} />}
    </Card>
  );
}
