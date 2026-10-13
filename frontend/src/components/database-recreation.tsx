"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, APIError, Capability } from "@/lib/api";
import { choiceLabel } from "@/lib/labels";
import {
  isMissingManagedDatabaseReceipt,
  canRecreateDeletedManagedDatabase,
  managedDatabaseCreateErrorMessage,
  managedDatabaseCreatePreset,
  ManagedDatabaseReceipt,
  parseManagedDatabaseReceipt,
} from "@/lib/database-recreation";
import { State, Unavailable } from "./resource-ui";
import { ActionForm } from "./actions";

type Lookup =
  | { kind: "idle" }
  | { kind: "fresh" }
  | { kind: "receipt"; value: ManagedDatabaseReceipt };

export function DatabaseRecreation({
  instanceID,
  initialDatabase = "",
  receiptCapability,
  createCapability,
  onBack,
  onExecuted,
}: {
  instanceID: string;
  initialDatabase?: string;
  receiptCapability: Capability;
  createCapability: Capability;
  onBack: () => void;
  onExecuted: () => void;
}) {
  const [database, setDatabase] = useState(initialDatabase);
  const [lookup, setLookup] = useState<Lookup>({ kind: "idle" });
  const [mode, setMode] = useState<"lookup" | "create" | "done">("lookup");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const inspect = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    setLookup({ kind: "idle" });
    try {
      const value = await action<unknown>(receiptCapability.name, {
        instance_id: instanceID,
        database,
      });
      setLookup({
        kind: "receipt",
        value: parseManagedDatabaseReceipt(value, instanceID, database),
      });
    } catch (cause) {
      if (isMissingManagedDatabaseReceipt(cause)) {
        setLookup({ kind: "fresh" });
      } else {
        setError(
          cause instanceof APIError && cause.code === "forbidden"
            ? "当前账号无权读取该名称的数据库管理记录，安全创建流程已停止。"
            : cause instanceof Error
              ? cause.message
              : "读取数据库管理记录失败。",
        );
      }
    } finally {
      setBusy(false);
    }
  };

  if (
    mode === "create" &&
    (lookup.kind === "fresh" ||
      (lookup.kind === "receipt" &&
        canRecreateDeletedManagedDatabase(lookup.value)))
  ) {
    const receipt = lookup.kind === "receipt" ? lookup.value : undefined;
    const preset = managedDatabaseCreatePreset(instanceID, database, receipt);
    const revision = Number(preset.expected_revision);
    return (
      <section className="stack" aria-label="确认数据库创建">
        <div className="notice warning">
          <strong>
            {receipt ? "按已删除记录版本重建" : "创建无管理记录的新名称"}
          </strong>
          <p>
            数据库 {database} · 提交版本 {revision}
          </p>
        </div>
        <ActionForm
          key={`${instanceID}:${database}:${revision}`}
          capability={createCapability}
          preset={preset}
          cacheKeySuffix={`receipt:${lookup.kind}:${database}:${revision}`}
          lockedPresetFields={["instance_id", "database", "expected_revision"]}
          formatError={(cause) =>
            managedDatabaseCreateErrorMessage(
              cause.code,
              cause.status,
              cause.message,
            )
          }
          onBack={() => setMode("lookup")}
          onExecuted={() => {
            onExecuted();
            setMode("done");
          }}
        />
      </section>
    );
  }

  if (mode === "done")
    return (
      <section className="stack" aria-label="数据库创建完成">
        <p className="notice success" role="status">
          数据库创建已完成，实例资源列表已重新读取。
        </p>
        <Button label="返回数据库资源" onClick={onBack} />
      </section>
    );

  return (
    <section className="stack" aria-label="查询数据库管理记录">
      <Button label="返回数据库资源" variant="ghost" onClick={onBack} />
      <p className="muted">
        先查询该名称的权威管理记录，再按当前删除版本重建或按新名称创建。
      </p>
      <form
        className="form-stack"
        onSubmit={(event) => void inspect(event)}
      >
        <TextInput
          label="数据库名称"
          value={database}
          onChange={(value) => {
            setDatabase(value);
            setLookup({ kind: "idle" });
            setError("");
          }}
          autoComplete="off"
          isRequired
        />
        <Button
          type="submit"
          label="读取管理记录"
          variant="primary"
          isLoading={busy}
          isDisabled={busy}
        />
      </form>
      {busy && (
        <p className="muted" role="status">
          正在读取权威管理记录…
        </p>
      )}
      {error && (
        <div className="notice danger" role="alert">
          {error}
        </div>
      )}
      {lookup.kind === "fresh" && (
        <div className="stack">
          <div className="notice warning">
            当前没有该名称的面板管理记录。若数据库服务中已有同名外部数据库或账户，服务会拒绝创建。
          </div>
          <Button
            label="确认按新名称创建"
            variant="primary"
            onClick={() => setMode("create")}
          />
        </div>
      )}
      {lookup.kind === "receipt" && (
        <div className="stack">
          <div className="detail-grid">
            <div>
              <small>管理记录</small>
              <strong>{lookup.value.database}</strong>
              <span>{lookup.value.username}</span>
            </div>
            <div>
              <small>引擎与权限</small>
              <strong>{lookup.value.engine}</strong>
              <span>{choiceLabel(lookup.value.permission, "permission")}</span>
            </div>
            <div>
              <small>记录状态</small>
              {lookup.value.state === "deleted" ? (
                <span className="status-chip warning">已删除</span>
              ) : (
                <State value={lookup.value.state} />
              )}
              <span className="muted">版本 {lookup.value.revision}</span>
            </div>
          </div>
          {lookup.value.state === "deleted" &&
          lookup.value.engine === "mongodb" ? (
            <Unavailable
              title="MongoDB 暂不支持重建已删除数据库"
              detail="服务目前无法为 MongoDB tombstone 提供原子所有权保护，因此不会提交重建操作。"
            />
          ) : lookup.value.state === "deleted" ? (
            <Button
              label={`确认按版本 ${lookup.value.revision} 重建`}
              variant="primary"
              onClick={() => setMode("create")}
            />
          ) : (
            <Unavailable
              title="当前记录不是已删除状态"
              detail="只有权威记录明确为已删除时，才能选择按该版本重建。"
            />
          )}
        </div>
      )}
    </section>
  );
}
