"use client";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { action, bytes, Capability, fullDate } from "@/lib/api";
import {
  databaseBinlogCanPreview,
  databaseBinlogList,
  databaseBinlogPreview,
  databaseBinlogPurgeInput,
  databaseBinlogStatus,
  DatabaseBinlogPreview,
  phase2Capability,
} from "@/lib/database-binlog";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { State, Unavailable } from "./resource-ui";

const fields = {
  status: ["instance_id"],
  list: ["instance_id", "limit", "after"],
  configure: [
    "instance_id",
    "enabled",
    "expected_config_revision",
    "expected_instance_revision",
  ],
  recover: [
    "instance_id",
    "command",
    "expected_config_revision",
    "expected_instance_revision",
  ],
  preview: ["instance_id", "retain_from"],
  purge: [
    "instance_id",
    "preview_id",
    "snapshot_sha256",
    "expected_config_revision",
    "expected_instance_revision",
    "binlogs_to_purge",
    "confirm_replica_history_loss",
  ],
};

export function DatabaseBinlogManagement({
  instanceID,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const has = (
    name: string,
    scope: string,
    required: string[],
    task: boolean,
    readOnly?: boolean,
  ) => phase2Capability(capabilities, name, scope, required, task, readOnly);
  const statusCapability = has(
    "managed_database_binlog_status",
    "databases:read",
    fields.status,
    false,
    true,
  );
  const listCapability = has(
    "managed_database_binlogs_list",
    "databases:read",
    fields.list,
    false,
    true,
  );
  const configureCapability = has(
    "managed_database_binlog_configure",
    "databases:admin",
    fields.configure,
    true,
    false,
  );
  const recoverCapability = has(
    "managed_database_binlog_config_recover",
    "databases:admin",
    fields.recover,
    true,
    false,
  );
  const previewCapability = has(
    "managed_database_binlog_purge_preview",
    "databases:admin",
    fields.preview,
    false,
    false,
  );
  const purgeCapability = has(
    "managed_database_binlog_purge",
    "databases:admin",
    fields.purge,
    true,
    false,
  );
  const [pageCursors, setPageCursors] = useState<string[]>([""]);
  const after = pageCursors[pageCursors.length - 1] ?? "";
  const statusResource = useResource<unknown>(
    "managed_database_binlog_status",
    { instance_id: instanceID },
    active && !!statusCapability,
    15000,
  );
  const binlogResource = useResource<unknown>(
    "managed_database_binlogs_list",
    { instance_id: instanceID, limit: 500, ...(after ? { after } : {}) },
    active && !!listCapability,
    15000,
  );
  const status = databaseBinlogStatus(statusResource.data);
  const binlogs = databaseBinlogList(binlogResource.data);
  const canPreview = databaseBinlogCanPreview(status, binlogs);
  const [retainFrom, setRetainFrom] = useState("");
  const [preview, setPreview] = useState<DatabaseBinlogPreview>();
  const [acknowledged, setAcknowledged] = useState(false);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const [configuration, setConfiguration] = useState<boolean>();
  const [recovery, setRecovery] = useState<
    "apply_pending" | "restore_previous"
  >();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!preview) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [preview]);
  const refresh = () => {
    void statusResource.reload();
    void binlogResource.reload();
    onExecuted();
  };
  const makePreview = async () => {
    if (!previewCapability || !retainFrom || !canPreview) return;
    setPreviewBusy(true);
    setPreviewError("");
    setPreview(undefined);
    setAcknowledged(false);
    try {
      const value = await action<unknown>(previewCapability.name, {
        instance_id: instanceID,
        retain_from: retainFrom,
      });
      const parsed = databaseBinlogPreview(value, retainFrom);
      if (!parsed)
        throw new Error(
          translate("服务返回的清理预览无效，请重新读取binlog列表。"),
        );
      const retainIndex =
        binlogs?.items.findIndex((log) => log.name === retainFrom) ?? -1;
      const candidates =
        retainIndex < 0 ? [] : binlogs!.items.slice(0, retainIndex);
      if (
        !binlogs ||
        parsed.active_log !== binlogs.active_log ||
        parsed.expected_config_revision !== binlogs.config_revision ||
        parsed.expected_instance_revision !== binlogs.instance_revision ||
        candidates.length !== parsed.binlogs_to_purge.length ||
        candidates.some(
          (log, index) =>
            log.name !== parsed.binlogs_to_purge[index]?.name ||
            log.size_bytes !== parsed.binlogs_to_purge[index]?.size_bytes,
        )
      )
        throw new Error(
          translate("清理预览与当前完整日志清单不一致，请重新读取后再试。"),
        );
      setPreview(parsed);
      setNow(Date.now());
    } catch (error) {
      setPreviewError(
        error instanceof Error
          ? error.message
          : translate("创建清理预览失败。"),
      );
      refresh();
    } finally {
      setPreviewBusy(false);
    }
  };
  let purgeInput: ReturnType<typeof databaseBinlogPurgeInput> | undefined;
  let purgeInputError = "";
  if (preview) {
    try {
      purgeInput = databaseBinlogPurgeInput(instanceID, preview, now);
    } catch (error) {
      purgeInputError =
        error instanceof Error ? error.message : translate("清理预览已失效。");
    }
  }
  const previewStale =
    !!preview &&
    (!status ||
      !binlogs ||
      status.config_revision !== preview.expected_config_revision ||
      status.instance_revision !== preview.expected_instance_revision ||
      binlogs.config_revision !== preview.expected_config_revision ||
      binlogs.instance_revision !== preview.expected_instance_revision ||
      binlogs.active_log !== preview.active_log);

  return (
    <section
      className="stack"
      aria-label={translate("MySQL和MariaDB二进制日志管理")}
    >
      <div className="card-heading">
        <div>
          <h3>{translate("Binlog 管理")}</h3>
          <p className="muted">
            {translate("MySQL / MariaDB 实例级二进制日志。")}
          </p>
        </div>
        <div className="button-row">
          {after && (
            <Button
              label={translate("从第一页重新读取")}
              variant="ghost"
              onClick={() => setPageCursors([""])}
            />
          )}
          <Button
            label={translate("刷新 Binlog 状态与列表")}
            variant="ghost"
            isDisabled={statusResource.loading || binlogResource.loading}
            onClick={refresh}
          />
        </div>
      </div>
      {statusResource.error && (
        <div className="notice danger" role="alert">
          {statusResource.error}
        </div>
      )}
      {binlogResource.error && (
        <div className="notice danger" role="alert">
          {binlogResource.error}
        </div>
      )}
      {!statusCapability ? (
        <Unavailable title={translate("当前账户没有 Binlog 状态读取能力")} />
      ) : status ? (
        <>
          <div className="detail-grid">
            <div>
              <small>{translate("二进制日志")}</small>
              <State value={status.enabled ? "active" : "disabled"} />
              <span>{status.format || translate("格式未提供")}</span>
            </div>
            <div>
              <small>{translate("活动日志")}</small>
              <strong>{status.active_log || translate("未提供")}</strong>
              <span>
                {translate("服务器 ID ")}
                {status.server_id}
              </span>
            </div>
            <div>
              <small>{translate("日志基名")}</small>
              <strong>{status.basename || translate("未提供")}</strong>
              <span>
                {translate("过期设置 ")}
                {status.expiration_seconds}
                {translate(" 秒")}
              </span>
            </div>
            <div>
              <small>{translate("配置状态")}</small>
              <State value={status.configuration_state} />
              <span className="muted">
                {translate("配置版本")}
                {status.config_revision.slice(0, 12)}
                {translate(" · 实例版本 ")}
                {status.instance_revision}
              </span>
              {status.configuration_managed &&
                status.configured_enabled !== undefined && (
                  <span className="muted">
                    {translate("受管设置")}
                    {status.configured_enabled
                      ? translate("已启用")
                      : translate("已停用")}
                  </span>
                )}
            </div>
          </div>
          {status.configuration_state === "runtime_config_mismatch" && (
            <div className="notice warning">
              {translate(
                "运行时 Binlog 状态与受管配置不一致。请先恢复配置状态，再启用或停用 Binlog。",
              )}
            </div>
          )}
          {status.configuration_state === "configuration_unmanaged" && (
            <div className="notice warning">
              {translate(
                "此实例没有可验证的面板受管 Binlog 配置；面板不会覆盖未受管配置。",
              )}
            </div>
          )}
          {status.configuration_state === "recovery_required" && (
            <div className="notice warning">
              <strong>{translate("上一次重启状态需要恢复")}</strong>
              <p>
                {translate(
                  "先核对受管配置与服务状态，再应用待处理配置或恢复先前配置。",
                )}
              </p>
              {!recoverCapability ? (
                <p>{translate("当前账户没有恢复此配置的权限。")}</p>
              ) : (
                <div className="button-row">
                  <Button
                    label={translate("继续应用待处理配置")}
                    onClick={() => {
                      setConfiguration(undefined);
                      setRecovery("apply_pending");
                    }}
                  />
                  <Button
                    label={translate("恢复先前配置")}
                    variant="ghost"
                    onClick={() => {
                      setConfiguration(undefined);
                      setRecovery("restore_previous");
                    }}
                  />
                </div>
              )}
            </div>
          )}
          {status.configuration_state === "settled" && configureCapability && (
            <div className="button-row">
              {!status.enabled && (
                <Button
                  label={translate("启用 Binlog")}
                  onClick={() => {
                    setRecovery(undefined);
                    setConfiguration(true);
                  }}
                />
              )}
              {status.enabled && (
                <Button
                  label={translate("停用 Binlog 配置")}
                  variant="ghost"
                  onClick={() => {
                    setRecovery(undefined);
                    setConfiguration(false);
                  }}
                />
              )}
            </div>
          )}
          {status.enabled && !configureCapability && (
            <p className="muted">
              {translate("当前账户没有修改 Binlog 配置的权限。")}
            </p>
          )}
          {status.enabled && configureCapability && (
            <p className="muted">
              {translate("停用受管 Binlog 配置不会删除已有日志文件。")}
            </p>
          )}
          {configuration !== undefined && configureCapability && (
            <ActionForm
              key={`binlog-config:${instanceID}:${status.config_revision}:${configuration}`}
              capability={configureCapability}
              preset={{
                instance_id: instanceID,
                enabled: configuration,
                expected_config_revision: status.config_revision,
                expected_instance_revision: status.instance_revision,
              }}
              lockedPresetFields={[
                "instance_id",
                "enabled",
                "expected_config_revision",
                "expected_instance_revision",
              ]}
              onBack={() => setConfiguration(undefined)}
              onExecuted={refresh}
            />
          )}
          {recovery && recoverCapability && (
            <ActionForm
              key={`binlog-recover:${instanceID}:${status.config_revision}:${recovery}`}
              capability={recoverCapability}
              preset={{
                instance_id: instanceID,
                command: recovery,
                expected_config_revision: status.config_revision,
                expected_instance_revision: status.instance_revision,
              }}
              lockedPresetFields={[
                "instance_id",
                "command",
                "expected_config_revision",
                "expected_instance_revision",
              ]}
              onBack={() => setRecovery(undefined)}
              onExecuted={refresh}
            />
          )}
        </>
      ) : statusResource.loading ? (
        <p role="status">{translate("正在读取 Binlog 状态…")}</p>
      ) : statusResource.data !== undefined ? (
        <Unavailable title={translate("Binlog 状态响应无效，请重新读取")} />
      ) : null}
      <div className="card-heading">
        <h4>{translate("现有日志")}</h4>
        {binlogs && (
          <span className="muted">
            {binlogs.total}
            {translate(" 个日志文件")}
            {binlogs.truncated ? translate(" · 当前页后还有日志") : ""}
          </span>
        )}
      </div>
      {!listCapability ? (
        <Unavailable title={translate("当前账户没有 Binlog 列表读取能力")} />
      ) : binlogs ? (
        <div className="instance-list">
          {binlogs.items.map((log) => (
            <div className="instance-row" key={log.name}>
              <div>
                <strong>{log.name}</strong>
                <p>
                  {bytes(log.size_bytes)}
                  {log.name === binlogs.active_log
                    ? translate(" · 当前活动日志")
                    : ""}
                </p>
              </div>
              {log.name === binlogs.active_log && <State value="running" />}
            </div>
          ))}
          {!binlogs.items.length && !binlogResource.loading && (
            <Unavailable title={translate("当前没有可见 Binlog 文件")} />
          )}
        </div>
      ) : binlogResource.loading ? (
        <p role="status">{translate("正在读取 Binlog 列表…")}</p>
      ) : binlogResource.data !== undefined ? (
        <Unavailable title={translate("Binlog 列表响应无效，请重新读取")} />
      ) : null}
      {binlogs && (after || binlogs.truncated) && (
        <div className="button-row">
          {after && (
            <Button
              label={translate("上一页")}
              variant="ghost"
              onClick={() => setPageCursors((current) => current.slice(0, -1))}
            />
          )}
          {binlogs.truncated && binlogs.next_after && (
            <Button
              label={translate("读取下一页")}
              variant="ghost"
              isDisabled={binlogResource.loading}
              onClick={() =>
                setPageCursors((current) => [...current, binlogs.next_after!])
              }
            />
          )}
          <Button
            label={translate("回到第一页")}
            variant="ghost"
            onClick={() => setPageCursors([""])}
          />
        </div>
      )}
      {previewCapability && listCapability && binlogs && (
        <section className="stack" aria-label={translate("Binlog 清理预览")}>
          <div className="card-heading">
            <h4>{translate("按保留位置预览清理")}</h4>
          </div>
          <p className="muted">
            {translate(
              "选择保留起点后，服务会冻结将清理的旧日志清单；当前活动日志和保留起点会保留。",
            )}
          </p>
          {!status?.configuration_managed && (
            <div className="notice warning">
              {translate(
                "此实例没有可验证的受管配置版本，不能安全绑定清理预览。",
              )}
            </div>
          )}
          {status?.configuration_managed && !canPreview && (
            <div className="notice warning">
              {translate(
                "Binlog 状态与列表版本不一致，请刷新后再生成清理预览。",
              )}
            </div>
          )}
          {binlogs.truncated && (
            <div className="notice warning">
              {translate(
                "日志列表不完整，当前不会生成清理预览。请轮换或整理日志后重新读取。",
              )}
            </div>
          )}
          {binlogs.total !== binlogs.items.length && (
            <div className="notice warning">
              {translate("日志总数与可见清单不一致，当前不会生成清理预览。")}
            </div>
          )}
          {binlogs.items.length > 128 && (
            <div className="notice warning">
              {translate(
                "当前有超过 128 个日志文件，服务端不会生成清理快照。请先轮换或整理日志，再重新读取。",
              )}
            </div>
          )}
          {binlogs.total > 128 && (
            <div className="notice warning">
              {translate(
                "Binlog 列表总数超过 128 个文件，服务端不会冻结清理快照；先轮换或整理日志。",
              )}
            </div>
          )}
          {binlogs.total === binlogs.items.length &&
            !binlogs.items.some((log) => log.name === binlogs.active_log) && (
              <div className="notice warning">
                {translate(
                  "活动 Binlog 未出现在读取清单中，当前不会生成清理预览。",
                )}
              </div>
            )}
          <label className="native-field">
            {translate("保留起点")}
            <select
              value={retainFrom}
              onChange={(event) => {
                setRetainFrom(event.target.value);
                setPreview(undefined);
                setAcknowledged(false);
              }}
              disabled={
                binlogs.truncated ||
                binlogs.items.length > 128 ||
                binlogs.total > 128 ||
                binlogs.total !== binlogs.items.length ||
                !binlogs.items.some((log) => log.name === binlogs.active_log)
              }
            >
              <option value="">{translate("选择要保留的第一个日志")}</option>
              {binlogs.items
                .slice(
                  0,
                  binlogs.items.findIndex(
                    (log) => log.name === binlogs.active_log,
                  ) + 1,
                )
                .map((log) => (
                  <option key={log.name} value={log.name}>
                    {log.name}
                    {log.name === binlogs.active_log
                      ? translate(" · 活动日志")
                      : ""}
                  </option>
                ))}
            </select>
          </label>
          {!purgeCapability && (
            <p className="muted">
              {translate("当前账户没有提交 Binlog 清理任务的权限。")}
            </p>
          )}
          <Button
            label={
              previewBusy
                ? translate("正在生成预览…")
                : translate("生成清理预览")
            }
            isDisabled={
              !retainFrom ||
              previewBusy ||
              !previewCapability ||
              !canPreview ||
              binlogs.truncated ||
              binlogs.items.length > 128 ||
              binlogs.total > 128 ||
              binlogs.total !== binlogs.items.length ||
              !binlogs.items.some((log) => log.name === binlogs.active_log)
            }
            onClick={() => void makePreview()}
          />
          {previewError && (
            <div className="notice danger" role="alert">
              {previewError}
            </div>
          )}
          {preview && (
            <div className="stack notice warning">
              <strong>
                {translate("清理预览 · 保留 ")}
                {preview.retain_from}
              </strong>
              {preview.binlogs_to_purge.length ? (
                <>
                  <p>
                    {translate("将清理")}
                    {preview.binlogs_to_purge.length}
                    {translate(" 个日志文件，约 ")}
                    {bytes(preview.bytes_to_purge)}
                    {translate("；活动日志 ")}
                    {preview.active_log}
                    {translate("将保留。")}
                  </p>
                  <ul>
                    {preview.binlogs_to_purge.map((log) => (
                      <li key={log.name}>
                        {log.name}· {bytes(log.size_bytes)}
                      </li>
                    ))}
                  </ul>
                </>
              ) : (
                <p>{translate("此保留位置没有可清理的旧日志。")}</p>
              )}
              <p className="muted">
                {translate("预览有效至")}
                {fullDate(preview.expires_at)}
                {translate(
                  "。服务器无法确认离线或未登记副本是否仍需要这些日志。",
                )}
              </p>
              {previewStale && (
                <p role="alert">
                  {translate(
                    "Binlog 配置或实例版本已变化，请重新读取并生成预览。",
                  )}
                </p>
              )}
              {purgeInputError && <p role="alert">{purgeInputError}</p>}
              {preview.binlogs_to_purge.length > 0 &&
                purgeCapability &&
                purgeInput &&
                !previewStale && (
                  <>
                    <label className="check-label">
                      <input
                        type="checkbox"
                        checked={acknowledged}
                        onChange={(event) =>
                          setAcknowledged(event.target.checked)
                        }
                      />
                      <span>
                        {translate(
                          "我确认这些文件可能包含离线或未登记副本需要的历史，并继续清理此精确清单。",
                        )}
                      </span>
                    </label>
                    {acknowledged && (
                      <ActionForm
                        key={`binlog-purge:${instanceID}:${preview.preview_id}`}
                        capability={purgeCapability}
                        preset={purgeInput}
                        lockedPresetFields={Object.keys(purgeInput)}
                        onBack={() => setAcknowledged(false)}
                        onExecuted={refresh}
                      />
                    )}
                  </>
                )}
            </div>
          )}
        </section>
      )}
    </section>
  );
}
