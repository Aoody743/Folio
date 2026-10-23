"use client";
import { SectionHeader } from "./section-header";
import { ResourceTabs } from "./resource-tabs";
import { ResourceError } from "./resource-ui";
import { ClientTable } from "./client-table";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import { MonitorAlerts } from "./monitor-alerts";
import { MetricChart, type MetricSample } from "./metric-chart";
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
const descriptions: Record<string, string> = {
  get mode() {
    return translate("方式");
  },
  get timezone() {
    return translate("时区");
  },
  get action() {
    return translate("目标动作");
  },
  get next_run_at() {
    return translate("下次执行");
  },
  get last_run_at() {
    return translate("上次执行");
  },
  get last_error() {
    return translate("最近错误");
  },
  get last_run_status() {
    return translate("最近结果");
  },
  get last_task_id() {
    return translate("关联任务");
  },
  get kind() {
    return translate("类型");
  },
  get unit() {
    return translate("服务单元");
  },
  get active_state() {
    return translate("运行状态");
  },
  get sub_state() {
    return translate("子状态");
  },
  get ownership() {
    return translate("所有权");
  },
  get enabled() {
    return translate("启用");
  },
};
const cell = (key: string, value: unknown) =>
  key === "ownership" && typeof value === "string"
    ? value === "managed" ? translate("面板管理") : value === "external" ? translate("外部服务") : value === "adopted" ? translate("已接管") : translate("状态未知")
    : typeof value === "boolean"
    ? value
      ? translate("是")
      : translate("否")
    : key.endsWith("_at")
      ? fullDate(String(value ?? ""))
      : typeof value === "object"
        ? JSON.stringify(value)
        : String(value ?? "—");
