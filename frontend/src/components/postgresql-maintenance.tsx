"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, Capability, fullDate } from "@/lib/api";
import {
  parsePostgreSQLAccessList,
  parsePostgreSQLSlowlogRead,
  parsePostgreSQLSlowlogStatus,
  postgresqlAccessMutationInput,
  postgresqlCapability,
  postgresqlSlowlogInput,
  postgresqlSlowlogMayConfigure,
} from "@/lib/postgresql-maintenance";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { State, Unavailable } from "./resource-ui";

const accessFields = [
  "instance_id",
  "database",
  "expected_config_revision",
  "expected_instance_revision",
  "expected_database_revision",
];
const slowRevisionFields = [
  "instance_id",
  "database",
  "expected_config_revision",
  "expected_instance_revision",
  "expected_database_revision",
];
const finalTaskStates = new Set([
  "succeeded",
  "failed",
  "canceled",
  "cancelled",
  "interrupted",
  "completed",
]);
const isTaskPending = (value: unknown) => {
  if (!value || typeof value !== "object" || Array.isArray(value)) return true;
  const task = value as Record<string, unknown>;
  return !finalTaskStates.has(String(task.status ?? task.state ?? ""));
};

export function PostgreSQLMaintenance({
  instanceID,
  database,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  database: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const cap = (
    name: string,
    scope: "databases:read" | "databases:admin",
    fields: string[],
    task: boolean,
    readOnly: boolean,
  ) => postgresqlCapability(capabilities, name, scope, fields, task, readOnly);
  const accessRead = cap(
    "managed_postgresql_access_rules_list",
    "databases:read",
    ["instance_id", "database"],
    false,
    true,
  );
  const accessAdd = cap(
    "managed_postgresql_access_rule_add",
    "databases:admin",
    [...accessFields, "cidr", "auth_method"],
    true,
    false,
  );
  const accessRemove = cap(
    "managed_postgresql_access_rule_remove",
    "databases:admin",
    [...accessFields, "rule_id"],
    true,
    false,
  );
  const accessRecover = cap(
    "managed_postgresql_access_recover",
    "databases:admin",
    [...accessFields, "recovery_choice"],
    true,
    false,
  );
  const slowStatusCapability = cap(
    "managed_postgresql_slowlog_status",
    "databases:read",
    ["instance_id", "database"],
    false,
    true,
  );
  const slowConfigure = cap(
    "managed_postgresql_slowlog_configure",
    "databases:admin",
    [...slowRevisionFields, "enabled", "minimum_duration_ms"],
    true,
    false,
  );
  const slowReadCapability = cap(
    "managed_postgresql_slowlog_read",
    "databases:read",
    ["instance_id", "database"],
    false,
    true,
  );
  const slowRecover = cap(
    "managed_postgresql_slowlog_recover",
    "databases:admin",
    [...slowRevisionFields, "recovery_choice"],
    true,
    false,
  );
  const anyRelated = capabilities.some(
    (item) =>
      item.name.startsWith("managed_postgresql_access_") ||
      item.name.startsWith("managed_postgresql_slowlog_"),
  );
  const accessResource = useResource<unknown>(
    "managed_postgresql_access_rules_list",
    { instance_id: instanceID, database },
    active && !!accessRead,
    15000,
  );
  const slowStatusResource = useResource<unknown>(
    "managed_postgresql_slowlog_status",
    { instance_id: instanceID, database },
    active && !!slowStatusCapability,
    15000,
  );
  const slowReadResource = useResource<unknown>(
    "managed_postgresql_slowlog_read",
    { instance_id: instanceID, database, limit: 50 },
    active && !!slowReadCapability,
    30000,
  );
  const access = parsePostgreSQLAccessList(
    accessResource.data,
    instanceID,
    database,
  );
  const slowStatus = parsePostgreSQLSlowlogStatus(
    slowStatusResource.data,
    instanceID,
    database,
  );
  const slowlog = parsePostgreSQLSlowlogRead(
    slowReadResource.data,
    instanceID,
    database,
  );
  const [cidr, setCidr] = useState("127.0.0.1/32");
  const [duration, setDuration] = useState("1000");
  const [busy, setBusy] = useState(false);
  const [taskActive, setTaskActive] = useState(false);
  const [error, setError] = useState("");
  const [task, setTask] = useState<unknown>();
  const refresh = () => {
    void accessResource.reload();
    void slowStatusResource.reload();
    void slowReadResource.reload();
    onExecuted();
  };
  const submit = async (
    capability: Capability | undefined,
    input: Record<string, unknown> | undefined,
    confirmation: string,
  ) => {
    if (!capability || !input || busy || taskActive) return;
    if (!window.confirm(confirmation)) return;
    setBusy(true);
    setError("");
    setTask(undefined);
    try {
      const accepted = await action<unknown>(capability.name, input);
      setTask(accepted);
      setTaskActive(isTaskPending(accepted));
      refresh();
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message
          : translate("操作失败。请重新读取当前状态。"),
      );
      refresh();
    } finally {
      setBusy(false);
    }
  };

  if (!anyRelated) return null;
  return (
    <Card className="panel-card">
      <SectionHeader icon="settings" className="card-heading">
        <div>
          <h3>{translate("PostgreSQL 访问规则与慢查询")}</h3>
          <p className="muted">
            {translate(
              "仅管理当前记录的数据库与角色；变更按配置和所有权版本提交。",
            )}
          </p>
        </div>
        <Button
          label={translate("刷新")}
          variant="ghost"
          onClick={refresh}
          isDisabled={busy}
        />
      </SectionHeader>
      {(error ||
        accessResource.error ||
        slowStatusResource.error ||
        slowReadResource.error) && (
        <div className="notice danger" role="alert">
          {error ||
            accessResource.error ||
            slowStatusResource.error ||
            slowReadResource.error}
        </div>
      )}
      <section className="stack" aria-label={translate("PostgreSQL访问规则")}>
        <SectionHeader icon="settings" className="card-heading">
          <div>
            <h4>{translate("数据库访问规则")}</h4>
            <p className="muted">
              {translate(
                "只允许 loopback 客户端 CIDR。远程访问因未配置 TLS 而不可用。",
              )}
            </p>
          </div>
          {access && <State value={access.configuration_state} />}
        </SectionHeader>
        {!accessRead ? (
          <Unavailable
            title={translate("当前没有 PostgreSQL 访问规则读取权限")}
          />
        ) : accessResource.loading && !access ? (
          <p role="status">{translate("正在读取受管理的访问规则…")}</p>
        ) : access ? (
          <>
            <p className="muted">
              {translate("角色")}
              <code>
                {access.rules[0]?.role ?? translate("当前数据库的受管理角色")}
              </code>
              {translate("· 本地管理员规则受到保护。")}
            </p>
            <div className="instance-list">
              {access.rules.map((rule) => (
                <div className="instance-row" key={rule.rule_id}>
                  <div>
                    <strong>{rule.cidr}</strong>
                    <p>
                      {rule.role}· {rule.auth_method}
                    </p>
                    <small className="monospace">{rule.rule_id}</small>
                  </div>
                  <div className="button-row">
                    <State value={rule.state} />
                    {rule.state === "allowed" && accessRemove && (
                      <Button
                        label={translate("阻止此规则")}
                        variant="ghost"
                        isDisabled={
                          busy ||
                          taskActive ||
                          access.configuration_state === "pending_recovery"
                        }
                        onClick={() =>
                          void submit(
                            accessRemove,
                            postgresqlAccessMutationInput(
                              access,
                              instanceID,
                              database,
                              "remove",
                              rule.rule_id,
                            ),
                            translate(
                              "确认阻止 {0} 对此数据库角色的访问？该变更会保留为精确拒绝规则。",
                              [rule.cidr],
                            ),
                          )
                        }
                      />
                    )}
                  </div>
                </div>
              ))}
            </div>
            {!access.rules.length && (
              <p className="muted">
                {translate("当前没有额外的 loopback 访问规则。")}
              </p>
            )}
            {access.configuration_state === "pending_recovery" ? (
              <div className="notice warning">
                {translate(
                  "访问配置有未完成的恢复记录。先检查当前摘要，再明确选择应用待处理变更或恢复上一版本。",
                )}
                <div className="button-row">
                  {(["apply_pending", "restore_previous"] as const).map(
                    (choice) => (
                      <Button
                        key={choice}
                        label={
                          choice === "apply_pending"
                            ? translate("应用待处理变更")
                            : translate("恢复上一版本")
                        }
                        isDisabled={busy || taskActive || !accessRecover}
                        onClick={() =>
                          void submit(
                            accessRecover,
                            postgresqlAccessMutationInput(
                              access,
                              instanceID,
                              database,
                              "recover",
                              choice,
                            ),
                            choice === "apply_pending"
                              ? translate("确认应用待处理的访问规则配置？")
                              : translate("确认恢复到上一版本的访问规则配置？"),
                          )
                        }
                      />
                    ),
                  )}
                </div>
              </div>
            ) : accessAdd ? (
              <div className="button-row">
                <TextInput
                  label="Loopback CIDR"
                  value={cidr}
                  onChange={setCidr}
                  autoComplete="off"
                  description={translate(
                    "IPv4 使用规范网络地址（127.0.0.0/8 至 /32）；IPv6 仅支持 ::1/128。",
                  )}
                />
                <Button
                  label={translate("添加 SCRAM 访问规则")}
                  isDisabled={
                    busy ||
                    taskActive ||
                    !postgresqlAccessMutationInput(
                      access,
                      instanceID,
                      database,
                      "add",
                      cidr,
                    )
                  }
                  onClick={() =>
                    void submit(
                      accessAdd,
                      postgresqlAccessMutationInput(
                        access,
                        instanceID,
                        database,
                        "add",
                        cidr,
                      ),
                      translate("确认允许 {0} 使用 SCRAM-SHA-256 访问 {1}？", [
                        cidr.trim(),
                        database,
                      ]),
                    )
                  }
                />
              </div>
            ) : (
              <Unavailable title={translate("当前没有访问规则变更权限")} />
            )}
          </>
        ) : (
          <Unavailable
            title={translate("访问规则响应无效")}
            detail={translate("字段或版本与当前数据库不匹配，未启用变更操作。")}
          />
        )}
      </section>
      <section className="stack" aria-label={translate("PostgreSQL慢查询日志")}>
        <SectionHeader icon="settings" className="card-heading">
          <div>
            <h4>{translate("慢查询日志")}</h4>
            <p className="muted">
              {translate(
                "只调整慢查询阈值；结果是固定日志路径中的有界最新窗口。",
              )}
            </p>
          </div>
          {slowStatus && <State value={slowStatus.configuration_state} />}
        </SectionHeader>
        {!slowStatusCapability ? (
          <Unavailable
            title={translate("当前没有 PostgreSQL 慢查询状态读取权限")}
          />
        ) : slowStatusResource.loading && !slowStatus ? (
          <p role="status">{translate("正在读取实际运行设置…")}</p>
        ) : slowStatus ? (
          <>
            <div className="detail-grid">
              <div>
                <small>{translate("慢查询记录")}</small>
                <strong>
                  {slowStatus.enabled
                    ? translate("已启用")
                    : translate("已关闭")}
                </strong>
              </div>
              <div>
                <small>{translate("触发阈值")}</small>
                <strong>{slowStatus.minimum_duration_ms}ms</strong>
              </div>
              <div>
                <small>{translate("实际重载状态")}</small>
                <strong>
                  {slowStatus.reload_applied
                    ? translate("已应用")
                    : translate("未应用")}
                </strong>
              </div>
            </div>
            {slowStatus.configuration_state === "pending_recovery" ? (
              <div className="notice warning">
                {translate("慢查询配置需要显式恢复。")}
                <div className="button-row">
                  {(["apply_pending", "restore_previous"] as const).map(
                    (choice) => (
                      <Button
                        key={choice}
                        label={
                          choice === "apply_pending"
                            ? translate("应用待处理配置")
                            : translate("恢复上一版本")
                        }
                        isDisabled={busy || taskActive || !slowRecover}
                        onClick={() =>
                          void submit(
                            slowRecover,
                            postgresqlSlowlogInput(
                              slowStatus,
                              "recover",
                              undefined,
                              undefined,
                              choice,
                            ),
                            choice === "apply_pending"
                              ? translate("确认应用待处理的慢查询配置？")
                              : translate("确认恢复上一版本的慢查询配置？"),
                          )
                        }
                      />
                    ),
                  )}
                </div>
              </div>
            ) : slowStatus.configuration_state === "pending_reload" ? (
              <div className="notice warning">
                {translate(
                  "配置已写入但运行状态尚未匹配；请刷新状态后再调整。",
                )}
              </div>
            ) : slowStatus.configuration_state === "unmanaged" ? (
              <div className="notice warning">
                {translate("当前配置不受安全管理，未提供覆盖或恢复操作。")}
              </div>
            ) : slowConfigure && postgresqlSlowlogMayConfigure(slowStatus) ? (
              <div className="button-row">
                <TextInput
                  label={translate("慢查询阈值（毫秒）")}
                  value={duration}
                  onChange={setDuration}
                  autoComplete="off"
                  description={translate("输入整数，范围 1–600000 毫秒。")}
                />
                <Button
                  label={
                    slowStatus.enabled
                      ? translate("更新阈值")
                      : translate("启用慢查询日志")
                  }
                  isDisabled={
                    busy ||
                    taskActive ||
                    !Number.isInteger(Number(duration)) ||
                    Number(duration) < 1 ||
                    Number(duration) > 600000
                  }
                  onClick={() =>
                    void submit(
                      slowConfigure,
                      postgresqlSlowlogInput(
                        slowStatus,
                        "configure",
                        true,
                        Number(duration),
                      ),
                      translate("确认启用慢查询日志，阈值为 {0} 毫秒？", [
                        duration,
                      ]),
                    )
                  }
                />
                {slowStatus.enabled && (
                  <Button
                    label={translate("关闭慢查询日志")}
                    variant="ghost"
                    isDisabled={busy || taskActive}
                    onClick={() =>
                      void submit(
                        slowConfigure,
                        postgresqlSlowlogInput(
                          slowStatus,
                          "configure",
                          false,
                          Math.max(1, slowStatus.minimum_duration_ms),
                        ),
                        translate("确认关闭此数据库的慢查询日志？"),
                      )
                    }
                  />
                )}
              </div>
            ) : (
              <Unavailable title={translate("当前没有慢查询配置权限")} />
            )}
            {slowReadCapability && (
              <div className="stack">
                <SectionHeader icon="settings" className="card-heading">
                  <h5>{translate("最近的慢查询")}</h5>
                  <Button
                    label={translate("刷新日志")}
                    variant="ghost"
                    isDisabled={slowReadResource.loading}
                    onClick={() => void slowReadResource.reload()}
                  />
                </SectionHeader>
                {slowReadResource.error && (
                  <div className="notice danger" role="alert">
                    {slowReadResource.error}
                  </div>
                )}
                {slowReadResource.loading && !slowlog ? (
                  <p role="status">{translate("正在读取日志…")}</p>
                ) : slowlog ? (
                  <>
                    <div className="notice warning">
                      {translate(
                        "慢查询中的字符串与数字字面量、SQL 注释已遮蔽；标识符仍可能包含敏感文字。",
                      )}
                    </div>
                    {!slowlog.log_available ? (
                      <p className="muted">
                        {translate("受管理的日志尚未生成或当前不可读取。")}
                      </p>
                    ) : slowlog.items.length ? (
                      <div className="instance-list">
                        {slowlog.items.map((entry, index) => (
                          <article
                            className="instance-row"
                            key={`${entry.started_at}:${index}`}
                          >
                            <div>
                              <strong>
                                {fullDate(entry.started_at)}·{" "}
                                {entry.duration_ms}ms
                              </strong>
                              <p>
                                {entry.role}· {entry.database}
                              </p>
                              <pre className="log-lines">{entry.statement}</pre>
                            </div>
                          </article>
                        ))}
                      </div>
                    ) : (
                      <p className="muted">
                        {translate("当前窗口没有慢查询记录。")}
                      </p>
                    )}
                  </>
                ) : (
                  <Unavailable
                    title={translate("慢查询日志响应无效")}
                    detail={translate(
                      "未显示字段不完整或不属于当前数据库的内容。",
                    )}
                  />
                )}
                {slowlog?.truncated && (
                  <p className="muted">
                    {translate("列表已截断；这里只显示最近的有界结果。")}
                  </p>
                )}
              </div>
            )}
          </>
        ) : (
          <Unavailable
            title={translate("慢查询状态响应无效")}
            detail={translate(
              "未显示字段或版本与当前数据库不匹配，未启用变更操作。",
            )}
          />
        )}
      </section>
      {task !== undefined && (
        <TaskResult
          key={JSON.stringify(task)}
          submitted={task}
          capabilities={capabilities}
          onFinished={() => {
            setTaskActive(false);
            refresh();
          }}
        />
      )}
    </Card>
  );
}
