"use client";
import { useEffect, useRef, useState } from "react";
import dynamic from "next/dynamic";
const SQLResult = dynamic(
  () => import("./sql-result").then((m) => m.SQLResult),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import {
  action as execute,
  APIError,
  apiErrorMessage,
  Capability,
  intentKey,
  Task,
} from "@/lib/api";
import {
  initialValue,
  applyLockedPreset,
  isDecimalCursorSchema,
  normalizeInput,
  Schema,
} from "@/lib/schema";
import { validateSQLParameters } from "@/lib/sql-wire";
import { Icon } from "./icon";
import { actionLabel, fieldLabel, choiceLabel } from "@/lib/labels";
import { TaskResult } from "./task-result";
import { readDraft, saveDraft, forgetDraft } from "@/lib/draft-cache";
const prefixes: Record<string, RegExp> = {
  sites:
    /^(sites?|websites?|domains?|certificates?|tls|proxy|redirect|php|nginx|apache)_/,
  software:
    /^(plugins?|software|managed_database|managed_databases|database|mysql|postgres|redis|memcached|mongodb|cache|containers?|docker|runtime|php|ftp|rsync|supervisor|mail|ldap)_/,
  files: /^(fs|files?|directories|paths)_/,
  monitor: /^(system|monitor|metrics|process|services)_/,
  terminal: /^(terminal|ssh|commands?)_/,
  schedules: /^(schedules?|cron|tasks|jobs|notifications)_/,
  storage: /^(backups?|storage|cloud)_/,
  security:
    /^(security|firewall|ssh|users?|account|secrets?|settings|applications|temporary_grants)_/,
};
export type References = Record<
  string,
  { value: string; label: string; disabled?: boolean }[]
>;
export type ActionContext = {
  action: string;
  input: { id?: string; site_id?: string };
  task?: Task;
};
type TaskSubmission = {
  submitted: { task_id: string };
  input: ActionContext["input"];
};
export function Field({
  schema,
  value,
  onChange,
  label,
  fieldKey,
  required = false,
  readonlyFields = [],
  references = {},
}: {
  schema: Schema;
  value: unknown;
  onChange: (v: unknown) => void;
  label: string;
  fieldKey: string;
  required?: boolean;
  readonlyFields?: string[];
  references?: References;
}) {
  if (schema.const !== undefined)
    return (
      <div className="readonly-field">
        <span>{label}</span>
        <code>{JSON.stringify(schema.const)}</code>
      </div>
    );
  if (isDecimalCursorSchema(schema))
    return (
      <TextInput
        label={label}
        value={
          typeof value === "string" || typeof value === "number"
            ? String(value)
            : ""
        }
        onChange={onChange}
        isRequired={required}
        autoComplete="off"
        description="输入完整十进制游标；0开始新扫描，响应0表示这一轮结束。"
      />
    );
  if (schema.oneOf || schema.anyOf)
    return (
      <TextArea
        label={label}
        description={
          schema.description ?? "此参数支持联合类型；填写JSON，由服务端验证。"
        }
        rows={4}
        value={
          typeof value === "string" ? value : JSON.stringify(value, null, 2)
        }
        onChange={onChange}
        isRequired={required}
      />
    );
  if (references[fieldKey] && !schema.enum) {
    const choices = references[fieldKey];
    const current = typeof value === "string" ? value : "";
    const listed = choices.some((item) => item.value === current);
    return (
      <label className="native-field">
        {label}
        {required ? " · 必填" : ""}
        <select
          value={listed || current ? current : "__choose_reference__"}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="__choose_reference__" disabled>
            {choices.length ? "请选择资源" : "暂无可见资源"}
          </option>
          {current && !listed && (
            <option value={current}>当前绑定 · {current}</option>
          )}
          {choices.map((item) => (
            <option
              key={item.value}
              value={item.value}
              disabled={item.disabled}
            >
              {item.label}
            </option>
          ))}
        </select>
        {schema.description && <small>{schema.description}</small>}
      </label>
    );
  }
  if (schema.enum)
    return (
      <label className="native-field">
        {label}
        {required ? " · 必填" : ""}
        <select
          value={schema.enum.findIndex(
            (v) => JSON.stringify(v) === JSON.stringify(value),
          )}
          onChange={(e) => onChange(schema.enum![Number(e.target.value)])}
        >
          <option value={-1} disabled>
            {schema.enum.length ? "请选择" : "没有可选值"}
          </option>
          {schema.enum.map((v, i) => (
            <option key={i} value={i}>
              {choiceLabel(v, fieldKey)}
            </option>
          ))}
        </select>
        {schema.description && <small>{schema.description}</small>}
        {fieldKey === "permission" && schema.enum.includes("data_write") && (
          <small>
            只读可查询；数据读写可增删改数据；完整库权限还可修改表结构。可选档位来自服务器，实际授权由数据库验证。
          </small>
        )}
      </label>
    );
  const type = Array.isArray(schema.type)
    ? schema.type.find((t) => t !== "null")
    : schema.type;
  if (readonlyFields.includes(fieldKey))
    return (
      <div className="readonly-field">
        <span>{label}</span>
        <code>{String(value ?? "未读取")}</code>
      </div>
    );
  if (type === "object" || schema.properties) {
    if (!schema.properties || !Object.keys(schema.properties).length)
      return (
        <div
          className={
            schema.writeOnly || /credentials/.test(fieldKey)
              ? "secret-multiline"
              : undefined
          }
        >
          <TextArea
            label={label}
            description={schema.description ?? "JSON对象"}
            value={
              typeof value === "string"
                ? value
                : JSON.stringify(value ?? {}, null, 2)
            }
            rows={4}
            onChange={onChange}
            isRequired={required}
          />
        </div>
      );
    const obj = (
      value && typeof value === "object" && !Array.isArray(value) ? value : {}
    ) as Record<string, unknown>;
    return (
      <fieldset className="nested-fields">
        <legend>{label}</legend>
        {schema.description && <p className="muted">{schema.description}</p>}
        {Object.entries(schema.properties).map(([key, child]) => {
          const mandatory = schema.required?.includes(key) ?? false;
          const present = Object.prototype.hasOwnProperty.call(obj, key);
          const set = (v: unknown) =>
            onChange(Object.assign(Object.create(null), obj, { [key]: v }));
          return (
            <div className="schema-field" key={key}>
              {!mandatory && (
                <label className="check-label optional-field">
                  <input
                    type="checkbox"
                    checked={present}
                    onChange={(e) => {
                      if (e.target.checked) set(initialValue(child));
                      else {
                        const next = Object.assign(Object.create(null), obj);
                        delete next[key];
                        onChange(next);
                      }
                    }}
                  />
                  {fieldLabel(key, child.title)} · 可选
                </label>
              )}
              {(present || mandatory) && (
                <Field
                  schema={child}
                  value={obj[key] ?? initialValue(child)}
                  onChange={set}
                  label={fieldLabel(key, child.title)}
                  fieldKey={key}
                  required={mandatory}
                  readonlyFields={readonlyFields}
                  references={references}
                />
              )}
            </div>
          );
        })}
      </fieldset>
    );
  }
  if (type === "array") {
    if (
      fieldKey === "parameters" &&
      !schema.items?.type &&
      !schema.items?.properties
    )
      return (
        <TextArea
          label={label}
          rows={5}
          isRequired={required}
          description='填写参数JSON数组。大整数使用带引号的十进制字符串，或{"encoding":"integer","value":"十进制"}；不能使用已舍入的数字。'
          value={
            typeof value === "string"
              ? value
              : JSON.stringify(value ?? [], null, 2)
          }
          onChange={onChange}
        />
      );
    const rows = Array.isArray(value) ? value : [];
    return (
      <fieldset className="nested-fields">
        <legend>
          {label}
          {required ? " · 必填" : ""}
        </legend>
        {schema.description && <p className="muted">{schema.description}</p>}
        {rows.map((v, i) => (
          <div className="array-field" key={i}>
            <Field
              schema={schema.items ?? { type: "string" }}
              value={v}
              onChange={(next) =>
                onChange(rows.map((x, j) => (j === i ? next : x)))
              }
              label={`第 ${i + 1} 项`}
              fieldKey={fieldKey}
              required
            />
            <Button
              label={`移除第 ${i + 1} 项`}
              variant="ghost"
              icon={<Icon name="close" />}
              isIconOnly
              onClick={() => onChange(rows.filter((_, j) => j !== i))}
            />
          </div>
        ))}
        <Button
          label={`添加${label}`}
          icon={<Icon name="plus" />}
          isDisabled={rows.length >= (schema.maxItems ?? 100)}
          onClick={() =>
            onChange([
              ...rows,
              initialValue(schema.items ?? { type: "string" }),
            ])
          }
        />
      </fieldset>
    );
  }
  if (type === "boolean")
    return (
      <label className="check-label">
        <input
          type="checkbox"
          checked={value === true}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          {label}
          {schema.description && (
            <small className="field-description">{schema.description}</small>
          )}
        </span>
      </label>
    );
  const secret =
    schema.writeOnly ||
    schema.format === "password" ||
    (/password|secret|token/.test(fieldKey) && !fieldKey.endsWith("_id"));
  if (/private_key|key_pem|credentials/.test(fieldKey))
    return (
      <div className="secret-multiline">
        <TextArea
          label={label}
          description={
            schema.description ??
            "仅用于本次配置输入；服务器不会在资源列表中回传密钥。"
          }
          value={
            typeof value === "string"
              ? value
              : JSON.stringify(value ?? {}, null, 2)
          }
          rows={5}
          onChange={onChange}
          autoComplete="off"
          isRequired={required}
        />
      </div>
    );
  if (
    !secret &&
    ((schema.maxLength ?? 0) > 2000 ||
      /content|config|sql|script|body|certificate|private_key/.test(fieldKey))
  )
    return (
      <TextArea
        label={label}
        description={schema.description}
        rows={5}
        value={String(value ?? "")}
        onChange={onChange}
        autoComplete="off"
        isRequired={required}
      />
    );
  return (
    <TextInput
      label={label}
      value={String(value ?? "")}
      onChange={onChange}
      description={schema.description}
      type={secret ? "password" : "text"}
      autoComplete="off"
      isRequired={required}
    />
  );
}
export function ActionForm({
  capability,
  preset = {},
  onExecuted,
  onBack,
  compact = false,
  references,
  cacheKeySuffix = "",
  lockedPresetFields = [],
  formatError,
}: {
  capability: Capability;
  preset?: Record<string, unknown>;
  onExecuted?: (result: unknown, context?: ActionContext) => void;
  onBack?: () => void;
  compact?: boolean;
  references?: References;
  cacheKeySuffix?: string;
  lockedPresetFields?: string[];
  formatError?: (error: APIError) => string | undefined;
}) {
  const schema = (capability.input_schema ?? {
    type: "object",
    properties: {},
  }) as Schema;
  const presetKey = JSON.stringify(preset);
  const stablePreset = Object.fromEntries(
    Object.entries(preset).filter(([key]) =>
      [
        "id",
        "site_id",
        "root",
        "path",
        "plugin_id",
        "instance_id",
        "operation",
        "channel_id",
        "service_id",
        "name",
        "database",
        "target_id",
        "credential_id",
        "certificate_id",
        "backup_id",
      ].includes(key),
    ),
  );
  const cacheKey =
    capability.name +
    ":" +
    JSON.stringify(stablePreset) +
    ":" +
    Object.keys(schema.properties ?? {})
      .sort()
      .join(",") +
    (cacheKeySuffix ? `:${cacheKeySuffix}` : "");
  const [draft, setDraft] = useState<unknown>(
    () => readDraft(cacheKey) ?? initialValue(schema),
  );
  const [raw, setRaw] = useState(false);
  const [rawValue, setRawValue] = useState("");
  const [result, setResult] = useState<unknown>();
  const [error, setError] = useState("");
  const [details, setDetails] = useState<unknown>();
  const [recovery, setRecovery] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const lastIntent = useRef<{ body: string; key: string }>(undefined);
  const awaitingRevision = useRef<number>(undefined);
  useEffect(() => {
    const cached = readDraft(cacheKey);
    const value = (cached ?? initialValue(schema)) as Record<string, unknown>;
    for (const [key, v] of Object.entries(JSON.parse(presetKey)))
      if (
        schema.properties?.[key] &&
        (!cached || !Object.prototype.hasOwnProperty.call(value, key))
      )
        value[key] = v;
    setDraft(value);
    const cachedRaw = readDraft(cacheKey + ":raw") as
      | { enabled: boolean; value: string }
      | undefined;
    setRaw(cachedRaw?.enabled ?? false);
    setRawValue(cachedRaw?.value ?? JSON.stringify(value, null, 2));
    const task = readDraft<TaskSubmission>(cacheKey + ":task");
    setResult(task?.submitted);
    setError("");
    setDetails(undefined);
    setConfirm(false);
    setRecovery(readDraft(cacheKey + ":recovery") === true);
    setConflict(false);
    lastIntent.current = readDraft(cacheKey + ":intent") as
      | { body: string; key: string }
      | undefined;
    awaitingRevision.current = undefined;
  }, [capability.name, cacheKey]);
  useEffect(() => {
    const next = JSON.parse(presetKey).expected_revision;
    if (
      awaitingRevision.current === undefined ||
      typeof next !== "number" ||
      next === awaitingRevision.current
    )
      return;
    setDraft((previous: unknown) =>
      previous && typeof previous === "object" && !Array.isArray(previous)
        ? { ...previous, expected_revision: next }
        : previous,
    );
    setRawValue((previous) => {
      try {
        const parsed = JSON.parse(previous);
        return JSON.stringify({ ...parsed, expected_revision: next }, null, 2);
      } catch {
        return previous;
      }
    });
    awaitingRevision.current = undefined;
  }, [presetKey]);
  useEffect(() => {
    saveDraft(cacheKey + ":raw", { enabled: raw, value: rawValue });
    saveDraft(
      cacheKey,
      raw
        ? (() => {
            try {
              return JSON.parse(rawValue);
            } catch {
              return draft;
            }
          })()
        : draft,
    );
  }, [cacheKey, draft, raw, rawValue]);
  const submit = async () => {
    if (recovery) return;
    let input: unknown;
    try {
      input = normalizeInput(
        schema,
        applyLockedPreset(
          raw ? JSON.parse(rawValue) : draft,
          preset,
          lockedPresetFields,
        ),
      );
      if (
        ["database_query", "database_instance_query"].includes(capability.name)
      )
        validateSQLParameters(input);
    } catch (e) {
      setError(apiErrorMessage(e, formatError));
      return;
    }
    if (!capability.read_only && !confirm) {
      setConfirm(true);
      return;
    }
    const body = JSON.stringify(input);
    const identifiers = (input && typeof input === "object" ? input : {}) as {
      id?: unknown;
      site_id?: unknown;
    };
    const contextInput: ActionContext["input"] = {
      ...(typeof identifiers.id === "string" ? { id: identifiers.id } : {}),
      ...(typeof identifiers.site_id === "string"
        ? { site_id: identifiers.site_id }
        : {}),
    };
    if (!lastIntent.current || lastIntent.current.body !== body)
      lastIntent.current = { body, key: intentKey() };
    saveDraft(cacheKey + ":intent", lastIntent.current);
    setBusy(true);
    setError("");
    setDetails(undefined);
    setConflict(false);
    try {
      const response = await execute(capability.name, input, {
        idempotencyKey: capability.read_only
          ? undefined
          : lastIntent.current.key,
      });
      setResult(response);
      if (capability.task) {
        const task = response as { id?: string; task_id?: string };
        const id = task?.id ?? task?.task_id;
        if (typeof id === "string")
          saveDraft(cacheKey + ":task", {
            submitted: { task_id: id },
            input: contextInput,
          } satisfies TaskSubmission);
      }
      if (!capability.task) {
        forgetDraft(cacheKey);
        forgetDraft(cacheKey + ":raw");
      }
      forgetDraft(cacheKey + ":intent");
      forgetDraft(cacheKey + ":recovery");
      setConfirm(false);
      lastIntent.current = undefined;
      const applied = input as { expected_revision?: number };
      awaitingRevision.current = applied.expected_revision;
      onExecuted?.(response, { action: capability.name, input: contextInput });
    } catch (e) {
      setError(apiErrorMessage(e, formatError));
      if (e instanceof APIError) {
        setDetails(e.details);
        setConflict(
          [409, 412].includes(e.status) &&
            /revision|conflict/.test(e.code) &&
            e.code !== "idempotency_conflict",
        );
        if (e.code === "operation_recovery_required") {
          setRecovery(true);
          saveDraft(cacheKey + ":recovery", true);
          const id = (e.details as { task_id?: unknown } | undefined)?.task_id;
          if (typeof id === "string" && id) {
            const submission = {
              submitted: { task_id: id },
              input: contextInput,
            };
            setResult(submission.submitted);
            saveDraft(cacheKey + ":task", submission);
          }
        }
      }
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className={`action-form ${compact ? "compact" : ""}`}>
      {onBack && (
        <Button label="返回操作列表" variant="ghost" onClick={onBack} />
      )}
      <div className="page-heading">
        <div>
          <h2>{actionLabel(capability)}</h2>
          <p className="monospace muted">{capability.name}</p>
        </div>
        <span className="status-chip neutral">
          {capability.read_only
            ? "只读查询"
            : capability.task
              ? "后台任务"
              : "管理操作"}
        </span>
      </div>
      <form
        className="form-stack"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <div className="split-label">
          <span className="muted">操作参数</span>
          <Button
            label={raw ? "使用表单" : "使用JSON输入"}
            variant="ghost"
            onClick={() => {
              if (!raw) setRawValue(JSON.stringify(draft, null, 2));
              else {
                try {
                  setDraft(JSON.parse(rawValue));
                  awaitingRevision.current = undefined;
                } catch {
                  setError("JSON尚未正确填写。");
                  return;
                }
              }
              setRaw(!raw);
            }}
          />
        </div>
        {raw ? (
          <TextArea
            label="操作参数JSON"
            rows={10}
            value={rawValue}
            onChange={(v) => {
              setRawValue(v);
              awaitingRevision.current = undefined;
              setConfirm(false);
            }}
          />
        ) : (
          <Field
            schema={schema}
            value={draft}
            onChange={(v) => {
              setDraft(v);
              awaitingRevision.current = undefined;
              setConfirm(false);
            }}
            label="参数"
            fieldKey=""
            references={references}
            readonlyFields={Array.from(
              new Set([
                ...lockedPresetFields,
                ...(typeof preset.expected_revision === "number"
                  ? ["expected_revision"]
                  : []),
              ]),
            )}
          />
        )}
        {error && (
          <div className="notice danger" role="alert">
            {error}
            {details !== undefined && (
              <details>
                <summary>查看错误详情</summary>
                <pre>{JSON.stringify(details, null, 2)}</pre>
              </details>
            )}
            {conflict &&
              !recovery &&
              typeof preset.expected_revision === "number" && (
                <div className="button-row">
                  <Button
                    label="重新读取资源版本"
                    variant="ghost"
                    onClick={() =>
                      window.dispatchEvent(new Event("powerlinux:invalidate"))
                    }
                  />
                  <Button
                    label="采用最新资源版本，保留当前参数"
                    onClick={() => {
                      try {
                        const current = (
                          raw ? JSON.parse(rawValue) : draft
                        ) as Record<string, unknown>;
                        const next = {
                          ...current,
                          expected_revision: preset.expected_revision,
                        };
                        setDraft(next);
                        setRawValue(JSON.stringify(next, null, 2));
                        lastIntent.current = undefined;
                        awaitingRevision.current = undefined;
                        forgetDraft(cacheKey + ":intent");
                        setConfirm(false);
                        setError("");
                        setConflict(false);
                      } catch {
                        setError("JSON尚未正确填写，当前草稿已保留。");
                      }
                    }}
                  />
                </div>
              )}
            {lastIntent.current && !recovery && (
              <Button
                label="开始新的操作"
                variant="ghost"
                onClick={() => {
                  lastIntent.current = undefined;
                  forgetDraft(cacheKey + ":intent");
                  setConfirm(false);
                  setError("");
                  setResult(undefined);
                  forgetDraft(cacheKey + ":task");
                }}
              />
            )}
          </div>
        )}
        {recovery && (
          <div className="notice warning">
            上次操作状态尚未确认。请核对关联任务与目标资源，不能直接创建新请求重复执行。
          </div>
        )}
        {confirm && (
          <div className="notice warning">
            <strong>确认操作与变更参数</strong>
            <p>{actionLabel(capability)}</p>
            <p>
              核对目标、权限与参数。失败后的重试会保留同一请求标识，避免重复执行。
            </p>
            <Button label="取消执行" onClick={() => setConfirm(false)} />
          </div>
        )}
        <Button
          label={
            confirm ? "确认执行" : capability.read_only ? "查询" : "执行操作"
          }
          variant="primary"
          type="submit"
          isLoading={busy}
          isDisabled={recovery}
        />
      </form>
      {result !== undefined && (
        <section className="action-result" aria-live="polite">
          <h3>操作结果</h3>
          {(capability.task || recovery) && (
            <p className="muted">
              {recovery ? "关联任务待核对" : "任务已提交"}
              ；最终执行、产物与健康验证请在任务详情中查看。
            </p>
          )}
          {capability.task || recovery ? (
            <TaskResult
              submitted={result}
              onFinished={(task) => {
                const submission = readDraft<TaskSubmission>(
                  cacheKey + ":task",
                );
                if (!submission || submission.submitted.task_id !== task.id)
                  return;
                if (!recovery) forgetDraft(cacheKey + ":task");
                if (task.status === "succeeded" || task.state === "succeeded") {
                  forgetDraft(cacheKey);
                  forgetDraft(cacheKey + ":raw");
                }
                const result = task.result as { data?: unknown } | undefined;
                onExecuted?.(result?.data ?? task.result ?? task, {
                  action: capability.name,
                  input: submission.input,
                  task,
                });
              }}
            />
          ) : [
              "database_query",
              "database_instance_query",
              "database_tables_list",
            ].includes(capability.name) ? (
            <SQLResult result={result} />
          ) : (
            <pre className="json-result">{JSON.stringify(result, null, 2)}</pre>
          )}
        </section>
      )}
    </div>
  );
}
export function ActionBrowser({
  capabilities,
  category,
  selected,
  onSelect,
  onExecuted,
  error,
  preset = {},
}: {
  capabilities: Capability[];
  category: string;
  selected?: string;
  onSelect: (name?: string) => void;
  onExecuted: () => void;
  error?: string;
  preset?: Record<string, unknown>;
}) {
  const [filter, setFilter] = useState("");
  const selectedCapability = capabilities.find((c) => c.name === selected);
  const matching = capabilities.filter(
    (c) =>
      (prefixes[category]?.test(c.name) ?? true) &&
      (c.name.includes(filter) || c.description?.includes(filter)),
  );
  if (selectedCapability)
    return (
      <Card className="panel-card action-detail">
        <ActionForm
          key={selectedCapability.name}
          capability={selectedCapability}
          preset={preset}
          onExecuted={onExecuted}
          onBack={() => onSelect(undefined)}
        />
      </Card>
    );
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <h2>已授权的操作</h2>
        <TextInput
          label="筛选操作"
          isLabelHidden
          value={filter}
          onChange={setFilter}
          placeholder="按名称筛选"
          startIcon={<Icon name="search" />}
        />
      </div>
      {error ? (
        <div className="notice danger" role="alert">
          {error}
        </div>
      ) : matching.length ? (
        <div className="action-grid">
          {matching.map((c) => (
            <button
              className="action-tile"
              key={c.name}
              onClick={() => onSelect(c.name)}
            >
              <span className="heading-icon">
                <Icon name={c.read_only ? "search" : "settings"} />
              </span>
              <div>
                <strong>{actionLabel(c)}</strong>
                <p>{c.name}</p>
              </div>
              <Icon name="arrow" size={17} />
            </button>
          ))}
        </div>
      ) : (
        <div className="empty">
          <span className="empty-icon">
            <Icon name="software" size={26} />
          </span>
          <strong>当前没有已授权的操作</strong>
          <p>此模块尚未提供可用动作，或当前账户无访问权限。</p>
        </div>
      )}
      <p className="section-caption">
        网页与MCP使用同一业务动作、权限、任务与审计。
      </p>
    </Card>
  );
}
