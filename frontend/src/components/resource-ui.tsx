"use client";
import { t as translate } from "@/lib/i18n";
import { Button } from "@astryxdesign/core/Button";
import { Dialog } from "@astryxdesign/core/Dialog";
import { Capability } from "@/lib/api";
import { actionLabel } from "@/lib/labels";
import { Icon, IconName } from "./icon";
import { ActionContext, ActionForm, References } from "./actions";
export function State({ value, reason }: { value?: string; reason?: string }) {
  const labels: Record<string, string> = {
    active: translate("已启用"),
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
      title={reason}
      className={`status-chip ${["failed", "unhealthy", "interrupted"].includes(value ?? "") ? "danger" : ["unavailable", "unsupported", "recovery_required", "outcome_unknown", "conflict"].includes(value ?? "") ? "warning" : ["healthy", "verified", "succeeded", "completed", "active"].includes(value ?? "") ? "success" : "neutral"}`}
    >
      {value ? (labels[value] ?? value) : translate("状态未提供")}
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
      <p>
        {detail ?? translate("当前服务未提供此能力，或当前账户没有访问权限。")}
      </p>
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
        {translate("第")}
        {Math.floor(offset / limit) + 1}
        {translate("页")}
        {total !== undefined
          ? translate(" · 共 {0} 条", [total])
          : translate(" · 本页 {0} 条", [count])}
      </span>
      <div className="button-row">
        <Button
          label={translate("上一页资源")}
          isDisabled={offset === 0 || loading}
          onClick={() => onOffset(Math.max(0, offset - limit))}
        />
        <Button
          label={translate("下一页资源")}
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
          label={translate("关闭资源详情")}
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
    <Unavailable title={translate("此分类暂无已授权的操作")} />
  );
}
