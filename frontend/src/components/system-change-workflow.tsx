"use client";
import { t as translate } from "@/lib/i18n";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import {
  action,
  APIError,
  Capability,
  fullDate,
  intentKey,
  Task,
} from "@/lib/api";
import {
  canUseSystemChangeStorage,
  hasRequiredIDSchema,
  hasSystemChangeGetSchema,
  hasSystemChangeIDSchema,
  hasSystemChangePlanSchema,
  parseSystemChangePlan,
  readSystemChangeRecovery,
  SystemChangeAction,
  SystemChangePlan,
  systemChangeActionAllowed,
  systemChangeIDInput,
  systemChangeIntentResolved,
  systemChangeRecoveryKey,
  systemChangeTaskFinished,
  systemChangeTaskID,
  writeSystemChangeRecovery,
  SystemChangeRecovery,
} from "@/lib/system-change-workflow";
import { choiceLabel, fieldLabel } from "@/lib/labels";
import { ActionForm } from "./actions";
import { State, Unavailable } from "./resource-ui";

type Confirmation = {
  action: SystemChangeAction;
  key: string;
  retry: boolean;
};

const actionTitle: Record<SystemChangeAction, string> = {
  get system_change_apply() {
    return translate("应用计划");
  },
  get system_change_confirm() {
    return translate("确认配置正常");
  },
  get system_change_rollback() {
    return translate("回滚变更");
  },
};

const planStateLabels: Record<string, string> = {
  get planned() {
    return translate("待应用");
  },
  get applying() {
    return translate("应用中");
  },
  get awaiting_confirmation() {
    return translate("等待连接确认");
  },
  get applied() {
    return translate("已应用");
  },
  get rolled_back() {
    return translate("已回滚");
  },
  get recovery_required() {
    return translate("需要恢复处置");
  },
};

function PlanState({ state }: { state: string }) {
  const tone =
    state === "recovery_required"
      ? "danger"
      : state === "planned" ||
          state === "applying" ||
          state === "awaiting_confirmation"
        ? "warning"
        : "neutral";
  return (
    <span className={`status-chip ${tone}`}>
      {planStateLabels[state] ?? state}
    </span>
  );
}

function displayValue(key: string, value: unknown): string {
  if (typeof value === "boolean")
    return value ? translate("是") : translate("否");
  if (Array.isArray(value)) return JSON.stringify(value);
  if (value && typeof value === "object") return JSON.stringify(value);
  if (key === "kind" || key === "service_state" || key === "power")
    return choiceLabel(value, key);
  return String(value ?? "—");
}

function getErrorMessage(error: unknown) {
  return error instanceof Error
    ? error.message
    : translate("系统变更请求失败。");
}

