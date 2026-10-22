"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, apiErrorMessage, Capability } from "@/lib/api";
import { dockerCapability } from "@/lib/docker-management";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import {
  databaseAdminState,
  databaseAdminTaskInput,
} from "@/lib/database-admin-credentials";
export function DatabaseAdminCredentials({
  instanceID,
  engine,
  capabilities,
  onExecuted,
  active,
}: {
  instanceID: string;
  engine: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active: boolean;
}) {
  const supported = [
    "mysql",
    "mysql-community",
    "mariadb",
    "postgresql",
  ].includes(engine);
  const cap = (kind: string, fields: string[], task?: boolean) =>
    dockerCapability(
      capabilities,
      `managed_database_admin_${kind}`,
      fields,
      task,
    );
  const data = useResource<unknown>(
    "managed_database_admin_inspect",
    { instance_id: instanceID },
    active && supported && !!cap("inspect", ["instance_id"]),
  );
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<unknown>();
  const state = data.error ? undefined : databaseAdminState(data.data);
  const valid = state;
  const refresh = () => {
    void data.reload();
    onExecuted();
  };
  const execute = async (kind: string, custom = false) => {
    if (!state || data.loading || data.error) return;
    const draft = password;
    setPassword("");
    setBusy(true);
    setError("");
    setResult(undefined);
    const input = databaseAdminTaskInput(
      instanceID,
      state.current_credential_version,
    );
    try {
      let password_version: string | undefined;
      if (custom) {
        const prepared = await action<{ password_version: string }>(
          "managed_database_admin_prepare_password",
          { ...input, password: draft },
        );
        if (typeof prepared?.password_version !== "string")
          throw new Error(translate("密码准备响应无效，请重新读取状态。"));
        databaseAdminTaskInput(
          instanceID,
          state.current_credential_version,
          prepared.password_version,
        );
        password_version = prepared.password_version;
      }
      const value = await action<unknown>(
        `managed_database_admin_${kind}`,
        databaseAdminTaskInput(
          instanceID,
          state.current_credential_version,
          password_version,
        ),
      );
      setResult(value);
      refresh();
    } catch (e) {
      setError(apiErrorMessage(e));
      refresh();
    } finally {
      setBusy(false);
    }
  };
  if (!supported || !cap("inspect", ["instance_id"])) return null;
  return (
    <section className="stack" aria-label={translate("数据库管理员凭据")}>
      <h3>{translate("数据库管理员凭据")}</h3>
      {(data.error || error) && (
        <p role="alert" className="notice danger">
          {error || data.error}
        </p>
      )}
      {valid && (
        <>
          <p>
            {translate("状态：")}
            {state.state}
            {translate(" · 当前凭据版本：")}
            {state.current_credential_version || translate("旧版未初始化")}
          </p>
          <div className="button-row">
            {[
              ["initialize", translate("初始化管理员凭据版本")],
              ["rotate", translate("轮换为随机管理员密码")],
              ["recover", translate("恢复凭据事务")],
              ["apply_pending", translate("应用待处理凭据")],
            ].map(([kind, label]) =>
              state.available_actions.includes(kind) &&
              cap(kind, ["instance_id", "current_credential_version"], true) ? (
                <Button
                  key={kind}
                  label={label}
                  isDisabled={busy || data.loading}
                  onClick={() => {
                    setPassword("");
                    if (
                      window.confirm(
                        translate("确认{0}？操作将按当前凭据版本提交。", [
                          label,
                        ]),
                      )
                    )
                      void execute(kind);
                  }}
                />
              ) : null,
            )}
          </div>
          {state.available_actions.includes("rotate") &&
            cap(
              "prepare_password",
              ["instance_id", "current_credential_version", "password"],
              false,
            ) &&
            cap(
              "rotate",
              ["instance_id", "current_credential_version", "password_version"],
              true,
            ) && (
              <>
                <TextInput
                  label={translate("自定义管理员新密码")}
                  type="password"
                  value={password}
                  onChange={setPassword}
                  autoComplete="new-password"
                />
                <p className="muted">
                  {translate(
                    "密码经安全通道准备，持久任务仅保存密码版本编号。密码不会回显。",
                  )}
                </p>
                <Button
                  label={translate("轮换为自定义密码")}
                  isDisabled={
                    busy ||
                    data.loading ||
                    password.length < 12 ||
                    password.length > 512 ||
                    /[\r\n]/.test(password)
                  }
                  onClick={() => {
                    if (window.confirm(translate("确认轮换数据库管理员密码？")))
                      void execute("rotate", true);
                  }}
                />
              </>
            )}
        </>
      )}
      {result !== undefined && (
        <TaskResult
          key={JSON.stringify(result)}
          submitted={result}
          capabilities={capabilities}
          onFinished={refresh}
        />
      )}
    </section>
  );
}
