"use client";
import { t as translate } from "@/lib/i18n";
import { Icon, IconName } from "./icon";
import { Site } from "@/lib/resources";
export function ResourceSignal({ status, configuration = false }: { status: string; configuration?: boolean }) {
  const enabled = ["running", "active", "enabled"].includes(status);
  const stopped = ["stopped", "disabled", "inactive"].includes(status);
  const label = translate(enabled ? configuration ? "已启用" : "运行中" : stopped ? configuration ? "已停用" : "已停止" : "状态未知");
  return <span className={`resource-signal ${enabled ? "positive" : stopped ? "neutral" : "uncertain"}`} title={label}>
    <span className="resource-signal-dot" aria-hidden="true" />{label}
  </span>;
}
export function siteProject(site: Site): { key: string; label: string; icon: IconName } {
  if (site.project_type === "git" || site.deployment_type === "git" || typeof site.git_repository === "string" && site.git_repository.length > 0)
    return { key: "git", label: translate("Git 项目"), icon: "git" };
  if (site.php_version || site.php_instance) return { key: "php", label: translate("PHP 项目"), icon: "code" };
  if (site.proxies?.some(proxy => proxy.enabled)) return { key: "proxy", label: translate("代理项目"), icon: "network" };
  return { key: "static", label: translate("静态项目"), icon: "files" };
}