export function SystemChangeWorkflow({
  capabilities,
  identityKey,
  onExecuted,
}: {
  capabilities: Capability[];
  identityKey: string;
  onExecuted: () => void;
}) {
  const capability = useCallback(
    (name: string) => capabilities.find((item) => item.name === name),
    [capabilities],
  );
  const planCapability = capability("system_change_plan");
  const getCapability = capability("system_change_get");
  const applyCapability = capability("system_change_apply");
  const confirmCapability = capability("system_change_confirm");
  const rollbackCapability = capability("system_change_rollback");
  const tasksCapability = capability("tasks_get");
  const canPlan = hasSystemChangePlanSchema(planCapability);
  const canGet = hasSystemChangeGetSchema(getCapability);
  const canApply =
    hasSystemChangeIDSchema(applyCapability, "system_change_apply") &&
    applyCapability?.task === true;
  const canConfirm =
    hasSystemChangeIDSchema(confirmCapability, "system_change_confirm") &&
    confirmCapability?.task !== true;
  const canRollback =
    hasSystemChangeIDSchema(rollbackCapability, "system_change_rollback") &&
    rollbackCapability?.task === true;
  const canReadTasks = hasRequiredIDSchema(tasksCapability, "tasks_get");

  const [storageKey, setStorageKey] = useState("");
  const [hydrated, setHydrated] = useState(false);
  const [storageReady, setStorageReady] = useState(false);
  const [recovery, setRecovery] = useState<SystemChangeRecovery>();
  const [recoveryInvalid, setRecoveryInvalid] = useState(false);
  const [storageError, setStorageError] = useState("");
  const [plan, setPlan] = useState<SystemChangePlan>();
  const [planFresh, setPlanFresh] = useState(false);
  const [planLoading, setPlanLoading] = useState(false);
  const [planError, setPlanError] = useState("");
  const [planStale, setPlanStale] = useState(false);
  const [task, setTask] = useState<Task>();
  const [taskLoading, setTaskLoading] = useState(false);
  const [taskError, setTaskError] = useState("");
  const [operationError, setOperationError] = useState("");
  const [mutationBusy, setMutationBusy] = useState(false);
  const [confirmation, setConfirmation] = useState<Confirmation>();
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  useEffect(() => {
    const key = systemChangeRecoveryKey(window.location.origin, identityKey);
    setStorageKey(key);
    setPlan(undefined);
    setPlanFresh(false);
    setPlanError("");
    setPlanStale(false);
    setTask(undefined);
    setTaskError("");
    setOperationError("");
    setRecoveryInvalid(false);
    setConfirmation(undefined);
    try {
      setStorageReady(canUseSystemChangeStorage(window.localStorage, key));
      setRecovery(readSystemChangeRecovery(window.localStorage, key));
      setStorageError("");
    } catch {
      setStorageReady(false);
      setRecovery(undefined);
      setStorageError(
        translate("浏览器无法保存计划恢复记录，系统变更操作已停用。"),
      );
    }
    setHydrated(true);
  }, [identityKey]);

  const persistRecovery = useCallback(
    (next: SystemChangeRecovery | undefined) => {
      if (!storageKey) return false;
      try {
        writeSystemChangeRecovery(window.localStorage, storageKey, next);
        setRecovery(next);
        setStorageError("");
        return true;
      } catch {
        setStorageReady(false);
        setStorageError(
          translate(
            "恢复记录保存失败；为避免丢失执行状态，系统变更操作已停用。",
          ),
        );
        return false;
      }
    },
    [storageKey],
  );

  const readPlan = useCallback(
    async (id: string): Promise<SystemChangePlan | undefined> => {
      const input = systemChangeIDInput(getCapability, "system_change_get", id);
      if (!input || !canGet) {
        setPlanFresh(false);
        setPlanError(translate("当前账号未提供可用的系统变更计划读取能力。"));
        return undefined;
      }
      setPlanLoading(true);
      setPlanFresh(false);
      setPlanError("");
      try {
        const value = await action<unknown>("system_change_get", input);
        const current = parseSystemChangePlan(value);
        if (!current || current.id !== id) {
          setPlan(undefined);
          setPlanError(
            translate("服务返回的计划记录无法验证；请保留当前状态后重试读取。"),
          );
          return undefined;
        }
        setPlan(current);
        setPlanFresh(true);
        const currentRecovery = readSystemChangeRecovery(
          window.localStorage,
          storageKey,
        );
        if (
          currentRecovery?.planId === id &&
          currentRecovery.pendingAction &&
          !currentRecovery.taskId &&
          systemChangeIntentResolved(current, currentRecovery.pendingAction)
        ) {
          persistRecovery({ planId: id });
          setOperationError("");
        }
        if (
          current.state === "planned" &&
          current.expires_at &&
          Date.parse(current.expires_at) <= Date.now()
        )
          setPlanStale(true);
        return current;
      } catch (error) {
        setPlanFresh(false);
        setPlanError(getErrorMessage(error));
        if (error instanceof APIError && [403, 404].includes(error.status)) {
          persistRecovery(undefined);
          setPlan(undefined);
          setRecoveryInvalid(true);
        }
        return undefined;
      } finally {
        setPlanLoading(false);
      }
    },
    [canGet, getCapability, persistRecovery, storageKey],
  );

  useEffect(() => {
    if (!hydrated || !recovery?.planId || !canGet) return;
    void readPlan(recovery.planId);
  }, [canGet, hydrated, readPlan, recovery?.planId]);

  const readTask = useCallback(async () => {
    const taskId = recovery?.taskId;
    if (!taskId || !canReadTasks) {
      setTaskError(translate("当前账号无法读取该执行任务；计划状态仍待核对。"));
      return;
    }
    setTaskLoading(true);
    setTaskError("");
    try {
      const next = await action<Task>("tasks_get", { id: taskId });
      setTask(next);
      if (systemChangeTaskFinished(next.status ?? next.state)) {
        const current = await readPlan(recovery.planId);
        if (current) persistRecovery({ planId: recovery.planId });
        else
          setTaskError(translate("任务已结束，但计划读取失败；操作仍待核对。"));
      }
    } catch (error) {
      setTaskError(getErrorMessage(error));
      if (error instanceof APIError && [403, 404].includes(error.status)) {
        persistRecovery(undefined);
        setPlan(undefined);
        setPlanFresh(false);
        setRecoveryInvalid(true);
      }
    } finally {
      setTaskLoading(false);
    }
  }, [canReadTasks, persistRecovery, readPlan, recovery]);

  useEffect(() => {
    if (!recovery?.taskId) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      if (!live) return;
      const taskId = recovery.taskId!;
      if (!canReadTasks) {
        setTaskError(translate("当前账号无法读取任务状态；操作保持待核对。"));
        return;
      }
      setTaskLoading(true);
      try {
        const next = await action<Task>("tasks_get", { id: taskId });
        if (!live) return;
        setTask(next);
        setTaskError("");
        if (systemChangeTaskFinished(next.status ?? next.state)) {
          const current = await readPlan(recovery.planId);
          if (live && current) persistRecovery({ planId: recovery.planId });
          else if (live)
            setTaskError(
              translate("任务已结束，但计划读取失败；操作仍待核对。"),
            );
          return;
        }
      } catch (error) {
        if (!live) return;
        setTaskError(getErrorMessage(error));
        if (error instanceof APIError && [403, 404].includes(error.status)) {
          persistRecovery(undefined);
          setPlan(undefined);
          setPlanFresh(false);
          setRecoveryInvalid(true);
          return;
        }
        return;
      } finally {
        if (live) setTaskLoading(false);
      }
      timer = setTimeout(poll, 1000);
    };
    void poll();
    return () => {
      live = false;
      if (timer) clearTimeout(timer);
    };
  }, [
    canReadTasks,
    persistRecovery,
    readPlan,
    recovery?.planId,
    recovery?.taskId,
  ]);

  const available = useMemo(
    () => canPlan || canGet || canApply || canConfirm || canRollback,
    [canApply, canConfirm, canGet, canPlan, canRollback],
  );

  const createPlan = (value: unknown) => {
    const created = parseSystemChangePlan(value);
    if (!created) {
      setPlanFresh(false);
      setPlanError(
        translate("计划返回内容不完整；请刷新并核对服务端状态，不会继续执行。"),
      );
      return;
    }
    if (!persistRecovery({ planId: created.id })) {
      setPlan(created);
      setPlanFresh(false);
      setPlanError(translate("计划已生成，但恢复记录未能保存；请勿继续操作。"));
      return;
    }
    setPlan(created);
    setPlanFresh(true);
    setPlanStale(false);
    setPlanError("");
    setOperationError("");
    setTask(undefined);
    setTaskError("");
    onExecuted();
  };

  const askToRun = (name: SystemChangeAction, retry = false) => {
    const prior = recovery;
    const key =
      retry && prior?.pendingAction === name && prior.intentKey
        ? prior.intentKey
        : intentKey();
    setConfirmation({ action: name, key, retry });
    setOperationError("");
  };

  const runConfirmed = async () => {
    const selected = confirmation;
    if (!selected || !plan || !planFresh || !storageReady) return;
    const cap = capability(selected.action);
    const input = systemChangeIDInput(cap, selected.action, plan.id);
    if (!input) {
      setOperationError(
        translate("当前能力描述不包含所需计划参数，操作已停止。"),
      );
      return;
    }
    const request: SystemChangeRecovery = {
      planId: plan.id,
      pendingAction: selected.action,
      intentKey: selected.key,
    };
    if (!persistRecovery(request)) return;
    setMutationBusy(true);
    setPlanFresh(false);
    setConfirmation(undefined);
    setOperationError("");
    setTaskError("");
    try {
      const result = await action<unknown>(selected.action, input, {
        idempotencyKey: selected.key,
      });
      if (
        selected.action === "system_change_apply" ||
        selected.action === "system_change_rollback"
      ) {
        const taskId = systemChangeTaskID(result);
        if (!taskId) {
          setOperationError(
            translate(
              "服务未返回可核对的任务编号；计划保留为待核对状态，请勿重复创建新计划。",
            ),
          );
          await readPlan(plan.id);
        } else if (!persistRecovery({ ...request, taskId })) {
          setOperationError(
            translate("任务已提交，但任务恢复记录保存失败；当前状态待核对。"),
          );
        } else if (!canReadTasks) {
          setTaskError(
            translate(
              "任务已提交，但当前账号缺少 tasks_get 能力；状态待核对。",
            ),
          );
        } else {
          setTask(undefined);
        }
      } else {
        const current = await readPlan(plan.id);
        if (current && systemChangeIntentResolved(current, selected.action)) {
          persistRecovery({ planId: current.id });
        } else if (current) {
          setOperationError(
            translate("操作已返回，但计划状态仍未确认；请保留记录并重新读取。"),
          );
        }
      }
      onExecuted();
    } catch (error) {
      setOperationError(getErrorMessage(error));
      if (error instanceof APIError && [403, 404].includes(error.status)) {
        persistRecovery(undefined);
        setPlan(undefined);
        setPlanFresh(false);
        setRecoveryInvalid(true);
      } else {
        const current = await readPlan(plan.id);
        if (
          current &&
          error instanceof APIError &&
          error.status >= 400 &&
          error.status < 500
        ) {
          persistRecovery({ planId: current.id });
          if (["host_conflict", "plan_expired"].includes(error.code))
            setPlanStale(true);
        }
      }
    } finally {
      setMutationBusy(false);
    }
  };

  const settleUnknownIntent = async () => {
    if (!recovery?.pendingAction || !recovery.intentKey) return;
    askToRun(recovery.pendingAction, true);
  };

  const blocked =
    !storageReady ||
    !planFresh ||
    planLoading ||
    mutationBusy ||
    !!recovery?.taskId ||
    (!!recovery?.pendingAction && !recovery.taskId) ||
    planStale;
  const allow = (name: SystemChangeAction, available: boolean) =>
    available &&
    systemChangeActionAllowed(plan, name, {
      fresh: planFresh,
      blocked,
      now,
    });
  const canCreatePlan =
    hydrated &&
    !recoveryInvalid &&
    storageReady &&
    canPlan &&
    canGet &&
    (!recovery ||
      (!!plan && ["applied", "rolled_back"].includes(plan.state)) ||
      planStale);

  if (!available)
    return (
      <Unavailable
        title={translate("系统变更工作流未开放")}
        detail={translate("当前身份没有可用的系统变更计划能力。")}
      />
    );

  return (
    <Card className="panel-card system-change-workflow">
      <div className="card-heading">
        <div>
          <h2>{translate("系统变更计划")}</h2>
          <p className="muted">
            {translate(
              "先由服务器预检并生成计划；应用、连接确认和回滚都需要单独操作。",
            )}
          </p>
        </div>
        {plan && <PlanState state={plan.state} />}
      </div>
      {storageError && (
        <div className="notice danger" role="alert">
          {storageError}
        </div>
      )}
      {recoveryInvalid && (
        <div className="notice danger" role="alert">
          {translate(
            "当前身份无法读取上次计划或任务，恢复记录已清除，系统变更继续停用。请先确认身份与能力后重新载入页面。",
          )}
        </div>
      )}
      {!hydrated && (
        <Unavailable title={translate("正在读取本地计划恢复记录…")} />
      )}
      {hydrated && !canGet && (
        <div className="notice warning" role="status">
          {translate(
            "当前能力集没有可用的 `system_change_get`；无法验证计划状态，所有变更操作保持停用。",
          )}
        </div>
      )}
      {hydrated && canGet && !canPlan && !recovery && !plan && (
        <div className="notice warning" role="status">
          {translate(
            "当前身份没有可用的 `system_change_plan` 写入能力，不能创建计划。",
          )}
        </div>
      )}
      {planError && (
        <div className="notice danger" role="alert">
          <p>{planError}</p>
          {recovery?.planId && canGet && (
            <Button
              label={translate("重新读取计划")}
              variant="ghost"
              isLoading={planLoading}
              isDisabled={planLoading || mutationBusy}
              onClick={() => void readPlan(recovery.planId)}
            />
          )}
        </div>
      )}
      {plan && (
        <section className="stack" aria-label={translate("当前系统变更计划")}>
          <div className="resource-summary">
            <span>{translate("计划状态")}</span>
            <PlanState state={plan.state} />
            {plan.created_at && (
              <span>
                {translate("创建于 ")}
                {fullDate(plan.created_at)}
              </span>
            )}
            {plan.expires_at && (
              <span>
                {translate("计划有效至 ")}
                {fullDate(plan.expires_at)}
              </span>
            )}
            {plan.confirmation_deadline && (
              <span>
                {translate("连接确认截止 ")}
                {fullDate(plan.confirmation_deadline)}
              </span>
            )}
          </div>
          <div className="detail-grid">
            {Object.entries(plan.change).map(([key, value]) => (
              <div key={key}>
                <small>{fieldLabel(key)}</small>
                <span>{displayValue(key, value)}</span>
              </div>
            ))}
          </div>
          {plan.error && (
            <div className="notice warning" role="status">
              {plan.error}
            </div>
          )}
          {planStale && (
            <div className="notice warning" role="status">
              {translate(
                "计划基线已变化或已过期。请重新生成计划；不会沿用旧计划应用变更。",
              )}
            </div>
          )}
          {!planFresh && !planLoading && (
            <div className="notice warning" role="status">
              {translate(
                "当前显示内容不是已确认的最新状态，读取成功前不能确认或继续变更。",
              )}
            </div>
          )}
          {recovery?.pendingAction && !recovery.taskId && (
            <div className="notice warning" role="status">
              {translate("上次“")}
              {actionTitle[recovery.pendingAction]}
              {translate(
                "”的响应尚未核实。先重新读取计划；若状态仍允许，可使用同一请求标识显式重试。",
              )}
              {planFresh && recovery.intentKey && (
                <div className="button-row">
                  <Button
                    label={translate("按原请求继续核对")}
                    variant="ghost"
                    isDisabled={mutationBusy || planLoading || !storageReady}
                    onClick={() => void settleUnknownIntent()}
                  />
                </div>
              )}
            </div>
          )}
          {recovery?.taskId && (
            <div className="notice warning" role="status">
              {translate(
                "后台任务仍是计划的执行依据。任务读取或计划回读失败时，后续操作会保持停用。",
              )}
            </div>
          )}
          {(taskError || task?.error) && (
            <div className="notice danger" role="alert">
              {taskError || task?.error}
              {recovery?.taskId && canReadTasks && (
                <div className="button-row">
                  <Button
                    label={translate("重新读取任务状态")}
                    variant="ghost"
                    isLoading={taskLoading}
                    isDisabled={taskLoading || mutationBusy}
                    onClick={() => void readTask()}
                  />
                </div>
              )}
            </div>
          )}
          {task && (
            <div className="resource-summary" aria-live="polite">
              <span>{translate("后台任务")}</span>
              <State value={task.status ?? task.state} />
              {(task.status === "failed" || task.state === "failed") &&
                task.error && <span>{task.error}</span>}
            </div>
          )}
          {operationError && (
            <div className="notice danger" role="alert">
              {operationError}
            </div>
          )}
          <div className="button-row">
            <Button
              label={translate("重新读取计划状态")}
              variant="ghost"
              isLoading={planLoading}
              isDisabled={planLoading || mutationBusy || !canGet}
              onClick={() => void readPlan(plan.id)}
            />
            {allow("system_change_apply", canApply && canReadTasks) && (
              <Button
                label={translate("应用计划")}
                variant="primary"
                onClick={() => askToRun("system_change_apply")}
              />
            )}
            {allow("system_change_confirm", canConfirm) && (
              <Button
                label={translate("确认配置正常")}
                variant="primary"
                onClick={() => askToRun("system_change_confirm")}
              />
            )}
            {allow("system_change_rollback", canRollback && canReadTasks) && (
              <Button
                label={translate("回滚变更")}
                variant="ghost"
                onClick={() => askToRun("system_change_rollback")}
              />
            )}
          </div>
          {confirmation && (
            <div
              className="notice warning"
              role="alertdialog"
              aria-label={translate("确认系统变更")}
            >
              <strong>
                {confirmation.retry
                  ? translate("继续核对原请求")
                  : translate("确认{0}", [actionTitle[confirmation.action]])}
              </strong>
              <p>
                {confirmation.action === "system_change_apply"
                  ? translate("服务器会再次核对计划基线并执行变更。")
                  : confirmation.action === "system_change_confirm"
                    ? translate(
                        "服务器会验证连接敏感配置；只有验证成功后计划才会确认为已应用。",
                      )
                    : translate("服务器会校验当前快照后恢复计划记录的原状态。")}
              </p>
              <div className="button-row">
                <Button
                  label={translate("取消")}
                  variant="ghost"
                  isDisabled={mutationBusy}
                  onClick={() => setConfirmation(undefined)}
                />
                <Button
                  label={
                    confirmation.retry
                      ? translate("按原请求继续")
                      : actionTitle[confirmation.action]
                  }
                  variant="primary"
                  isLoading={mutationBusy}
                  isDisabled={mutationBusy || !planFresh || !storageReady}
                  onClick={() => void runConfirmed()}
                />
              </div>
            </div>
          )}
        </section>
      )}
      {!plan && planLoading && (
        <Unavailable title={translate("正在读取系统变更计划…")} />
      )}
      {canCreatePlan ? (
        <section className="stack" aria-label={translate("创建系统变更计划")}>
          {recovery && plan && (
            <p className="muted">
              {translate("当前计划已结束，可以准备新的计划。")}
            </p>
          )}
          {planCapability && (
            <ActionForm
              capability={planCapability}
              cacheKeySuffix={`system-change:${storageKey}`}
              onExecuted={createPlan}
            />
          )}
        </section>
      ) : (
        !plan &&
        !planLoading &&
        canPlan &&
        !recovery &&
        !canGet && (
          <Unavailable
            title={translate("无法读取计划")}
            detail={translate(
              "需要同时具备 system_change_plan 与 system_change_get 的实际能力描述。",
            )}
          />
        )
      )}
      {plan?.state === "awaiting_confirmation" && !canConfirm && (
        <div className="notice warning" role="status">
          {translate(
            "计划等待连接确认，但当前能力集没有可用的 `system_change_confirm`；请由具备该能力的同一身份完成核验。",
          )}
        </div>
      )}
      {plan &&
        (plan.state === "planned" || plan.state === "applied") &&
        (canApply || canRollback) &&
        !canReadTasks && (
          <div className="notice warning" role="status">
            {translate(
              "当前身份没有 `tasks_get` 能力，无法核实异步应用或回滚任务；这些操作保持停用。",
            )}
          </div>
        )}
      {hydrated && !storageReady && !storageError && (
        <div className="notice warning" role="status">
          {translate("浏览器恢复记录不可用；系统变更提交已停用。")}
        </div>
      )}
      {taskLoading && <p className="muted">{translate("正在读取后台任务…")}</p>}
      {planLoading && <p className="muted">{translate("正在核对计划状态…")}</p>}
    </Card>
  );
}
