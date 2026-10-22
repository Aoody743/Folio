"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Capability, fullDate, list } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { Page } from "@/lib/resources";
import { ActionBrowser, ActionForm } from "./actions";
import { TaskResult } from "./task-result";
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
  last_run_status: "最近结果",
  last_task_id: "关联任务",
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
  const scheduleTabs = [
    ...(has("schedules_list")
      ? [{ id: "schedules", label: "计划任务" }]
      : []),
    ...(has("notifications_channels_list")
      ? [{ id: "notifications", label: "通知渠道" }]
      : []),
  ];
  const pageTabs =
    page === "monitor"
      ? has("services_list")
        ? [{ id: "services", label: "主机服务" }]
        : []
      : scheduleTabs;
  const activeTab = pageTabs.some((item) => item.id === tab)
    ? tab
    : pageTabs[0]?.id;
  const name =
    activeTab === "services"
      ? "services_list"
      : activeTab === "notifications"
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
    activeTab === "services"
      ? "主机服务"
      : activeTab === "notifications"
        ? "通知渠道"
        : "计划任务";
  const create = capabilities.find(
    (c) =>
      c.name ===
      (activeTab === "notifications"
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
    activeTab === "services"
      ? /^services_get$/
      : activeTab === "notifications"
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
      {page === "monitor" && has("monitor_history") && (
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
      {((page === "monitor" && has("services_list")) ||
        (page === "schedules" && pageTabs.length > 0)) && (
      <Card className="panel-card">
        <div className="card-heading">
          <h2>{title}</h2>
          {activeTab !== "services" && create && (
            <Button
              label={activeTab === "notifications" ? "新增通知渠道" : "创建计划任务"}
              variant="primary"
              onClick={() => setCreating(true)}
            />
          )}
        </div>
        {page === "schedules" && pageTabs.length > 1 && (
          <div className="section-tabs" role="tablist" aria-label="自动化分类">
            {pageTabs.map((t) => (
              <button
                role="tab"
                aria-selected={activeTab === t.id}
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
                    {(activeTab === "services"
                      ? ["unit", "active_state", "ownership"]
                      : activeTab === "notifications"
                        ? ["kind"]
                        : ["mode", "action", "enabled", "next_run_at", "last_run_status"]
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
                      {(activeTab === "services"
                        ? ["unit", "active_state", "ownership"]
                        : activeTab === "notifications"
                          ? ["kind"]
                          : ["mode", "action", "enabled", "next_run_at", "last_run_status"]
                      ).map((k) => (
                        <td key={k}>
                          {k === "active_state" || k === "last_run_status" ? (
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
                  activeTab === "services"
                    ? "仅列出受信任主机策略中明确管理的服务。"
                    : "创建后由网页与MCP共同管理。"
                }
              />
            )}
          </>
        )}
      </Card>
      )}
      {page === "schedules" && !pageTabs.length && (
        <Unavailable title="当前没有可用的计划任务或通知能力" />
      )}
      {page === "monitor" &&
        has("system_change_plan") &&
        has("system_change_get") && (
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
          {activeTab === "schedules" && has("schedules_history") && open && (
            <ScheduleHistory id={current.id} capabilities={capabilities} />
          )}
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
          title={activeTab === "notifications" ? "新增通知渠道" : "创建计划任务"}
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

function ScheduleHistory({id, capabilities}: {id: string; capabilities: Capability[]}) {
  type Run = {id: string; started_at: string; status: string; task_id?: string; error_code?: string; notification_state?: string};
  const history = useResource<Run[]>("schedules_history", {id}, true, 3000);
  const [taskID, setTaskID] = useState<string>();
  return <section className="stack"><h3>最近执行</h3>
    {history.error && <div className="notice danger" role="alert">{history.error}</div>}
    {history.data?.map((run) => <div key={run.id} className="resource-summary">
      <span>{fullDate(run.started_at)}</span><State value={run.status} />
      {run.error_code && <code>{run.error_code}</code>}
      {run.notification_state && <span>失败通知：{run.notification_state === "submitted" ? "已提交" : run.notification_state === "failed" ? "发送失败" : "发送中或待检查"}</span>}
      {run.task_id && capabilities.some((c) => c.name === "tasks_get") && <Button label="查看关联任务" onClick={() => setTaskID(run.task_id)} />}
    </div>)}
    {taskID && <TaskResult key={taskID} submitted={{task_id: taskID}} capabilities={capabilities} onFinished={() => void history.reload()} />}
  </section>;
}