import { usePageState } from "@/lib/navigation";
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
  const [rangeHours, setRangeHours] = useState(24);
  const [windowTime, setWindowTime] = useState(() => Date.now());
  const [changeOpen, setChangeOpen] = usePageState("more", "view", "");
  useEffect(() => {
    if (page !== "monitor") return;
    const tick = () => { if (document.visibilityState === "visible") setWindowTime(Date.now()); };
    const timer = setInterval(tick, 60000);
    document.addEventListener("visibilitychange", tick);
    window.addEventListener("powerlinux:invalidate", tick);
    return () => { clearInterval(timer); document.removeEventListener("visibilitychange", tick); window.removeEventListener("powerlinux:invalidate", tick); };
  }, [page]);
  const [selectedID, setSelectedID] = usePageState("more", "id", "", true);
  const selected = selectedID ? ({ id: selectedID } as RecordData) : undefined;
  const setSelected = (value?: RecordData) => setSelectedID(value?.id ?? "");
  const open = !!selectedID;
  const setOpen = (value: boolean) => {
    if (!value) setSelectedID("");
  };
  const [operation, setOperation] = useState<string>();
  const [other, setOther] = useState<string>();
  const [creating, setCreating] = usePageState("more", "create", false, true);
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const scheduleTabs = [
    ...(has("schedules_list")
      ? [{ id: "schedules", label: translate("计划任务") }]
      : []),
    ...(has("notifications_channels_list")
      ? [{ id: "notifications", label: translate("通知渠道") }]
      : []),
  ];
  const pageTabs =
    page === "monitor"
      ? has("services_list")
        ? [{ id: "services", label: translate("主机服务") }]
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
    samples: MetricSample[];
    sample_interval_seconds: number;
  }>(
    "monitor_history",
    { start: new Date(windowTime - rangeHours * 3600000).toISOString(), limit: 500 },
    page === "monitor" && has("monitor_history"),
    0,
  );
  const rows = list(records.data);
  const current = rows.find((row) => row.id === selected?.id) ?? selected;
  const title =
    activeTab === "services"
      ? translate("主机服务")
      : activeTab === "notifications"
        ? translate("通知渠道")
        : translate("计划任务");
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
      ? capabilities.filter(
          (capability) => !/^system_change_/.test(capability.name),
        )
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
  return (
    <div className="stack">
      {page === "monitor" && has("monitor_history") && (
        <Card className="panel-card">
          <SectionHeader icon="monitor" className="card-heading">
            <h2>{translate("指标历史")}</h2>
            {has("monitor_alerts_list") && <Button label={translate("告警规则")} variant="ghost" onClick={()=>setChangeOpen("alerts")}/>}
            <select aria-label={translate("监控时间范围")} value={rangeHours} onChange={event => { setRangeHours(Number(event.target.value)); setWindowTime(Date.now()); }}>
              {[1, 6, 24, 168].map(hours => <option key={hours} value={hours}>{hours === 168 ? translate("最近 7 天") : translate("最近 {0} 小时", [hours])}</option>)}
            </select>
          </SectionHeader>
          {history.error && <ResourceError error={history.error} state={history.state} retry={() => void history.reload()} />}
          {history.data?.samples.length ? <MetricChart samples={history.data.samples} interval={history.data.sample_interval_seconds} /> : !history.error && <Unavailable title={history.loading ? translate("正在读取监控历史…") : translate("暂无历史样本")} detail={translate("采样器会保存真实主机数据；没有采样的时段不会补造数据。")} />}
        </Card>
      )}
      {page==="monitor" && has("monitor_alerts_list") && changeOpen==="alerts" && <ResourceDrawer open title={translate("监控告警")} onClose={()=>setChangeOpen("")}><MonitorAlerts capabilities={capabilities}/></ResourceDrawer>}
      {((page === "monitor" && has("services_list")) ||
        (page === "schedules" && pageTabs.length > 0)) && (
        <Card className="panel-card">
          <SectionHeader icon="monitor" className="card-heading">
            <h2>{title}</h2>
            {activeTab !== "services" && create && (
              <Button
                label={
                  activeTab === "notifications"
                    ? translate("新增通知渠道")
                    : translate("创建计划任务")
                }
                variant="primary"
                onClick={() => setCreating(true)}
              />
            )}
          </SectionHeader>
          {page === "schedules" && pageTabs.length > 1 && (
            <ResourceTabs
              aria-label={translate("自动化分类")}
            >
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
            </ResourceTabs>
          )}
          {records.error && (
            <ResourceError error={records.error} state={records.state} retry={() => void records.reload()} />
          )}
          {!has(name) ? (
            <Unavailable title={translate("{0}服务暂不可用", [title])} />
          ) : (
            <>
              <ClientTable>
                <table className={`resource-table operations-table ${activeTab}`}>
                  <thead>
                    <tr>
                      <th>{translate("名称")}</th>
                      {(activeTab === "services"
                        ? ["unit", "active_state", "ownership"]
                        : activeTab === "notifications"
                          ? ["kind"]
                          : ["enabled", "next_run_at", "last_run_status"]
                      ).map((k) => (
                        <th key={k} className={`operation-${k}`}>{descriptions[k] ?? k}</th>
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
                        </td>
                        {(activeTab === "services"
                          ? ["unit", "active_state", "ownership"]
                          : activeTab === "notifications"
                            ? ["kind"]
                            : ["enabled", "next_run_at", "last_run_status"]
                        ).map((k) => (
                          <td key={k} className={`operation-${k}`}>
                            {k === "active_state" || k === "last_run_status" ? (
                              <State value={(k === "active_state" ? row[k] === "active" ? "running" : row[k] === "inactive" ? "stopped" : row[k] : row[k]) as string} />
                            ) : (
                              cell(k, row[k])
                            )}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ClientTable>
              {!rows.length && !records.loading && !records.error && (
                <Unavailable
                  title={translate("暂无{0}", [title])}
                  detail={
                    activeTab === "services"
                      ? translate("仅列出受信任主机策略中明确管理的服务。")
                      : translate("创建后由网页与MCP共同管理。")
                  }
                />
              )}
            </>
          )}
        </Card>
      )}
      {page === "schedules" && !pageTabs.length && (
        <Unavailable title={translate("当前没有可用的计划任务或通知能力")} />
      )}
      {page === "monitor" &&
        has("system_change_plan") &&
        has("system_change_get") && (
          <>
            <Button label={translate("管理系统变更")} onClick={() => setChangeOpen("system-changes")} />
            <ResourceDrawer open={changeOpen === "system-changes"} title={translate("系统变更与恢复")} onClose={() => setChangeOpen("")}>
              <SystemChangeWorkflow capabilities={capabilities} identityKey={identityKey} onExecuted={refresh} />
            </ResourceDrawer>
          </>
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
          <details>
            <summary>{translate("技术详情")}</summary>
            <code>
              {current.id} · {current.revision ?? "—"}
            </code>
          </details>
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
          title={
            activeTab === "notifications"
              ? translate("新增通知渠道")
              : translate("创建计划任务")
          }
          onClose={() => setCreating(false)}
        >
          <ActionForm capability={create} onExecuted={refresh} />
        </ResourceDrawer>
      )}
      <details className="advanced-actions">
        <summary>
          {page === "monitor"
            ? translate("服务与系统变更计划")
            : translate("自动化操作与执行历史")}
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

function ScheduleHistory({
  id,
  capabilities,
}: {
  id: string;
  capabilities: Capability[];
}) {
  type Run = {
    id: string;
    started_at: string;
    status: string;
    task_id?: string;
    error_code?: string;
    notification_state?: string;
  };
  const history = useResource<Run[]>("schedules_history", { id }, true, 3000);
  const [taskID, setTaskID] = useState<string>();
  return (
    <section className="stack">
      <h3>{translate("最近执行")}</h3>
      {history.error && (
        <ResourceError error={history.error} state={history.state} retry={() => void history.reload()} />
      )}
      {history.data?.map((run) => (
        <div key={run.id} className="resource-summary">
          <span>{fullDate(run.started_at)}</span>
          <State value={run.status} />
          {run.error_code && <code>{run.error_code}</code>}
          {run.notification_state && (
            <span>
              {translate("失败通知：")}
              {run.notification_state === "submitted"
                ? translate("已提交")
                : run.notification_state === "failed"
                  ? translate("发送失败")
                  : translate("发送中或待检查")}
            </span>
          )}
          {run.task_id && capabilities.some((c) => c.name === "tasks_get") && (
            <Button
              label={translate("查看关联任务")}
              onClick={() => setTaskID(run.task_id)}
            />
          )}
        </div>
      ))}
      {taskID && (
        <TaskResult
          key={taskID}
          submitted={{ task_id: taskID }}
          capabilities={capabilities}
          onFinished={() => void history.reload()}
        />
      )}
    </section>
  );
}
