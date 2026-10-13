"use client";
import { useEffect, useState } from "react";
import { sqlCell, sqlColumns } from "@/lib/sql-wire";
import { Pager } from "./resource-ui";
type Result = {
  columns?: string[];
  items?: Record<string, unknown>[];
  rows_affected?: unknown;
  truncated?: boolean;
  database?: string;
};
export function SQLResult({ result }: { result: unknown }) {
  const data = result as Result;
  const [offset, setOffset] = useState(0);
  useEffect(() => setOffset(0), [result]);
  if (!data || typeof data !== "object")
    return <pre className="json-result">{JSON.stringify(result, null, 2)}</pre>;
  if (data.rows_affected !== undefined) {
    const value = sqlCell(data.rows_affected);
    return (
      <p>
        影响行数 <strong>{value.text}</strong>
        {value.detail && <small> · {value.detail}</small>}
      </p>
    );
  }
  if (
    !Array.isArray(data.columns) ||
    !data.columns.every((value) => typeof value === "string") ||
    !Array.isArray(data.items) ||
    !data.items.every(
      (row) => row && typeof row === "object" && !Array.isArray(row),
    )
  )
    return <pre className="json-result">{JSON.stringify(result, null, 2)}</pre>;
  const columns = sqlColumns(data.columns);
  const unsafe = data.items.some((row) =>
    Object.values(row).some((value) => sqlCell(value).kind === "unsafe"),
  );
  return (
    <section className="stack" aria-label="数据库查询结果">
      <p className="muted">
        {data.database} · 本次返回 {data.items.length} 行
      </p>
      {data.truncated && (
        <div className="notice warning">
          结果达到服务器行数上限，后续数据未读取。请收窄查询条件或明确分页。
        </div>
      )}
      {unsafe && (
        <div className="notice danger" role="alert">
          结果中有整数无法确认精度，不能作为原值使用。请检查服务端是否支持精确整数协议。
        </div>
      )}
      <div className="resource-table-wrap">
        <table className="resource-table" aria-label="SQL数据表">
          <thead>
            <tr>
              {columns.map((name) => (
                <th key={name}>{name}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {data.items.slice(offset, offset + 25).map((row, index) => (
              <tr key={offset + index}>
                {columns.map((name) => {
                  const value = sqlCell(row[name]);
                  return (
                    <td key={name}>
                      <code className="wrap-code">{value.text}</code>
                      {value.detail && <small>{value.detail}</small>}
                      {value.kind === "binary" && (
                        <details>
                          <summary>Base64原值</summary>
                          <code className="wrap-code">
                            {(row[name] as { data: string }).data}
                          </code>
                        </details>
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!data.items.length && <p>查询没有返回行。</p>}
      <Pager
        offset={offset}
        limit={25}
        total={data.items.length}
        count={Math.max(0, Math.min(25, data.items.length - offset))}
        loading={false}
        onOffset={setOffset}
      />
      {!unsafe && (
        <details>
          <summary>查看类型与原值</summary>
          <pre className="json-result">{JSON.stringify(result, null, 2)}</pre>
        </details>
      )}
    </section>
  );
}
