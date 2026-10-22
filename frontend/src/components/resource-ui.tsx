"use client";
import { Button } from "@astryxdesign/core/Button";
import { Dialog } from "@astryxdesign/core/Dialog";
import { Capability } from "@/lib/api";
import { actionLabel } from "@/lib/labels";
import { Icon, IconName } from "./icon";
import { ActionContext, ActionForm, References } from "./actions";
export function State({ value, reason }: { value?: string; reason?: string }) {
  const labels: Record<string, string> = {
    active: "已启用",
    submitted: "已提交，等待结果",
    skipped: "已跳过",
    observation_denied: "结果读取权限已失效",
    deleted: "已删除",
    creating: "创建中",
    deleting: "删除中",
    updating: "更新中",
    running: "运行中",
    stopped: "已停止",
    created: "待构建",
    built: "已构建",
    retained: "已停用，文件保留",
    purging: "清理中",
    configuring: "配置中",
    seeding: "初始化数据",
    degraded: "复制异常",
    removing: "移除中",
    removed: "已移除",
    recovery_required: "需要恢复",
    not_installed: "未安装",
    healthy: "健康",
    unhealthy: "异常",
    unavailable: "不可用",
    unsupported: "暂不支持",
    installing: "安装中",
    upgrading: "升级中",
    failed: "失败",
    succeeded: "执行成功",
    pending: "待处理",
    disabled: "已停用",
    unknown: "状态未知",
    not_tested: "尚未验证",
    verified: "验证成功",
    queued: "排队中",
    canceled: "已取消",
    interrupted: "执行中断",
    managed: "面板管理",
    adopted: "已接管",
    pause_pending: "暂停待完成",
    paused: "已暂停",
    resume_pending: "恢复待完成",
    conflict: "状态冲突",
    legacy_recovery_required: "旧版需恢复",
    legacy_recovery_pending: "旧版恢复待继续",
    legacy_unrecoverable: "无法安全恢复",
    pending_recovery: "需要恢复",
    pending_reload: "等待重载验证",
    settled: "配置已应用",
    unconfigured: "尚未配置",
    unmanaged: "未受管理",
    restored: "已恢复",
    ready: "预览待执行",
    expired: "预览已过期",
    partial: "部分完成",
    completed: "已完成",
    outcome_unknown: "结果待核实",
    not_started: "未执行",
    checking: "校验中",
    executing: "执行中",
  };
  return (
    <span
      title={reason}
      className={`status-chip ${value === "failed" || value === "unhealthy" ? "danger" : value === "unavailable" || value === "unsupported" ? "warning" : "neutral"}`}
    >
      {value ? (labels[value] ?? value) : "状态未提供"}
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
      <p>{detail ?? "当前服务未提供此能力，或当前账户没有访问权限。"}</p>
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
  return (
    <div className="pagination">
      <span className="muted">
        第 {Math.floor(offset / limit) + 1} 页
        {total !== undefined ? ` · 共 ${total} 条` : ` · 本页 ${count} 条`}
      </span>
      <div className="button-row">
        <Button
          label="上一页资源"
          isDisabled={offset === 0 || loading}
          onClick={() => onOffset(Math.max(0, offset - limit))}
        />
        <Button
          label="下一页资源"
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
  return (
    <Dialog
      isOpen={open}
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      purpose="form"
      width={800}
      maxHeight="calc(100dvh - 40px)"
      position={{ end: 20, top: 20 }}
      padding={6}
      className="resource-drawer"
      aria-label={title}
    >
      <div className="dialog-title">
        <h2>{title}</h2>
        <Button
          label="关闭资源详情"
          icon={<Icon name="close" />}
          isIconOnly
          variant="ghost"
          onClick={onClose}
        />
      </div>
      {children}
    </Dialog>
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
            <p>{c.name}</p>
          </div>
          <Icon name="arrow" size={16} />
        </button>
      ))}
    </div>
  ) : (
    <Unavailable title="此分类暂无已授权的操作" />
  );
}
