"use client";
import { t as translate } from "@/lib/i18n";
import { Button } from "@astryxdesign/core/Button";
import { createContext, useContext, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Capability } from "@/lib/api";
import { actionLabel } from "@/lib/labels";
import { Icon, IconName } from "./icon";
import { ActionContext, ActionForm, References } from "./actions";
import type { PageState } from "@/lib/navigation";
export const DetailScope = createContext(true);
export function State({ value, reason }: { value?: string; reason?: string }) {
  const labels: Record<string, string> = {
    active: translate("已启用"),
    enabled: translate("已启用"),
    inactive: translate("已停止"),
    installed: translate("已安装"),
    uninstalled: translate("未安装"),
    not_verified: translate("尚未验证"),
    verifying: translate("校验中"),
    submitted: translate("已提交，等待结果"),
    skipped: translate("已跳过"),
    observation_denied: translate("结果读取权限已失效"),
    deleted: translate("已删除"),
    creating: translate("创建中"),
    deleting: translate("删除中"),
    updating: translate("更新中"),
    running: translate("运行中"),
    stopped: translate("已停止"),
    created: translate("待构建"),
    built: translate("已构建"),
    retained: translate("已停用，文件保留"),
    purging: translate("清理中"),
    configuring: translate("配置中"),
    seeding: translate("初始化数据"),
    degraded: translate("复制异常"),
    removing: translate("移除中"),
    removed: translate("已移除"),
    recovery_required: translate("需要恢复"),
    not_installed: translate("未安装"),
    healthy: translate("健康"),
    unhealthy: translate("异常"),
    unavailable: translate("不可用"),
    unsupported: translate("暂不支持"),
    installing: translate("安装中"),
    upgrading: translate("升级中"),
    failed: translate("失败"),
    succeeded: translate("执行成功"),
    pending: translate("待处理"),
    disabled: translate("已停用"),
    unknown: translate("状态未知"),
    not_tested: translate("尚未验证"),
    verified: translate("验证成功"),
    queued: translate("排队中"),
    canceled: translate("已取消"),
    interrupted: translate("执行中断"),
    managed: translate("面板管理"),
    adopted: translate("已接管"),
    pause_pending: translate("暂停待完成"),
    paused: translate("已暂停"),
    resume_pending: translate("恢复待完成"),
    conflict: translate("状态冲突"),
    legacy_recovery_required: translate("旧版需恢复"),
    legacy_recovery_pending: translate("旧版恢复待继续"),
    legacy_unrecoverable: translate("无法安全恢复"),
    pending_recovery: translate("需要恢复"),
    pending_reload: translate("等待重载验证"),
    settled: translate("配置已应用"),
    unconfigured: translate("尚未配置"),
    unmanaged: translate("未受管理"),
    restored: translate("已恢复"),
    ready: translate("预览待执行"),
    expired: translate("预览已过期"),
    partial: translate("部分完成"),
    completed: translate("已完成"),
    outcome_unknown: translate("结果待核实"),
    not_started: translate("未执行"),
    checking: translate("校验中"),
    executing: translate("执行中"),
  };
  return (
    <span
      title={reason ?? (value && !(value in labels) ? value : undefined)}
      className={`status-chip ${["failed", "unhealthy", "interrupted"].includes(value ?? "") ? "danger" : ["unavailable", "unsupported", "recovery_required", "outcome_unknown", "conflict"].includes(value ?? "") ? "warning" : ["healthy", "verified", "succeeded", "completed", "active"].includes(value ?? "") ? "success" : "neutral"}`}
    >
      {value
        ? (labels[value] ?? translate("状态未知"))
        : translate("状态未提供")}
    </span>
  );
}
export function Unavailable({
  title,
  detail,
  icon = "software",
}: {
  title: string;
  detail?: string;
  icon?: IconName;
}) {
  return (
    <div className="empty">
      <span className="empty-icon">
        <Icon name={icon} size={24} />
      </span>
      <strong>{title}</strong>
      {detail && <p>{detail}</p>}
    </div>
  );
}
export function ResourceError({
  error,
  retry,
  state,
}: {
  error: string;
  retry?: () => void;
  state?: PageState;
}) {
  const unconfigured =
    /privileged|agent_not_configured|特权执行器|executor.*configured/i.test(
      error,
    );
  return (
    <div className={`notice ${unconfigured || state === "unconfigured" ? "warning" : "danger"}`} role="alert">
      <div>
        {state === "forbidden"
          ? translate("无访问权限")
          : unconfigured || state === "unconfigured"
            ? translate("主机执行器未配置")
            : translate("读取失败，请重试")}
      </div>
      {(unconfigured || state === "unconfigured") && (
        <p>{translate("请检查服务器执行器配置，然后重试。")}</p>
      )}
      <details>
        <summary>{translate("技术详情")}</summary>
        <pre>{error}</pre>
      </details>
      {retry && (
        <Button label={translate("重试")} variant="ghost" onClick={retry} />
      )}
    </div>
  );
}
export function Pager({
  offset,
  limit,
  total,
  count,
  loading,
  onOffset,
}: {
  offset: number;
  limit: number;
  total?: number;
  count: number;
  loading: boolean;
  onOffset: (offset: number) => void;
}) {
  if (offset === 0 && count === 0) return null;
  return (
    <div className="pagination">
      <span className="muted">
        {translate("第 {0} 页", [Math.floor(offset / limit) + 1])}
        {total !== undefined
          ? translate(" · 共 {0} 条", [total])
          : translate(" · 本页 {0} 条", [count])}
      </span>
      <div className="button-row">
        <Button
          label={translate("上一页")}
          isDisabled={offset === 0 || loading}
          onClick={() => onOffset(Math.max(0, offset - limit))}
        />
        <Button
          label={translate("下一页")}
          isDisabled={
            loading ||
            (total === undefined ? count < limit : offset + limit >= total)
          }
          onClick={() => onOffset(offset + limit)}
        />
      </div>
    </div>
  );
}
export function ResourceDrawer({
  open,
  title,
  onClose,
  children,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: React.ReactNode;
}) {
  const active = useContext(DetailScope);
  const [target, setTarget] = useState<HTMLElement | null>(null);
  const back = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    setTarget(document.getElementById("detail-pages"));
  }, []);
  useEffect(() => {
    if (!open || !active) return;
    const previous = document.activeElement as HTMLElement | null;
    const timer = setTimeout(() => back.current?.focus(), 0);
    return () => {
      clearTimeout(timer);
      previous?.focus?.();
    };
  }, [open, active]);
  if (!target) return null;
  return createPortal(
    <section
      hidden={!open || !active}
      className="resource-page"
      aria-label={title}
    >
      <div className="resource-page-heading">
        <button
          ref={back}
          className="text-button"
          aria-label={translate("返回列表")}
          onClick={onClose}
        >
          <Icon name="back" size={18} />
          {translate("返回")}
        </button>
        <div className="resource-title">{title.includes(" · ") ? <><span className="resource-kind">{title.split(" · ")[0]}</span><h1>{title.split(" · ").slice(1).join(" · ")}</h1></> : <h1>{title}</h1>}</div>
      </div>
      <div className="resource-page-content">{children}</div>
    </section>,
    target,
  );
}
export function ResourceActions({
  capabilities,
  names,
  preset,
  selected,
  onSelect,
  onExecuted,
  references,
}: {
  capabilities: Capability[];
  names: (name: string) => boolean;
  preset: Record<string, unknown>;
  selected?: string;
  onSelect: (name?: string) => void;
  onExecuted: (result?: unknown, context?: ActionContext) => void;
  references?: References;
}) {
  const allowed = capabilities.filter((c) => names(c.name));
  const current = allowed.find((c) => c.name === selected);
  return current ? (
    <ActionForm
      key={
        current.name +
        String(preset.id ?? preset.path ?? preset.plugin_id ?? "")
      }
      capability={current}
      preset={preset}
      references={references}
      compact
      onBack={() => onSelect(undefined)}
      onExecuted={onExecuted}
    />
  ) : allowed.length ? (
    <div className="action-grid">
      {allowed.map((c) => (
        <button
          key={c.name}
          className="action-tile"
          onClick={() => onSelect(c.name)}
        >
          <span className="heading-icon">
            <Icon name={c.read_only ? "search" : "settings"} />
          </span>
          <div>
            <strong>{actionLabel(c)}</strong>
          </div>
          <Icon name="arrow" size={16} />
        </button>
      ))}
    </div>
  ) : (
    <Unavailable title={translate("此分类暂无已授权的操作")} />
  );
}
