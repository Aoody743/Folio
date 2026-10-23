"use client";
import { t as translate } from "@/lib/i18n";
import { Capability, SystemStatus, duration, list, bytes, fullDate } from "@/lib/api";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { applicationCounts, ApplicationState } from "@/lib/overview";
import { navigate } from "@/lib/navigation";
import { Icon } from "./icon";

export function Overview({
  status,
  error,
  connection,
  capabilities,
}: {
  status?: SystemStatus;
  error?: string;
  connection: string;
  capabilities: Capability[];
}) {
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const sites = useResource<Page<unknown>>(
    "sites_list",
    { limit: 1, offset: 0 },
    has("sites_list"),
  );
  const apps = useResource<Page<ApplicationState>>(
    "hosted_apps_list",
    {},
    has("hosted_apps_list"),
  );
  const counts = apps.data ? applicationCounts(list(apps.data)) : undefined;
  const percent = (value?: number | null) =>
    value == null || !Number.isFinite(value) ? "—" : `${Math.round(value)}%`;
  const ratio = (
    resource?: { used_bytes: number; total_bytes: number } | null,
  ) =>
    resource && resource.total_bytes > 0
      ? (resource.used_bytes / resource.total_bytes) * 100
      : undefined;
  return (
    <div className="status-overview">
      <section
        className="summary-panel host-status"
        aria-label={translate("主机")}
      >
        <button
          className="summary-heading"
          onClick={() => navigate("more", { page: "monitor" })}
        >
          <span>
            <Icon name="server" />
            <strong>{translate("主机")}</strong>
          </span>
          <span className="summary-connection">
            <i
              className={`status-dot ${connection === "online" && !error ? "online" : "offline"}`}
            />
            {error
              ? translate("读取失败")
              : connection === "online"
                ? translate("已连接")
                : connection === "connecting"
                  ? translate("连接中")
                  : translate("已断线")}
            <Icon name="arrow" size={16} />
          </span>
        </button>
        <div className="host-identity">
          <strong>{status?.hostname ?? translate("正在读取…")}</strong>
          <span>
            {translate("运行时间")}{" "}
            {status ? duration(status.uptime_seconds) : "—"}
          </span>
        </div>
        <div className="summary-metrics">
          {[
            { icon: "cpu" as const, label: "CPU", value: status?.cpu_percent, capacity: typeof status?.cpu_cores === "number" ? translate("{0} 核", [status.cpu_cores]) : "—" },
            { icon: "memory" as const, label: translate("内存"), value: ratio(status?.memory), capacity: status?.memory ? `${bytes(status.memory.used_bytes)} / ${bytes(status.memory.total_bytes)}` : "—" },
            { icon: "disk" as const, label: translate("磁盘"), value: ratio(status?.disk), capacity: status?.disk ? `${bytes(status.disk.used_bytes)} / ${bytes(status.disk.total_bytes)}` : "—" },
          ].map(({ icon, label, value, capacity }) => (
            <div key={String(label)}>
              <span><Icon name={icon} size={18} />{label}</span>
              <strong>{percent(value)}</strong>
              <small className="metric-capacity">{capacity}</small>
              <progress
                max={100}
                value={typeof value === "number" ? value : undefined}
                aria-label={String(label)}
              />
            </div>
          ))}
        </div>
        <p className="overview-sampled">{translate("数据时间")}：{fullDate(status?.sampled_at ?? status?.updated_at)}{error ? ` · ${translate("保留最近数据")}` : ""}</p>
      </section>
      {[
        {
          key: "sites",
          title: translate("网站"),
          allowed: has("sites_list"),
          data: sites.data,
          loading: sites.loading,
          error: sites.error,
          state: sites.state,
          total: sites.data?.total,
          counts: sites.data?.status_counts,
          labels: [
            translate("已启用"),
            translate("已停用"),
            translate("状态未知"),
          ],
          go: (state: string) => navigate("sites", { status: state }),
        },
        {
          key: "software",
          title: translate("应用"),
          allowed: has("hosted_apps_list"),
          data: apps.data,
          loading: apps.loading,
          error: apps.error,
          state: apps.state,
          total: apps.data ? list(apps.data).length : undefined,
          counts,
          labels: [
            translate("运行中"),
            translate("已停止"),
            translate("需检查"),
          ],
          go: (state: string) =>
            navigate("software", { page: "applications", status: state }),
        },
      ].map((item) => (
        <section
          className="summary-panel resource-status"
          key={item.key}
          aria-label={item.title}
        >
          <button
            className="summary-heading"
            onClick={() => item.go("")}
            disabled={!item.allowed}
          >
            <span>
              <Icon name={item.key === "sites" ? "sites" : "software"} />
              <strong>{item.title}</strong>
              <b>{item.error || !item.allowed ? "—" : (item.total ?? "—")}</b>
            </span>
            <Icon name="arrow" size={16} />
          </button>
          {!item.allowed || item.error || !item.data ? (
            <p className="summary-unavailable" role="status">
              {!item.allowed || item.state === "forbidden"
                ? translate("无访问权限")
                : item.state === "unconfigured"
                  ? translate("主机执行器未配置")
                  : item.error
                    ? translate("读取失败，请重试")
                    : translate("正在读取…")}
            </p>
          ) : (
            <div className="summary-counts">
              {["running", "stopped", "unknown"].map((state, index) => (
                <button key={state} onClick={() => item.go(state)}>
                  <strong>
                    {item.counts?.[state as keyof typeof item.counts] ?? "—"}
                  </strong>
                  <span>{item.labels[index]}</span>
                </button>
              ))}
            </div>
          )}
        </section>
      ))}
    </div>
  );
}
