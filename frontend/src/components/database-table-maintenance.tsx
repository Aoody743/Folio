"use client";
import { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { action, APIError, bytes, Capability, fullDate, intentKey } from "@/lib/api";
import {
  databaseTableCanMaintain,
  databaseMaintenanceHasUnknownOutcome,
  databaseTableInspectInput,
  databaseTableMaintenanceCapabilities,
  databaseTablePreviewInput,
  databaseTableRunInput,
  DatabaseMaintenanceInspection,
  DatabaseMaintenanceOperation,
  DatabaseMaintenancePage,
  DatabaseMaintenancePreview,
  DatabaseMaintenanceTable,
  parseDatabaseMaintenanceInspection,
  parseDatabaseMaintenancePage,
  parseDatabaseMaintenancePreview,
} from "@/lib/database-table-maintenance";
import { useResource } from "@/lib/resource-hook";
import { State, Unavailable } from "./resource-ui";
import { TaskResult } from "./task-result";

type RunAttempt = {
  input: Record<string, unknown>;
  key: string;
  state: "submitting" | "uncertain" | "accepted";
};

function supportedEngine(engine: string): boolean {
  return ["mysql", "mysql-community", "mariadb"].includes(engine.toLowerCase());
}

function tableSize(table: DatabaseMaintenanceTable): string {
  if (!table.estimate_known) return "大小估算未知";
  return bytes((table.data_bytes ?? 0) + (table.index_bytes ?? 0));
}

function inspectionWarning(result: DatabaseMaintenanceInspection): string | undefined {
  if (databaseMaintenanceHasUnknownOutcome(result)) {
    return "至少一个表的服务端结果无法确认。请勿自动重试或再次提交；先核实数据库状态。";
  }
  if (result.state === "partial") {
    return "任务部分完成。逐表检查结果后，再决定是否为未完成的表创建新的预览。";
  }
  if (result.state === "expired") return "此预览已过期，不能用于提交维护任务。";
  return undefined;
}

export function DatabaseTableMaintenance({
  instanceID,
  database,
  engine,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  database: string;
  engine: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const [page, setPage] = useState<DatabaseMaintenancePage>();
  const [cursorStack, setCursorStack] = useState<string[]>([""]);
  const [cursorIndex, setCursorIndex] = useState(0);
  const [selected, setSelected] = useState<Record<string, DatabaseMaintenanceTable>>({});
  const [operation, setOperation] = useState<DatabaseMaintenanceOperation>("optimize");
  const [listBusy, setListBusy] = useState(false);
  const [listError, setListError] = useState("");
  const [preview, setPreview] = useState<DatabaseMaintenancePreview>();
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [now, setNow] = useState(Date.now());
  const [runAttempt, setRunAttempt] = useState<RunAttempt>();
  const [runBusy, setRunBusy] = useState(false);
  const [runError, setRunError] = useState("");
  const [task, setTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const [taskOutcomeUnknown, setTaskOutcomeUnknown] = useState(false);
  const [inspectID, setInspectID] = useState("");
  const [inspection, setInspection] = useState<DatabaseMaintenanceInspection>();
  const [inspectBusy, setInspectBusy] = useState(false);
  const [inspectError, setInspectError] = useState("");
  const inventoryAnchor = useRef<Pick<DatabaseMaintenancePage, "schema_sha256" | "instance_revision" | "database_revision"> | undefined>(undefined);
  const inspectRef = useRef<(id: string, taskFinished?: boolean) => Promise<void>>(async () => {});
  const actions = useMemo(
    () => databaseTableMaintenanceCapabilities(capabilities),
    [capabilities],
  );
  const canMaintainEngine = supportedEngine(engine);
  const selectedTables = Object.values(selected);
  const hasUnknownOutcome = databaseMaintenanceHasUnknownOutcome(inspection);
  const previewExpired = !!preview && Date.parse(preview.expires_at) <= now;

  useEffect(() => {
    if (!preview) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [preview]);

  const clearPreview = () => {
    setPreview(undefined);
    setConfirmed(false);
    setPreviewError("");
    setRunError("");
    setRunAttempt(undefined);
  };

  const resetForFreshInventory = () => {
    inventoryAnchor.current = undefined;
    setPage(undefined);
    setCursorStack([""]);
    setCursorIndex(0);
    setSelected({});
    clearPreview();
  };

  const loadPage = async (
    after: string,
    mode: "fresh" | "next" | "previous",
  ): Promise<void> => {
    if (!actions.list || listBusy || !active ||
        (mode === "fresh" && (taskPending || taskOutcomeUnknown || runAttempt?.state === "uncertain"))) return;
    setListBusy(true);
    setListError("");
    if (mode === "fresh") resetForFreshInventory();
    try {
      const input = {
        instance_id: instanceID,
        database,
        limit: 200,
        ...(after ? { after } : {}),
      };
      const raw = await action<unknown>(actions.list.name, input);
      const next = parseDatabaseMaintenancePage(raw, instanceID, database);
      if (!next) throw new Error("表清单或快照版本字段无效；请刷新后再选择表。");
      const anchor = inventoryAnchor.current;
      if (mode !== "fresh" && anchor && (
        anchor.schema_sha256 !== next.schema_sha256 ||
        anchor.instance_revision !== next.instance_revision ||
        anchor.database_revision !== next.database_revision
      )) {
        resetForFreshInventory();
        throw new Error("分页期间数据库或表结构已变化；已清除选择，请从第一页重新读取。");
      }
      if (mode === "fresh") {
        inventoryAnchor.current = {
          schema_sha256: next.schema_sha256,
          instance_revision: next.instance_revision,
          database_revision: next.database_revision,
        };
        setCursorStack([""]);
        setCursorIndex(0);
      } else if (mode === "next") {
        setCursorStack((current) => [...current.slice(0, cursorIndex + 1), after]);
        setCursorIndex((current) => current + 1);
      } else {
        setCursorIndex((current) => Math.max(0, current - 1));
      }
      setPage(next);
    } catch (error) {
      if (mode !== "fresh" && error instanceof APIError &&
          ["stale_page_cursor", "revision_conflict"].includes(error.code)) {
        resetForFreshInventory();
        setListError("数据库清单分页已过期；选择已清除。请重新读取第一页。");
      } else {
        setListError(error instanceof Error ? error.message : "读取表清单失败");
      }
    } finally {
      setListBusy(false);
    }
  };
  const loadPageRef = useRef(loadPage);
  loadPageRef.current = loadPage;

  useEffect(() => {
    if (expanded && active && canMaintainEngine && actions.list) {
      void loadPageRef.current("", "fresh");
    }
  }, [expanded, active, canMaintainEngine, actions.list?.name, instanceID, database]);

  const inspect = async (id: string, taskFinished = false) => {
    if (!actions.inspect || !active) return;
    const input = databaseTableInspectInput(instanceID, database, id);
    if (!input) {
      setInspectError("请输入有效的表维护预览 ID。");
      return;
    }
    setInspectBusy(true);
    setInspectError("");
    try {
      const raw = await action<unknown>(actions.inspect.name, input);
      const next = parseDatabaseMaintenanceInspection(raw, instanceID, database, id);
      if (!next) throw new Error("维护检查结果与此数据库或预览不匹配。");
      setInspection(next);
      setInspectID(id);
      if (taskFinished) {
        setTaskOutcomeUnknown(databaseMaintenanceHasUnknownOutcome(next));
        setTaskPending(false);
      } else if (databaseMaintenanceHasUnknownOutcome(next)) {
        setTaskOutcomeUnknown(true);
      } else if (["expired", "completed", "partial", "failed", "canceled"].includes(next.state)) {
        setTaskOutcomeUnknown(false);
      }
    } catch (error) {
      setInspectError(error instanceof Error ? error.message : "检查维护结果失败");
    } finally {
      setInspectBusy(false);
    }
  };
  inspectRef.current = inspect;

  const makePreview = async () => {
    if (!actions.preview || !page || !active || taskPending || taskOutcomeUnknown ||
        runAttempt?.state === "submitting" || runAttempt?.state === "uncertain") return;
    const input = databaseTablePreviewInput(page, instanceID, database, operation, selectedTables);
    if (!input) {
      setPreviewError("选择的表不符合当前操作限制；请仅选择清单中的基础表，每次最多 25 张。");
      return;
    }
    setPreviewBusy(true);
    setPreviewError("");
    clearPreview();
    setInspection(undefined);
    setInspectError("");
    setInspectID("");
    try {
      const raw = await action<unknown>(actions.preview.name, input);
      const parsed = parseDatabaseMaintenancePreview(raw, {
        page,
        engine,
        operation,
        tables: selectedTables,
      });
      if (!parsed) throw new Error("服务端预览缺少匹配的表、版本、哈希或影响说明；没有提交维护任务。");
      setPreview(parsed);
      setInspectID(parsed.preview_id);
      setNow(Date.now());
    } catch (error) {
      setPreviewError(error instanceof Error ? error.message : "创建表维护预览失败");
    } finally {
      setPreviewBusy(false);
    }
  };

  const run = async (replaySameRequest = false) => {
    if (!actions.run || !preview || !active || taskPending || runBusy ||
        (taskOutcomeUnknown && !(replaySameRequest && runAttempt?.state === "uncertain"))) return;
    const previous = runAttempt;
    const input = previous?.input ?? databaseTableRunInput(preview, confirmed, now);
    if (!input) {
      setRunError("预览已过期或尚未确认维护影响；请读取新清单并生成新预览。");
      return;
    }
    if (previous && (!replaySameRequest || previous.state !== "uncertain")) return;
    const attempt: RunAttempt = previous ?? { input, key: intentKey(), state: "submitting" };
    setRunAttempt({ ...attempt, state: "submitting" });
    setRunBusy(true);
    setRunError("");
    try {
      const accepted = await action<unknown>(actions.run.name, attempt.input, {
        idempotencyKey: attempt.key,
      });
      setTask(accepted);
      setTaskPending(true);
      setTaskOutcomeUnknown(false);
      setRunAttempt({ ...attempt, state: "accepted" });
      setRunError("");
      setInspectID(preview.preview_id);
      onExecuted();
    } catch (error) {
      setRunAttempt({ ...attempt, state: "uncertain" });
      setTaskOutcomeUnknown(true);
      setRunError(error instanceof Error ? error.message : "服务未确认任务回执；结果未知。");
    } finally {
      setRunBusy(false);
    }
  };

  const toggleSelection = (table: DatabaseMaintenanceTable, checked: boolean) => {
    setSelected((current) => {
      if (checked) {
        if (current[table.name] || Object.keys(current).length >= 25) return current;
        return { ...current, [table.name]: table };
      }
      const next = { ...current };
      delete next[table.name];
      return next;
    });
  };
  const operationChanged = (value: DatabaseMaintenanceOperation) => {
    setOperation(value);
    setSelected({});
    clearPreview();
  };
  const canSelect = !!actions.preview && !taskPending && !taskOutcomeUnknown &&
    runAttempt?.state !== "submitting" && runAttempt?.state !== "uncertain" && !previewBusy;
  const canPreview = !!databaseTablePreviewInput(page, instanceID, database, operation, selectedTables) &&
    !!actions.preview && !previewBusy && !listBusy && !taskPending && !taskOutcomeUnknown &&
    runAttempt?.state !== "submitting" && runAttempt?.state !== "uncertain";
  const canRunInput = databaseTableRunInput(preview, confirmed, now);
  const tablePageNumber = cursorIndex + 1;

  return (
    <details
      className="stack"
      aria-label="数据库表维护"
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary>
        表优化与修复 · {canMaintainEngine ? "MySQL / MariaDB" : "当前引擎暂不支持"}
      </summary>
      {!canMaintainEngine ? (
        <Unavailable
          title="表维护仅支持托管 MySQL / MariaDB"
          detail="PostgreSQL、MongoDB 及其他数据库引擎没有此操作能力。"
        />
      ) : (
        <section className="stack" aria-label="表维护清单与预览">
          <p className="muted">
            仅处理本面板拥有的数据库基础表；一次选择不超过 25 张。清单读取使用数据库读取授权，预览和执行分别需要数据库管理授权。
          </p>
          {!actions.list ? (
            <Unavailable title="没有读取表清单的权限或服务未公布此能力" detail="需要 managed_database_tables_list 的 databases:read 授权。" />
          ) : (
            <>
              {listError && <div className="notice danger" role="alert">{listError}</div>}
              <div className="button-row">
                <Button
                  label={page ? "重新读取第一页" : "读取表清单"}
                  isDisabled={listBusy || !active || taskPending || taskOutcomeUnknown || runAttempt?.state === "uncertain"}
                  onClick={() => void loadPage("", "fresh")}
                />
                {page && (
                  <span className="muted">
                    第 {tablePageNumber} 页 · {page.items.length} / {page.total} 张表 · 修订 {page.instance_revision}/{page.database_revision}
                  </span>
                )}
              </div>
              {page && (
                <>
                  {page.replication_source_attached && (
                    <div className="notice warning">
                      此数据库属于受管复制源；维护语句可能传播到副本并增加复制延迟、锁等待和磁盘负载。
                    </div>
                  )}
                  {!actions.preview && (
                    <div className="notice">
                      当前身份可以读取和检查结果，但没有表维护预览管理权限；无法提交优化或修复。
                    </div>
                  )}
                  <label className="native-field">
                    操作
                    <select
                      value={operation}
                      disabled={!canSelect}
                      onChange={(event) => operationChanged(event.target.value as DatabaseMaintenanceOperation)}
                    >
                      <option value="optimize">优化表（InnoDB / MyISAM）</option>
                      <option value="repair">修复表（仅 MyISAM）</option>
                    </select>
                  </label>
                  {operation === "repair" && (
                    <div className="notice warning">
                      REPAIR 仅对 MyISAM 提供有限检查；可能锁表，也不能保证恢复损坏数据。InnoDB 不支持此命令。
                    </div>
                  )}
                  <div className="instance-list" aria-label="数据库基础表">
                    {page.items.map((table) => {
                      const supported = databaseTableCanMaintain(table, operation);
                      const reason = table.type !== "BASE TABLE"
                        ? "仅支持基础表"
                        : operation === "repair" && table.engine.toLowerCase() === "innodb"
                          ? "InnoDB 不支持 REPAIR；不会发送修复语句"
                          : "当前存储引擎不在支持范围";
                      return (
                        <div className="instance-row" key={table.name}>
                          <label className="native-field">
                            <input
                              type="checkbox"
                              checked={!!selected[table.name]}
                              disabled={!canSelect || !supported || (!selected[table.name] && selectedTables.length >= 25)}
                              onChange={(event) => toggleSelection(table, event.target.checked)}
                            />
                            <strong>{table.name}</strong>
                          </label>
                          <div>
                            <span>{table.engine || table.type}</span>
                            <span className="muted"> · {tableSize(table)}</span>
                            {table.type !== "BASE TABLE" && <small className="muted">{reason}</small>}
                            {table.type === "BASE TABLE" && !supported && <small className="muted">{reason}</small>}
                          </div>
                        </div>
                      );
                    })}
                    {!page.items.length && <Unavailable title="此数据库暂无基础表" />}
                  </div>
                  <div className="pagination">
                    <span className="muted">
                      已选 {selectedTables.length} / 25 · 结构指纹 <code title={page.schema_sha256}>{page.schema_sha256.slice(0, 12)}…</code>
                    </span>
                    <div className="button-row">
                      <Button
                        label="上一页"
                        isDisabled={listBusy || cursorIndex === 0}
                        onClick={() => void loadPage(cursorStack[cursorIndex - 1] ?? "", "previous")}
                      />
                      <Button
                        label="下一页"
                        isDisabled={listBusy || !page.truncated || !page.next_after}
                        onClick={() => page.next_after && void loadPage(page.next_after, "next")}
                      />
                      <Button
                        label="生成维护预览"
                        variant="primary"
                        isDisabled={!canPreview}
                        onClick={() => void makePreview()}
                      />
                    </div>
                  </div>
                  {selectedTables.length > 0 && selectedTables.some((table) => !databaseTableCanMaintain(table, operation)) && (
                    <div className="notice warning">当前所选表不全支持此操作；InnoDB 的 REPAIR 会被禁用，服务端也会在预览时重新校验引擎。</div>
                  )}
                </>
              )}
            </>
          )}

          {previewError && <div className="notice danger" role="alert">{previewError}</div>}
          {previewBusy && <p role="status">正在固定表定义、版本和影响预览…</p>}
          {preview && (
            <div className="stack" aria-label="表维护预览">
              <div className="resource-summary">
                <strong>{preview.operation === "optimize" ? "优化预览" : "MyISAM 修复预览"}</strong>
                <State value={previewExpired ? "expired" : "ready"} />
              </div>
              <div className="detail-grid">
                <div><small>目标</small><strong>{preview.database}</strong><span>{preview.table_count} 张表</span></div>
                <div><small>预览有效期</small><strong>{fullDate(preview.expires_at)}</strong></div>
                <div><small>预估表数据和索引</small><strong>{preview.estimate_known ? bytes(preview.estimated_bytes) : "估算未知"}</strong></div>
                <div><small>版本钉住</small><strong>{preview.instance_revision} / {preview.database_revision}</strong></div>
              </div>
              <p><strong>锁影响：</strong>{preview.lock_impact}</p>
              {preview.may_need_comparable_temporary_space && (
                <div className="notice warning">优化可能重建表并暂用接近当前表大小的空间；估算值不保证可回收空间，执行前确认磁盘余量。</div>
              )}
              <p><strong>复制影响：</strong>{preview.replica_impact}</p>
              {preview.warnings.map((warning, index) => <div className="notice warning" key={`${index}:${warning}`}>{warning}</div>)}
              <div className="instance-list">
                {preview.tables.map((table) => (
                  <div className="instance-row" key={table.name}>
                    <div><strong>{table.name}</strong><span className="muted"> · {table.engine}</span></div>
                    <span>{tableSize(table)}</span>
                  </div>
                ))}
              </div>
              {!actions.run ? (
                <Unavailable title="当前身份没有执行表维护任务的权限" detail="执行需要 managed_database_table_maintenance 的 databases:admin 授权。" />
              ) : (
                <>
                  <label className="native-field">
                    <input
                      type="checkbox"
                      checked={confirmed}
                      disabled={previewExpired || taskPending || taskOutcomeUnknown || runAttempt?.state === "accepted"}
                      onChange={(event) => setConfirmed(event.target.checked)}
                    />
                    我已查看上述表、锁、空间和复制影响，并确认执行此固定预览。
                  </label>
                  {previewExpired && <div className="notice warning">预览已过期；刷新清单并创建新预览后才能执行。</div>}
                  {runError && <div className="notice danger" role="alert">{runError}</div>}
                  {runAttempt?.state === "uncertain" && (
                    <div className="notice warning">
                      服务没有返回可确认的任务回执，结果未知。先检查预览状态；不会自动重试。若选择重放，只会使用相同请求键以读取同一任务回执。
                      <div className="button-row">
                        <Button label="检查此预览结果" variant="ghost" isDisabled={!actions.inspect || inspectBusy} onClick={() => void inspect(preview.preview_id)} />
                        <Button label="用相同请求键重放" isDisabled={runBusy} onClick={() => void run(true)} />
                      </div>
                    </div>
                  )}
                  {runAttempt?.state === "accepted" && (
                    <div className="notice">此预览已提交为一次性任务；不会再次提交。请检查任务和逐表结果。</div>
                  )}
                  {!runAttempt && (
                    <Button
                      label={runBusy ? "正在提交…" : "提交维护任务"}
                      variant="destructive"
                      isDisabled={!canRunInput || runBusy || taskPending || taskOutcomeUnknown}
                      onClick={() => void run()}
                    />
                  )}
                </>
              )}
            </div>
          )}

          {task !== undefined && (
            <TaskResult
              submitted={task}
              capabilities={capabilities}
              onFinished={() => {
                setTaskPending(false);
                if (preview) void inspectRef.current?.(preview.preview_id, true);
              }}
              onObservationUnavailable={() => {
                setTaskPending(false);
                setTaskOutcomeUnknown(true);
                if (preview) void inspectRef.current?.(preview.preview_id);
              }}
            />
          )}

          <details>
            <summary>检查已有预览结果</summary>
            {!actions.inspect ? (
              <Unavailable title="当前身份没有维护结果读取权限" detail="检查需要 managed_database_table_maintenance_inspect 的 databases:read 授权。" />
            ) : (
              <div className="stack">
                <label className="native-field">
                  预览 ID
                  <input
                    value={inspectID}
                    maxLength={53}
                    placeholder="plan_…"
                    onChange={(event) => setInspectID(event.target.value.trim())}
                  />
                </label>
                <div className="button-row">
                  <Button
                    label={inspectBusy ? "正在检查…" : "读取受保护结果"}
                    isDisabled={inspectBusy || !databaseTableInspectInput(instanceID, database, inspectID)}
                    onClick={() => void inspect(inspectID)}
                  />
                </div>
                {inspectError && <div className="notice danger" role="alert">{inspectError}</div>}
                {inspection && (
                  <div className="stack">
                    <div className="resource-summary">
                      <strong>{inspection.operation === "optimize" ? "优化" : "修复"} · {inspection.database}</strong>
                      <State value={inspection.state} />
                    </div>
                    <p className="muted">
                      预览 {inspection.preview_id}
                      {inspection.task_id ? ` · 任务 ${inspection.task_id}` : " · 尚无已记录任务"}
                      {inspection.finished_at ? ` · 完成于 ${fullDate(inspection.finished_at)}` : ""}
                    </p>
                    <p>已完成 {inspection.completed_count} · 失败 {inspection.failed_count} · 结果未知 {inspection.outcome_unknown_count}</p>
                    {inspectionWarning(inspection) && <div className="notice warning" role="alert">{inspectionWarning(inspection)}</div>}
                    {inspection.items.map((item) => (
                      <div className="instance-row" key={item.name}>
                        <div><strong>{item.name}</strong><span className="muted"> · {item.engine}</span></div>
                        <State value={item.state} />
                        {item.messages.map((message, index) => (
                          <p className={message.msg_type === "error" ? "danger-text" : "muted"} key={`${index}:${message.msg_type}`}>
                            {message.msg_type}: {message.msg_text}
                          </p>
                        ))}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
          </details>
        </section>
      )}
    </details>
  );
}
