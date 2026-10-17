"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Capability, fullDate, list } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { Page } from "@/lib/resources";
import { ActionBrowser, ActionForm } from "./actions";
import { SystemChangeWorkflow } from "./system-change-workflow";
import {
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
type RecordData = Record<string, unknown> & {
  id: string;
  name?: string;
  revision?: number;
};
type Sample = {
  timestamp: string;
  cpu_percent: number | null;
  memory: { percent: number } | null;
  disk: { percent: number } | null;
};
const descriptions: Record<string, string> = {
  mode: "方式",
  timezone: "时区",
  action: "目标动作",
  next_run_at: "下次执行",
  last_run_at: "上次执行",
  last_error: "最近错误",
  kind: "类型",
  unit: "服务单元",
  active_state: "运行状态",
  sub_state: "子状态",
  ownership: "所有权",
  enabled: "启用",
};
const cell = (key: string, value: unknown) =>
  typeof value === "boolean"
    ? value
      ? "是"
      : "否"
    : key.endsWith("_at")
      ? fullDate(String(value ?? ""))
      : typeof value === "object"
        ? JSON.stringify(value)
        : String(value ?? "—");
export function OperationsView({
  page,
  capabilities,
  identityKey,
  onExecuted,
}: {
  page: "monitor" | "schedules";
  capabilities: Capability[];
  identityKey: string;
  onExecuted: () => void;
}) {
  const [tab, setTab] = useState(page === "monitor" ? "services" : "schedules");
  const [selected, setSelected] = useState<RecordData>();
  const [open, setOpen] = useState(false);
  const [operation, setOperation] = useState<string>();
  const [other, setOther] = useState<string>();
  const [creating, setCreating] = useState(false);
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const name =
    tab === "services"
      ? "services_list"
      : tab === "notifications"
        ? "notifications_channels_list"
        : "schedules_list";
  const records = useResource<RecordData[] | Page<RecordData>>(
    name,
    {},
    has(name),
  );
  const history = useResource<{
    samples: Sample[];
    sample_interval_seconds: number;
  }>(
    "monitor_history",
    { limit: 500 },
    page === "monitor" && has("monitor_history"),
    60000,
  );
  const rows = list(records.data);
  const current = rows.find((row) => row.id === selected?.id) ?? selected;
  const times = (history.data?.samples ?? []).map((sample) =>
    Date.parse(sample.timestamp),
  );
  const intervals = times
    .slice(1)
    .map((time, i) => time - times[i])
    .filter((value) => Number.isFinite(value) && value > 0)
    .sort((a, b) => a - b);
  const gap =
    Math.max(
      (history.data?.sample_interval_seconds ?? 60) * 1000,
      intervals[Math.floor(intervals.length / 2)] ?? 0,
    ) * 3;
  const title =
    tab === "services"
      ? "主机服务"
      : tab === "notifications"
        ? "通知渠道"
        : "计划任务";
  const create = capabilities.find(
    (c) =>
      c.name ===
      (tab === "notifications"
        ? "notifications_channels_save"
        : "schedules_create"),
  );
  const refresh = () => {
    void records.reload();
    void history.reload();
    onExecuted();
  };
  const advancedCapabilities =
    page === "monitor"
      ? capabilities.filter((capability) => !/^system_change_/.test(capability.name))
      : capabilities;
  const prefix =
    tab === "services"
      ? /^services_get$/
      : tab === "notifications"
        ? /^notifications_/
        : /^schedules_/;
  const preset = current
    ? {
        ...Object.fromEntries(
          Object.entries(current).filter(
            ([k]) => !["created_by", "revision"].includes(k),
          ),
        ),
        id: current.id,
        service_id: current.id,
        channel_id: current.id,
        expected_revision: current.revision,
      }
    : {};
  const series = [
    {
      key: "cpu",
      label: "CPU",
      color: "var(--accent)",
      values: (history.data?.samples ?? []).map((s) => s.cpu_percent),
    },
    {
      key: "memory",
      label: "内存",
      color: "var(--success)",
      values: (history.data?.samples ?? []).map(
        (s) => s.memory?.percent ?? null,
      ),
    },
    {
      key: "disk",
      label: "磁盘",
      color: "var(--warning)",
      values: (history.data?.samples ?? []).map((s) => s.disk?.percent ?? null),
    },
  ];
  return (
    <div className="stack">
      {page === "monitor" && (
        <Card className="panel-card">
          <div className="card-heading">
            <h2>指标历史</h2>
            <span className="muted">最近24小时</span>
          </div>
          {history.error && (
            <div className="notice danger" role="alert">
              {history.error}
            </div>
          )}
          {history.data?.samples.length ? (
            <>
              <div
                className="metric-history"
                role="img"
                aria-label="实际主机CPU、内存与磁盘使用百分比历史"
              >
                <svg viewBox="0 0 700 140" preserveAspectRatio="none">
                  {[0, 25, 50, 75, 100].map((y) => (
                    <line
                      key={y}
                      x1="0"
                      x2="700"
                      y1={140 - y * 1.4}
                      y2={140 - y * 1.4}
                      stroke="var(--border)"
                      strokeDasharray="4 4"
                    />
                  ))}
                  {series.map((series) => {
                    let path = "";
                    let segment = false;
                    for (let i = 0; i < series.values.length; i++) {
                      const value = series.values[i];
                      if (
                        typeof value !== "number" ||
                        !Number.isFinite(value) ||
                        !Number.isFinite(times[i])
                      ) {
                        segment = false;
                        continue;
                      }
                      if (i > 0 && times[i] - times[i - 1] > gap)
                        segment = false;
                      path += `${segment ? "L" : "M"}${((times[i] - times[0]) / Math.max(1, times.at(-1)! - times[0])) * 700},${140 - Math.min(100, Math.max(0, value)) * 1.4} `;
                      segment = true;
                    }
                    return (
                      <path
                        key={series.key}
                        d={path}
                        fill="none"
                        stroke={series.color}
                        strokeWidth="2"
                      />
                    );
                  })}
                </svg>
              </div>
              <div className="resource-summary">
                {series.map((s) => (
                  <span key={s.key} style={{ color: s.color }}>
                    {s.label}
                  </span>
                ))}
                <span className="muted">
                  {history.data.samples.length} 个实际样本 · 采样间隔
                  {history.data.sample_interval_seconds}秒
                </span>
              </div>
              <p className="muted">
                {fullDate(history.data.samples[0].timestamp)} —{" "}
                {fullDate(history.data.samples.at(-1)?.timestamp)}
              </p>
            </>
          ) : (
            <Unavailable
              title={history.loading ? "正在读取监控历史…" : "暂无历史样本"}
              icon="overview"
              detail="采样器会保存真实主机数据；没有采样的时段不会补造数据。"
            />
          )}
        </Card>
      )}
      <Card className="panel-card">
        <div className="card-heading">
          <h2>{title}</h2>
          {tab !== "services" && create && (
            <Button
              label={tab === "notifications" ? "新增通知渠道" : "创建计划任务"}
              variant="primary"
              onClick={() => setCreating(true)}
            />
          )}
        </div>
        {page === "schedules" && (
          <div className="section-tabs" role="tablist" aria-label="自动化分类">
            {[
              { id: "schedules", label: "计划任务" },
              { id: "notifications", label: "通知渠道" },
            ].map((t) => (
              <button
                role="tab"
                aria-selected={tab === t.id}
                key={t.id}
                onClick={() => {
                  setTab(t.id);
                  setSelected(undefined);
                  setOperation(undefined);
                }}
              >
                {t.label}
              </button>
            ))}
          </div>
        )}
        {records.error && (
          <div className="notice danger" role="alert">
            {records.error}
          </div>
        )}
        {!has(name) ? (
          <Unavailable title={`${title}服务暂不可用`} />
        ) : (
          <>
            <div className="resource-table-wrap">
              <table className="resource-table">
                <thead>
                  <tr>
                    <th>名称</th>
                    {(tab === "services"
                      ? ["unit", "active_state", "ownership"]
                      : tab === "notifications"
                        ? ["kind"]
                        : ["mode", "action", "enabled", "next_run_at"]
                    ).map((k) => (
                      <th key={k}>{descriptions[k] ?? k}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.id}>
                      <td>
                        <button
                          className="resource-name"
                          onClick={() => {
                            setSelected(row);
                            setOpen(true);
                            setOperation(undefined);
                          }}
                        >
                          {row.name ?? row.id}
                        </button>
                        <small>{row.id}</small>
                      </td>
                      {(tab === "services"
                        ? ["unit", "active_state", "ownership"]
                        : tab === "notifications"
                          ? ["kind"]
                          : ["mode", "action", "enabled", "next_run_at"]
                      ).map((k) => (
                        <td key={k}>
                          {k === "active_state" ? (
                            <State value={row[k] as string} />
                          ) : (
                            cell(k, row[k])
                          )}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {!rows.length && !records.loading && !records.error && (
              <Unavailable
                title={`暂无${title}`}
                detail={
                  tab === "services"
                    ? "仅列出受信任主机策略中明确管理的服务。"
                    : "创建后由网页与MCP共同管理。"
                }
              />
            )}
          </>
        )}
      </Card>
      {page === "monitor" && (
        <SystemChangeWorkflow
          capabilities={capabilities}
          identityKey={identityKey}
          onExecuted={refresh}
        />
      )}
      {current && (
        <ResourceDrawer
          open={open}
          title={`${title} · ${current.name ?? current.id}`}
          onClose={() => setOpen(false)}
        >
          <div className="detail-grid">
            {Object.entries(current)
              .filter(([key]) => descriptions[key])
              .map(([key, value]) => (
                <div key={key}>
                  <small>{descriptions[key]}</small>
                  <span>{cell(key, value)}</span>
                </div>
              ))}
          </div>
          <div className="resource-summary">
            <code>修订 {current.revision ?? "未提供"}</code>
          </div>
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              prefix.test(name) &&
              !name.endsWith("_list") &&
              !name.endsWith("_create")
            }
            preset={preset}
            selected={operation}
            onSelect={setOperation}
            onExecuted={refresh}
          />
        </ResourceDrawer>
      )}
      {create && (
        <ResourceDrawer
          open={creating}
          title={tab === "notifications" ? "新增通知渠道" : "创建计划任务"}
          onClose={() => setCreating(false)}
        >
          <ActionForm capability={create} onExecuted={refresh} />
        </ResourceDrawer>
      )}
      <details className="advanced-actions">
        <summary>
          {page === "monitor" ? "服务与系统变更计划" : "自动化操作与执行历史"}
        </summary>
        <ActionBrowser
          capabilities={advancedCapabilities}
          category={page}
          selected={other}
          onSelect={setOther}
          onExecuted={refresh}
        />
      </details>
    </div>
  );
}
