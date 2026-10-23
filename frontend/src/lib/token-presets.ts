import { t as translate } from "./i18n";
export type TokenPurpose = "custom" | "monitor" | "notes";
export function tokenPresetScopes(purpose: TokenPurpose, available: readonly string[]): string[] {
  const presets = { custom: [], monitor: ["system:read", "tasks:read", "audit:read"], notes: ["notes:read", "notes:write"] };
  return (presets[purpose] ?? []).filter(scope => available.includes(scope));
}
export function scopeLabel(scope: string): string {
  const resources: Record<string, string> = {
    system: translate("主机"), notes: translate("共享便签"), tasks: translate("任务"), audit: translate("操作记录"),
    sites: translate("网站"), files: translate("文件"), software: translate("软件"), certificates: translate("证书"),
    database: translate("数据库"), databases: translate("数据库"), cache: translate("缓存"),
    browser: translate("服务器浏览器"), terminal: translate("终端"), schedules: translate("计划任务"),
    notifications: translate("通知"), account: translate("账户"), applications: translate("应用授权"),
  };
  const verbs: Record<string, string> = { read: translate("查看"), write: translate("修改"), manage: translate("管理"), delete: translate("删除"), backup: translate("备份"), export: translate("导出"), config: translate("配置"), use: translate("使用"), server: translate("服务器访问"), root: "Root" };
  const [resource, permission] = scope.split(":", 2);
  return resources[resource] && verbs[permission] ? `${resources[resource]} · ${verbs[permission]}` : scope;
}
