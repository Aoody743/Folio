"use client";
import { t as translate } from "@/lib/i18n";
import { fullDate } from "@/lib/api";
import { choiceLabel, fieldLabel } from "@/lib/labels";

const hidden = /token|password|secret|credential|private[_-]?key|api[_-]?key|access[_-]?key/i;
function Value({ value }: { value: unknown }) {
  if (value == null)
    return <span className="muted">{translate("未提供")}</span>;
  if (typeof value === "boolean")
    return <>{value ? translate("是") : translate("否")}</>;
  if (Array.isArray(value))
    return value.length ? (
      <ul className="change-list">
        {value.map((entry, index) => (
          <li key={index}>
            <Value value={entry} />
          </li>
        ))}
      </ul>
    ) : (
      <span className="muted">{translate("无")}</span>
    );
  if (typeof value === "object")
    return (
      <dl className="change-fields">
        {Object.entries(value)
          .filter(([key]) => !hidden.test(key))
          .map(([key, item]) => (
            <div key={key}>
              <dt>{fieldLabel(key)}</dt>
              <dd>
                <Value value={item} />
              </dd>
            </div>
          ))}
      </dl>
    );
  return <span className="break-anywhere">{String(value)}</span>;
}
export function ChangeSummary({ plan }: { plan: Record<string, unknown> }) {
  const main = [
    "operation",
    "plugin_id",
    "instance_id",
    "version",
    "state",
    "expires_at",
  ];
  const details = Object.fromEntries(
    Object.entries(plan).filter(
      ([key]) =>
        !main.includes(key) &&
        !hidden.test(key) &&
        !["plan_id", "id", "expected_revision"].includes(key),
    ),
  );
  return (
    <div className="change-summary">
      <dl className="change-fields">
        {main
          .filter((key) => plan[key] !== undefined)
          .map((key) => (
            <div key={key}>
              <dt>{fieldLabel(key)}</dt>
              <dd>
                {key === "operation" || key === "state" ? (
                  choiceLabel(plan[key])
                ) : key === "expires_at" ? (
                  fullDate(String(plan[key]))
                ) : (
                  <Value value={plan[key]} />
                )}
              </dd>
            </div>
          ))}
      </dl>
      {Object.keys(details).length > 0 && (
        <div className="change-impact">
          <h4>{translate("变更内容")}</h4>
          <Value value={details} />
        </div>
      )}
      <p className="muted">
        {translate(
          "请核对目标、依赖、目录与服务变更。执行后还需查看任务的最终验证结果。",
        )}
      </p>
      <details className="technical-details">
        <summary>{translate("高级详情：原始计划")}</summary>
        <pre className="json-result">
          {JSON.stringify(
            plan,
            (key, value) => (hidden.test(key) ? "[redacted]" : value),
            2,
          )}
        </pre>
      </details>
    </div>
  );
}
