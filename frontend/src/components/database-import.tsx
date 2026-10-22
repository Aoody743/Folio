"use client";
import { t as translate } from "@/lib/i18n";
import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import {
  Capability,
  bytes,
  list,
  streamRequest,
  apiErrorMessage,
} from "@/lib/api";
import { fileHash } from "@/lib/upload";
import {
  DatabaseImport as ImportRecord,
  verifiedDatabaseImport,
} from "@/lib/database-import";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { ResourceActions, State } from "./resource-ui";

export function DatabaseImport({
  instanceID,
  database,
  revision,
  engine,
  capabilities,
  onExecuted,
}: {
  instanceID: string;
  database: string;
  revision: number;
  engine: string;
  capabilities: Capability[];
  onExecuted: () => void;
}) {
  const [file, setFile] = useState<File>();
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState("");
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<string>();
  const [operation, setOperation] = useState<string>();
  const controller = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => controller.current?.abort(), []);
  const apply = capabilities.find(
    (item) => item.name === "managed_database_import_apply",
  );
  const canUpload = capabilities.some(
    (item) => item.name === "managed_database_import_upload",
  );
  const canList = capabilities.some(
    (item) => item.name === "managed_database_imports_list",
  );
  const imports = useResource<{ items: ImportRecord[] }>(
    "managed_database_imports_list",
    {
      instance_id: instanceID,
      database,
    },
    canList,
  );
  const target = { instance: instanceID, database, revision };
  const rows = list(imports.data);
  const selectedRow = rows.find((row) => row.id === selected);
  const chosen = verifiedDatabaseImport(selectedRow, target);
  const maximum = engine === "postgresql" ? 64 * 1024 ** 2 : 4 * 1024 ** 3;
  async function upload() {
    if (!file || busy || !canUpload) return;
    const abort = new AbortController();
    controller.current = abort;
    setBusy(true);
    setError("");
    setSelected(undefined);
    try {
      if (file.size < 1 || file.size > maximum)
        throw new Error(
          translate("请选择不超过 {0} 的文件。", [bytes(maximum)]),
        );
      setStage(translate("正在校验文件…"));
      const digest = await fileHash(file, abort.signal);
      setStage(translate("正在上传文件…"));
      const query = new URLSearchParams({
        instance_id: instanceID,
        database,
        expected_revision: String(revision),
      });
      const response = await streamRequest(
        `/api/v1/databases/backups/imports?${query}`,
        {
          method: "POST",
          body: file,
          signal: abort.signal,
          headers: {
            "Content-Type": "application/octet-stream",
            "X-Content-SHA256": digest,
          },
        },
      );
      const payload = await response.json();
      const record = verifiedDatabaseImport(payload.data, target);
      if (
        !record ||
        record.bytes !== file.size ||
        record.sha256 !== digest ||
        record.engine !== engine
      )
        throw new Error(
          translate("上传结果与文件不符。请刷新导入记录并核对，暂不能应用。"),
        );
      await imports.reload();
      setSelected(record.id);
      setStage(translate("文件已校验，请核对导入目标后执行。"));
    } catch (reason) {
      setError(apiErrorMessage(reason));
      setStage(translate("请刷新导入记录核对传输状态，避免重复上传。"));
      void imports.reload();
    } finally {
      setBusy(false);
      controller.current = undefined;
    }
  }
  return (
    <section className="stack" aria-label={translate("外部数据库导入")}>
      <p className="notice">
        {translate(
          "请选择空的、由 Folio 管理的目标数据库。导入会验证文件和目标权限；已有表或集合的目标会被拒绝。",
        )}
      </p>
      <p className="muted">
        {engine === "postgresql"
          ? translate(
              "PostgreSQL 当前支持 Folio 表数据 JSON，最大 64 MiB。原生归档导入仍在开发。",
            )
          : engine === "mongodb"
            ? translate("MongoDB 支持 Folio BSON 数据归档。")
            : translate(
                "MySQL/MariaDB 支持表结构与数据 SQL 文件；导入会拒绝账户、服务器和客户端命令。",
              )}
      </p>
      <label>
        {translate("选择导入文件")}
        <input
          type="file"
          disabled={busy || !canUpload}
          onChange={(event) => {
            setFile(event.target.files?.[0]);
            setError("");
            setStage("");
          }}
        />
      </label>
      {file && (
        <p>
          {file.name}· {bytes(file.size)}
        </p>
      )}
      <div className="resource-toolbar">
        <Button
          label={busy ? translate("正在处理…") : translate("上传并校验")}
          isDisabled={!file || busy || !canUpload}
          onClick={() => void upload()}
        />
        {busy && (
          <Button
            label={translate("取消上传")}
            variant="ghost"
            onClick={() => controller.current?.abort()}
          />
        )}
        <Button
          label={translate("刷新导入记录")}
          variant="ghost"
          isDisabled={busy || !canList}
          onClick={() => void imports.reload()}
        />
      </div>
      {stage && <p role="status">{stage}</p>}
      {(error || imports.error) && (
        <p role="alert" className="notice danger">
          {error || imports.error}
        </p>
      )}
      {!canUpload && (
        <p className="muted">{translate("当前没有上传导入文件的权限。")}</p>
      )}
      {rows.map((row) => (
        <button
          key={row.id}
          className="resource-row"
          disabled={busy}
          aria-pressed={selected === row.id}
          onClick={() => {
            setSelected(row.id);
            setOperation(undefined);
          }}
        >
          <span>
            {row.id}· {bytes(row.bytes)}
          </span>
          <State value={row.state} />
        </button>
      ))}
      {selected && !chosen && (
        <p className="notice warning">
          {translate(
            "此记录尚未完成上传、目标版本已改变或需要恢复检查。请重新核对后再应用。",
          )}
        </p>
      )}
      {selectedRow && (
        <ResourceActions
          key={`${selectedRow.id}:${selectedRow.revision}`}
          capabilities={capabilities}
          selected={operation}
          onSelect={setOperation}
          names={(name) =>
            name === "managed_database_import_delete" ||
            name === "managed_database_import_recover"
          }
          preset={{
            instance_id: instanceID,
            database,
            expected_revision: revision,
            import_id: selectedRow.id,
            import_revision: selectedRow.revision,
          }}
          onExecuted={() => {
            setSelected(undefined);
            void imports.reload();
            onExecuted();
          }}
        />
      )}
      {chosen && apply && (
        <>
          <div className="detail-grid">
            <div>
              <small>{translate("导入目标")}</small>
              <strong>{database}</strong>
              <span>{instanceID}</span>
            </div>
            <div>
              <small>{translate("文件摘要")}</small>
              <code>{chosen.sha256}</code>
              <span>{bytes(chosen.bytes)}</span>
            </div>
          </div>
          <ActionForm
            key={`${chosen.id}:${chosen.revision}:${revision}`}
            capability={apply}
            cacheKeySuffix={`${chosen.id}:${chosen.revision}:${revision}`}
            lockedPresetFields={[
              "instance_id",
              "database",
              "expected_revision",
              "import_id",
              "import_revision",
            ]}
            preset={{
              instance_id: instanceID,
              database,
              expected_revision: revision,
              import_id: chosen.id,
              import_revision: chosen.revision,
            }}
            onBack={() => setSelected(undefined)}
            onExecuted={() => {
              setSelected(undefined);
              void imports.reload();
              onExecuted();
            }}
          />
        </>
      )}
    </section>
  );
}
