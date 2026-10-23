"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";
import { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { action, bytes, Capability, fullDate, intentKey } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import {
  DatabaseReplicationCapability,
  DatabaseReplicationPair,
  DatabaseReplicationPreview,
  DatabaseReplicationStatus,
  databaseReplicationCapability,
  databaseReplicationConfirmCapability,
  databaseReplicationLifecycleInput,
  databaseReplicationPairStatusInput,
  databaseReplicationPreviewInput,
  databaseReplicationRecoveryInput,
  parseDatabaseReplicationCapability,
  parseDatabaseReplicationPairs,
  parseDatabaseReplicationPreview,
  parseDatabaseReplicationStatus,
  parseReplicationDatabaseReceipt,
  parseReplicationEndpoint,
  replicationEngine,
  replicationInstanceCandidates,
  ReplicationDatabaseReceipt,
  ReplicationEndpoint,
  ReplicationEngine,
  ReplicationOperation,
  databaseReplicationTargetProtectionVerified,
} from "@/lib/database-replication";
import { TaskResult } from "./task-result";
import { State, Unavailable } from "./resource-ui";

const fields = {
  capability: ["source_instance_id", "target_instance_id"],
  pairs: ["instance_id"],
  preview: [
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_source_instance_revision",
    "expected_target_instance_revision",
    "expected_source_database_revision",
    "expected_target_database_revision",
  ],
  configure: [
    "pair_id",
    "preview_id",
    "snapshot_sha256",
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_source_instance_revision",
    "expected_target_instance_revision",
    "expected_source_database_revision",
    "expected_target_database_revision",
    "confirm_empty_target",
    "confirm_replica_history_loss",
    "confirm_target_read_only",
  ],
  status: [
    "pair_id",
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_pair_revision",
  ],
  lifecycle: [
    "pair_id",
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_pair_revision",
  ],
  remove: [
    "pair_id",
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_pair_revision",
    "confirm_remove",
    "confirm_replica_history_loss",
  ],
  recover: [
    "pair_id",
    "source_instance_id",
    "target_instance_id",
    "database",
    "expected_pair_revision",
    "recovery_choice",
  ],
};

function cap(
  capabilities: Capability[],
  name: string,
  scope: string,
  required: string[],
  task: boolean,
  readOnly: boolean,
) {
  return databaseReplicationCapability(
    capabilities,
    name,
    scope,
    required,
    task,
    readOnly,
  );
}

function errorMessage(error: unknown) {
  return error instanceof Error
    ? error.message
    : translate("数据库复制请求失败。");
}

function engineName(engine: ReplicationEngine) {
  return engine === "mysql" ? "MySQL" : "MariaDB";
}

function EndpointRows({
  source,
  target,
  database,
}: {
  source: string;
  target: string;
  database: string;
}) {
  return (
    <div className="detail-grid">
      <div>
        <small>{translate("复制源")}</small>
        <strong>{source}</strong>
        <span>{database}</span>
      </div>
      <div>
        <small>{translate("只读目标")}</small>
        <strong>{target}</strong>
        <span>{database}</span>
      </div>
    </div>
  );
}

export function DatabaseReplicationManagement({
  instanceID,
  database,
  engine: pluginEngine,
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
  const engine = replicationEngine(pluginEngine);
  const listPluginsCapability = cap(
    capabilities,
    "plugins_list",
    "software:read",
    [],
    false,
    true,
  );
  const getPluginCapability = cap(
    capabilities,
    "plugins_get",
    "software:read",
    ["id"],
    false,
    true,
  );
  const receiptCapability = cap(
    capabilities,
    "managed_database_receipt",
    "databases:read",
    ["instance_id", "database"],
    false,
    true,
  );
  const capabilitiesCapability = cap(
    capabilities,
    "managed_database_replication_capabilities",
    "databases:replication",
    fields.capability,
    false,
    true,
  );
  const pairsCapability = cap(
    capabilities,
    "managed_database_replication_pairs_list",
    "databases:replication",
    fields.pairs,
    false,
    true,
  );
  const previewCapability = cap(
    capabilities,
    "managed_database_replication_preview",
    "databases:admin",
    fields.preview,
    false,
    true,
  );
  const configureCapability = cap(
    capabilities,
    "managed_database_replication_configure",
    "databases:admin",
    fields.configure,
    true,
    false,
  );
  const statusCapability = cap(
    capabilities,
    "managed_database_replication_pair_status",
    "databases:replication",
    fields.status,
    false,
    true,
  );
  const startCapability = cap(
    capabilities,
    "managed_database_replication_start",
    "databases:admin",
    fields.lifecycle,
    true,
    false,
  );
  const stopCapability = cap(
    capabilities,
    "managed_database_replication_stop",
    "databases:admin",
    fields.lifecycle,
    true,
    false,
  );
  const removeCapability = cap(
    capabilities,
    "managed_database_replication_remove",
    "databases:admin",
    fields.remove,
    true,
    false,
  );
  const recoverCapability = cap(
    capabilities,
    "managed_database_replication_recover",
    "databases:admin",
    fields.recover,
    true,
    false,
  );
  const configureConfirmationsValid =
    databaseReplicationConfirmCapability(configureCapability);
  const recoveryEnum = recoverCapability?.input_schema?.properties as
    | Record<string, { enum?: unknown[] }>
    | undefined;
  const recoveryChoicesSchema = recoveryEnum?.recovery_choice?.enum;

  const [orientation, setOrientation] = useState<"source" | "target">("source");
  const [peerID, setPeerID] = useState("");
  const sourceID = orientation === "source" ? instanceID : peerID;
  const targetID = orientation === "source" ? peerID : instanceID;
  const endpointsSelected =
    !!engine &&
    !!sourceID &&
    !!targetID &&
    sourceID !== targetID &&
    !!getPluginCapability &&
    !!receiptCapability;

  const instanceList = useResource<unknown>(
    "plugins_list",
    {},
    active && !!engine && !!listPluginsCapability,
    30000,
  );
  const candidates = useMemo(
    () =>
      engine
        ? replicationInstanceCandidates(instanceList.data, instanceID, engine)
        : [],
    [engine, instanceList.data, instanceID],
  );
  const selectedPeerAvailable = candidates.some(
    (candidate) => candidate.id === peerID,
  );
  const sourceInstanceResource = useResource<unknown>(
    "plugins_get",
    { id: sourceID },
    active && endpointsSelected && !!getPluginCapability,
  );
  const targetInstanceResource = useResource<unknown>(
    "plugins_get",
    { id: targetID },
    active && endpointsSelected && !!getPluginCapability,
  );
  const sourceReceiptResource = useResource<unknown>(
    "managed_database_receipt",
    { instance_id: sourceID, database },
    active && endpointsSelected && !!receiptCapability,
  );
  const targetReceiptResource = useResource<unknown>(
    "managed_database_receipt",
    { instance_id: targetID, database },
    active && endpointsSelected && !!receiptCapability,
  );
  const sourceEndpoint = engine
    ? parseReplicationEndpoint(sourceInstanceResource.data, sourceID, engine)
    : undefined;
  const targetEndpoint = engine
    ? parseReplicationEndpoint(targetInstanceResource.data, targetID, engine)
    : undefined;
  const sourceReceipt = engine
    ? parseReplicationDatabaseReceipt(
        sourceReceiptResource.data,
        sourceID,
        database,
        engine,
      )
    : undefined;
  const targetReceipt = engine
    ? parseReplicationDatabaseReceipt(
        targetReceiptResource.data,
        targetID,
        database,
        engine,
      )
    : undefined;
  const endpointsReady =
    !!sourceEndpoint &&
    !!targetEndpoint &&
    !!sourceReceipt &&
    !!targetReceipt &&
    sourceEndpoint.engine === targetEndpoint.engine &&
    sourceReceipt.engine === targetReceipt.engine;

  const capabilityResource = useResource<unknown>(
    "managed_database_replication_capabilities",
    { source_instance_id: sourceID, target_instance_id: targetID },
    active && endpointsSelected && !!capabilitiesCapability,
    30000,
  );
  const pairListResource = useResource<unknown>(
    "managed_database_replication_pairs_list",
    { instance_id: instanceID },
    active && !!engine && !!pairsCapability,
    15000,
  );
  const parsedCapability = engine
    ? parseDatabaseReplicationCapability(
        capabilityResource.data,
        sourceID,
        targetID,
        engine,
      )
    : undefined;
  const pairList = parseDatabaseReplicationPairs(
    pairListResource.data,
    instanceID,
  );
  const relevantPairs =
    pairList?.items.filter((pair) => pair.database === database) ?? [];
  const [selectedPairID, setSelectedPairID] = useState("");
  useEffect(() => {
    if (!relevantPairs.length) {
      setSelectedPairID("");
      return;
    }
    if (!relevantPairs.some((pair) => pair.pair_id === selectedPairID))
      setSelectedPairID(relevantPairs[0].pair_id);
  }, [pairListResource.data, database, selectedPairID]);
  const selectedPair = relevantPairs.find(
    (pair) => pair.pair_id === selectedPairID,
  );
  const pairStatusResource = useResource<unknown>(
    "managed_database_replication_pair_status",
    selectedPair
      ? databaseReplicationPairStatusInput(selectedPair)
      : {
          pair_id: "",
          source_instance_id: "",
          target_instance_id: "",
          database,
          expected_pair_revision: 0,
        },
    active && !!selectedPair && !!statusCapability,
    15000,
  );
  const pairStatus = selectedPair
    ? parseDatabaseReplicationStatus(pairStatusResource.data, selectedPair)
    : undefined;

  const [preview, setPreview] = useState<DatabaseReplicationPreview>();
  const [confirmEmpty, setConfirmEmpty] = useState(false);
  const [confirmHistory, setConfirmHistory] = useState(false);
  const [confirmReadOnly, setConfirmReadOnly] = useState(false);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const [actionError, setActionError] = useState("");
  const [actionBusy, setActionBusy] = useState(false);
  const [task, setTask] = useState<unknown>();
  const [taskPending, setTaskPending] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [confirmRemoveHistory, setConfirmRemoveHistory] = useState(false);
  const [now, setNow] = useState(Date.now());
  const taskIntent = useRef<{ fingerprint: string; key: string } | undefined>(
    undefined,
  );
  const [notice, setNotice] = useState("");

  useEffect(() => {
    if (!preview) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [preview]);
  useEffect(() => {
    setPreview(undefined);
    setConfirmEmpty(false);
    setConfirmHistory(false);
    setConfirmReadOnly(false);
    setPreviewError("");
    setNotice("");
  }, [sourceID, targetID, database]);
  useEffect(() => {
    setConfirmRemove(false);
    setConfirmRemoveHistory(false);
  }, [selectedPairID, pairStatus?.pair_revision]);

  const refresh = () => {
    void instanceList.reload();
    void capabilityResource.reload();
    void pairListResource.reload();
    void pairStatusResource.reload();
    onExecuted();
  };

  const resetPreview = () => {
    setPreview(undefined);
    setConfirmEmpty(false);
    setConfirmHistory(false);
    setConfirmReadOnly(false);
    setPreviewError("");
  };

  const readFreshEndpointPins = async (
    source: string,
    target: string,
  ): Promise<{
    sourceEndpoint: ReplicationEndpoint;
    targetEndpoint: ReplicationEndpoint;
    sourceDatabase: ReplicationDatabaseReceipt;
    targetDatabase: ReplicationDatabaseReceipt;
  }> => {
    if (!getPluginCapability || !receiptCapability || !engine)
      throw new Error(
        translate(
          "需要读取两个软件实例与同名数据库的权威版本。请检查软件读取和数据库读取权限。",
        ),
      );
    const [sourceInstance, targetInstance, sourceReceipt, targetReceipt] =
      await Promise.all([
        action<unknown>("plugins_get", { id: source }),
        action<unknown>("plugins_get", { id: target }),
        action<unknown>("managed_database_receipt", {
          instance_id: source,
          database,
        }),
        action<unknown>("managed_database_receipt", {
          instance_id: target,
          database,
        }),
      ]);
    const parsedSource = parseReplicationEndpoint(
      sourceInstance,
      source,
      engine,
    );
    const parsedTarget = parseReplicationEndpoint(
      targetInstance,
      target,
      engine,
    );
    const parsedSourceDatabase = parseReplicationDatabaseReceipt(
      sourceReceipt,
      source,
      database,
      engine,
    );
    const parsedTargetDatabase = parseReplicationDatabaseReceipt(
      targetReceipt,
      target,
      database,
      engine,
    );
    if (
      !parsedSource ||
      !parsedTarget ||
      !parsedSourceDatabase ||
      !parsedTargetDatabase
    )
      throw new Error(
        translate(
          "端点或数据库权威记录缺少有效归属、运行状态或版本；请刷新并确认两端数据库都由面板管理且处于 active 状态。",
        ),
      );
    return {
      sourceEndpoint: parsedSource,
      targetEndpoint: parsedTarget,
      sourceDatabase: parsedSourceDatabase,
      targetDatabase: parsedTargetDatabase,
    };
  };

  const inspectPairCapability = async (source: string, target: string) => {
    if (!capabilitiesCapability)
      throw new Error(
        translate(
          "当前身份没有数据库复制能力检查权限 (databases:replication)。",
        ),
      );
    const value = await action<unknown>(
      "managed_database_replication_capabilities",
      {
        source_instance_id: source,
        target_instance_id: target,
      },
    );
    const parsed = parseDatabaseReplicationCapability(
      value,
      source,
      target,
      engine,
    );
    if (!parsed)
      throw new Error(
        translate("服务返回的复制能力结果与所选实例不匹配，请刷新后重试。"),
      );
    if (!parsed.supported)
      throw new Error(
        parsed.reason ||
          translate("服务端未确认此实例对符合安全条件的托管复制。"),
      );
    return parsed;
  };

  const makePreview = async () => {
    if (
      !previewCapability ||
      !endpointsSelected ||
      !peerID ||
      !engine ||
      !pairListResource.data ||
      !pairList ||
      pairListResource.error
    )
      return;
    setPreviewBusy(true);
    setPreviewError("");
    setNotice("");
    resetPreview();
    try {
      const freshPairsRaw = await action<unknown>(
        "managed_database_replication_pairs_list",
        { instance_id: instanceID },
      );
      const freshPairs = parseDatabaseReplicationPairs(
        freshPairsRaw,
        instanceID,
      );
      if (!freshPairs)
        throw new Error(
          translate("无法确认当前已登记的复制对清单，请刷新后再试。"),
        );
      const targetAlreadyPaired = freshPairs.items.some(
        (pair) =>
          pair.target_instance_id === targetID && pair.database === database,
      );
      if (targetAlreadyPaired)
        throw new Error(
          translate(
            "所选目标数据库已有托管复制记录。请在下方选择该复制对并查看状态；不会创建第二个目标通道。",
          ),
        );
      const pins = await readFreshEndpointPins(sourceID, targetID);
      const discovered = await inspectPairCapability(sourceID, targetID);
      if (
        !discovered.actions.includes("preview") ||
        !discovered.actions.includes("configure")
      )
        throw new Error(translate("服务端没有为这对实例发布预览和配置能力。"));
      const raw = await action<unknown>(
        "managed_database_replication_preview",
        {
          source_instance_id: sourceID,
          target_instance_id: targetID,
          database,
          expected_source_instance_revision: pins.sourceEndpoint.revision,
          expected_target_instance_revision: pins.targetEndpoint.revision,
          expected_source_database_revision: pins.sourceDatabase.revision,
          expected_target_database_revision: pins.targetDatabase.revision,
        },
      );
      const parsed = parseDatabaseReplicationPreview(raw, {
        source: pins.sourceEndpoint,
        target: pins.targetEndpoint,
        database,
        sourceDatabase: pins.sourceDatabase,
        targetDatabase: pins.targetDatabase,
      });
      if (!parsed)
        throw new Error(
          translate(
            "服务返回的复制预览缺少完整版本、哈希或目标保护字段；没有提交配置任务。",
          ),
        );
      setPreview(parsed);
      setNow(Date.now());
    } catch (error) {
      setPreviewError(errorMessage(error));
      void pairListResource.reload();
    } finally {
      setPreviewBusy(false);
    }
  };

  const acknowledgments = {
    emptyTarget: confirmEmpty,
    replicaHistory: confirmHistory,
    targetReadOnly: confirmReadOnly,
  };
  const previewInput = preview
    ? databaseReplicationPreviewInput(preview, acknowledgments, now)
    : undefined;
  const previewExpired = !!preview && Date.parse(preview.expires_at) <= now;
  const allConfigureChecks = confirmEmpty && confirmHistory && confirmReadOnly;

  const submitTask = async (name: string, input: Record<string, unknown>) => {
    const fingerprint = `${name}:${JSON.stringify(input)}`;
    const existingIntent = taskIntent.current;
    const key =
      existingIntent?.fingerprint === fingerprint
        ? existingIntent.key
        : intentKey();
    taskIntent.current = { fingerprint, key };
    setActionBusy(true);
    setActionError("");
    setNotice("");
    try {
      const accepted = await action<unknown>(name, input, {
        idempotencyKey: key,
      });
      setTask(accepted);
      setTaskPending(true);
      setNotice(
        translate(
          "任务已提交；请查看任务状态和结果。失败或中断时先刷新复制状态，再决定是否执行下一步。",
        ),
      );
      refresh();
      return true;
    } catch (error) {
      setActionError(errorMessage(error));
      setNotice(
        translate(
          "请求没有得到可确认的任务回执。已刷新状态；如需重试，请先检查当前复制对状态。此页面不会自动重放任务。",
        ),
      );
      refresh();
      return false;
    } finally {
      setActionBusy(false);
    }
  };

  const configure = async () => {
    if (
      !configureCapability ||
      !configureConfirmationsValid ||
      !previewInput ||
      !allConfigureChecks ||
      !engine
    )
      return;
    setActionBusy(true);
    setActionError("");
    setNotice("");
    try {
      const pins = await readFreshEndpointPins(sourceID, targetID);
      if (
        pins.sourceEndpoint.revision !== preview?.source_instance_revision ||
        pins.targetEndpoint.revision !== preview?.target_instance_revision ||
        pins.sourceDatabase.revision !== preview?.source_database_revision ||
        pins.targetDatabase.revision !== preview?.target_database_revision
      ) {
        resetPreview();
        throw new Error(
          translate(
            "实例或数据库版本已变化，旧预览不能提交。请重新读取并生成新预览。",
          ),
        );
      }
      const discovered = await inspectPairCapability(sourceID, targetID);
      if (!discovered.actions.includes("configure"))
        throw new Error(translate("服务端当前没有发布此复制配置能力。"));
      const input = databaseReplicationPreviewInput(
        preview,
        acknowledgments,
        Date.now(),
      );
      if (!input || !allConfigureChecks)
        throw new Error(
          translate("预览已过期或三项确认未完成。请重新预览并分别确认。"),
        );
      const accepted = await submitTask(
        "managed_database_replication_configure",
        input,
      );
      if (accepted) resetPreview();
    } catch (error) {
      setActionError(errorMessage(error));
      setNotice(
        translate(
          "没有自动重试配置任务。请刷新端点与复制状态，检查预览是否仍然有效。",
        ),
      );
      refresh();
    } finally {
      setActionBusy(false);
    }
  };

  const executeLifecycle = async (
    operation: ReplicationOperation | "recover",
    recoveryChoice?: "apply_pending" | "restore_previous",
  ) => {
    if (!selectedPair || !statusCapability || !pairListResource.data) return;
    const requiredCapability =
      operation === "start"
        ? startCapability
        : operation === "stop"
          ? stopCapability
          : operation === "remove"
            ? removeCapability
            : recoverCapability;
    if (!requiredCapability) {
      setActionError(
        translate(
          "当前身份没有与此操作匹配的数据库管理能力；服务端还会检查两端软件和精确数据库资源授权。",
        ),
      );
      return;
    }
    setActionBusy(true);
    setActionError("");
    setNotice("");
    try {
      const freshListRaw = await action<unknown>(
        "managed_database_replication_pairs_list",
        { instance_id: instanceID },
      );
      const freshList = parseDatabaseReplicationPairs(freshListRaw, instanceID);
      const freshPair = freshList?.items.find(
        (item) => item.pair_id === selectedPair.pair_id,
      );
      if (!freshPair)
        throw new Error(
          translate("复制对已从授权清单中消失。请刷新并重新选择。"),
        );
      const freshStatusRaw = await action<unknown>(
        "managed_database_replication_pair_status",
        databaseReplicationPairStatusInput(freshPair),
      );
      const freshStatus = parseDatabaseReplicationStatus(
        freshStatusRaw,
        freshPair,
      );
      if (!freshStatus || freshStatus.pair_revision !== freshPair.pair_revision)
        throw new Error(
          translate("复制对状态与最新版本不匹配。请刷新状态后再提交。"),
        );
      let input: Record<string, unknown> | undefined;
      if (operation === "recover") {
        if (!recoveryChoice)
          throw new Error(
            translate("服务端未明确允许的恢复选择；当前不会提交恢复任务。"),
          );
        input = databaseReplicationRecoveryInput(freshStatus, recoveryChoice);
      } else {
        input = databaseReplicationLifecycleInput(
          freshStatus,
          operation,
          operation === "remove" && confirmRemove && confirmRemoveHistory,
        );
      }
      if (!input)
        throw new Error(
          translate(
            "复制对状态不允许此操作，或需要重新确认后端明确列出的恢复选项。",
          ),
        );
      await submitTask(requiredCapability.name, input);
    } catch (error) {
      setActionError(errorMessage(error));
      setNotice(
        translate(
          "操作没有自动重试。已请求刷新复制对；如果版本冲突，请重新读取状态后再决定下一步。",
        ),
      );
      refresh();
    } finally {
      setActionBusy(false);
    }
  };

  const existingTargetPair = pairList?.items.find(
    (pair) =>
      pair.target_instance_id === targetID && pair.database === database,
  );
  const capabilityUnsupported = parsedCapability && !parsedCapability.supported;
  const capabilitySupportsPreview =
    parsedCapability?.supported === true &&
    parsedCapability.actions.includes("preview") &&
    parsedCapability.actions.includes("configure");
  const statusCanStart =
    !!pairStatus && !!databaseReplicationLifecycleInput(pairStatus, "start");
  const statusCanStop =
    !!pairStatus && !!databaseReplicationLifecycleInput(pairStatus, "stop");
  const statusCanRemove =
    !!pairStatus &&
    !!removeCapability &&
    pairStatus.phase === "settled" &&
    !pairStatus.recovery_required &&
    ["running", "stopped", "degraded"].includes(pairStatus.state);
  const allowedRecoveryChoices = pairStatus?.allowed_recovery_choices ?? [];
  const recoveryChoiceSupported = (
    choice: "apply_pending" | "restore_previous",
  ) =>
    allowedRecoveryChoices.includes(choice) &&
    Array.isArray(recoveryChoicesSchema) &&
    recoveryChoicesSchema.includes(choice);
  const targetProtected = pairStatus
    ? databaseReplicationTargetProtectionVerified(pairStatus)
    : false;

  if (!engine) {
    return (
      <section className="stack" aria-label={translate("数据库复制管理")}>
        <SectionHeader icon="settings" className="card-heading">
          <h3>{translate("数据库复制")}</h3>
        </SectionHeader>
        <Unavailable
          title={translate("{0}暂不支持托管复制", [
            pluginEngine || translate("此数据库引擎"),
          ])}
          detail={translate(
            "当前只支持同一服务器上的托管 MySQL↔MySQL 或 MariaDB↔MariaDB。PostgreSQL、MongoDB、跨引擎和跨主机复制当前没有受管动作。",
          )}
        />
      </section>
    );
  }

  return (
    <section
      className="stack"
      aria-label={translate("MySQL 和 MariaDB 托管复制")}
    >
      <SectionHeader icon="settings" className="card-heading">
        <div>
          <h3>{translate("托管数据库复制")}</h3>
          <p className="muted">
            {translate("同主机、同引擎的单向复制；目标实例将被整体设为只读。")}
          </p>
        </div>
        <Button
          label={translate("刷新复制状态")}
          variant="ghost"
          isDisabled={pairListResource.loading || pairStatusResource.loading}
          onClick={refresh}
        />
      </SectionHeader>
      <p className="muted">
        {translate("当前数据库")}
        <code>{database}</code>
        {translate(
          "。两个实例都必须由面板管理，并且目标实例只能包含此数据库；开始配置前，服务端还会重新检查运行状态、目标空库和资源授权。复制状态读取需要两端的 ",
        )}
        <code>databases:replication</code>
        {translate(" 与精确数据库资源；配置和生命周期操作需要两端的 ")}
        <code>databases:admin</code>
        {translate("授权。")}
      </p>
      {instanceList.error && (
        <div className="notice danger" role="alert">
          {instanceList.error}
        </div>
      )}
      {capabilityResource.error && (
        <div className="notice danger" role="alert">
          {capabilityResource.error}
        </div>
      )}
      {pairListResource.error && (
        <div className="notice danger" role="alert">
          {pairListResource.error}
        </div>
      )}
      {pairStatusResource.error && (
        <div className="notice danger" role="alert">
          {pairStatusResource.error}
        </div>
      )}
      {(actionError || previewError) && (
        <div className="notice danger" role="alert">
          {actionError || previewError}
        </div>
      )}
      {notice && (
        <div className="notice warning" role="status">
          {notice}
        </div>
      )}
      <SectionHeader icon="settings" className="card-heading">
        <h4>{translate("新建复制对")}</h4>
      </SectionHeader>
      {!listPluginsCapability || !getPluginCapability || !receiptCapability ? (
        <Unavailable
          title={translate("缺少端点或数据库读取能力")}
          detail={translate(
            "选择复制端点需要软件实例列表与详情读取、以及两端同名数据库的受管记录读取权限。",
          )}
        />
      ) : (
        <div className="detail-grid">
          <label className="native-field">
            {translate("当前实例角色")}
            <select
              value={orientation}
              disabled={actionBusy || taskPending}
              onChange={(event) => {
                setOrientation(event.target.value as "source" | "target");
                resetPreview();
              }}
            >
              <option value="source">{translate("当前实例作为复制源")}</option>
              <option value="target">
                {translate("当前实例作为只读目标")}
              </option>
            </select>
          </label>
          <label className="native-field">
            {orientation === "source"
              ? translate("选择只读目标实例")
              : translate("选择复制源实例")}
            <select
              value={peerID}
              disabled={actionBusy || taskPending || instanceList.loading}
              onChange={(event) => {
                setPeerID(event.target.value);
                resetPreview();
              }}
            >
              <option value="">
                {translate("选择另一台已授权的 ")}
                {engineName(engine)}
                {translate(" 实例")}
              </option>
              {candidates.map((candidate) => (
                <option key={candidate.id} value={candidate.id}>
                  {candidate.id}· {candidate.plugin_id}
                </option>
              ))}
            </select>
          </label>
        </div>
      )}
      {!instanceList.loading &&
        !!listPluginsCapability &&
        !candidates.length && (
          <p className="muted">
            {translate("没有其他可见的受管 ")}
            {engineName(engine)}
            {translate(" 实例。列表只包含当前身份可读取的实例。")}
          </p>
        )}
      {peerID && !selectedPeerAvailable && (
        <div className="notice warning">
          {translate(
            "所选实例已不在当前授权的运行实例清单中；重新选择后再继续。",
          )}
        </div>
      )}
      {peerID && (
        <EndpointRows source={sourceID} target={targetID} database={database} />
      )}
      {peerID &&
        ((!sourceInstanceResource.loading && !sourceEndpoint) ||
          (!targetInstanceResource.loading && !targetEndpoint)) && (
          <div className="notice warning">
            {translate(
              "读取的实例详情尚未证明两端都是面板管理的可用实例；请检查各自的读取权限和服务状态。",
            )}
          </div>
        )}
      {peerID &&
        ((!sourceReceiptResource.loading && !sourceReceipt) ||
          (!targetReceiptResource.loading && !targetReceipt)) && (
          <div className="notice warning">
            {translate(
              "两端都必须存在同名且 active 的受管数据库记录。目标不会被自动创建或接管。",
            )}
          </div>
        )}
      {endpointsSelected && !capabilitiesCapability && (
        <Unavailable
          title={translate("当前身份无法检查两端复制能力")}
          detail={translate(
            "缺少 databases:replication 读取动作；服务端也会验证两个端点及精确数据库资源。",
          )}
        />
      )}
      {endpointsSelected && capabilityUnsupported && (
        <div className="notice warning" role="status">
          {translate("此端点组合不满足服务端支持条件")}
          {parsedCapability.reason ? `：${parsedCapability.reason}` : "。"}
        </div>
      )}
      {endpointsSelected &&
        !capabilityUnsupported &&
        !!capabilityResource.data &&
        !parsedCapability && (
          <div className="notice danger" role="alert">
            {translate("复制能力响应身份或字段不匹配；没有启用提交操作。")}
          </div>
        )}
      {existingTargetPair && (
        <div className="notice warning">
          {translate("目标实例已有此数据库的复制记录（")}
          {existingTargetPair.source_instance_id}→{" "}
          {existingTargetPair.target_instance_id}
          {translate("，状态 ")}
          {existingTargetPair.state}
          {translate(
            "）。不会为同一个目标创建第二条通道；请在下方检查现有复制对。",
          )}
        </div>
      )}
      {!pairsCapability && (
        <p className="muted">
          {translate(
            "当前身份没有读取受管复制对清单的权限，因此不能安全确认目标是否已使用；新建预览已禁用。",
          )}
        </p>
      )}
      {pairsCapability && !!pairListResource.data && !pairList && (
        <div className="notice danger" role="alert">
          {translate("复制对清单响应不完整；新建预览已禁用，请刷新后检查。")}
        </div>
      )}
      <Button
        label={
          previewBusy
            ? translate("正在刷新端点并生成预览…")
            : translate("读取最新端点并生成预览")
        }
        isDisabled={
          previewBusy ||
          actionBusy ||
          taskPending ||
          !peerID ||
          !selectedPeerAvailable ||
          !endpointsReady ||
          !capabilitiesCapability ||
          !pairsCapability ||
          !pairList ||
          !!pairListResource.error ||
          !previewCapability ||
          !capabilitySupportsPreview ||
          !!existingTargetPair
        }
        onClick={() => void makePreview()}
      />
      {capabilityResource.loading && endpointsSelected && (
        <p role="status">{translate("正在核对服务器复制能力…")}</p>
      )}
      {capabilityResource.error && endpointsSelected && (
        <p className="muted">
          {translate(
            "服务端能力检查失败；请检查当前 actor 对两端及精确数据库资源的读取授权。",
          )}
        </p>
      )}
      {!previewCapability && endpointsSelected && (
        <p className="muted">
          {translate("当前身份没有生成复制预览所需的 databases:admin 权限。")}
        </p>
      )}
      {previewExpired && (
        <div className="notice warning" role="alert">
          {translate("预览已过期；重新读取两端版本并生成新预览后才能提交。")}
        </div>
      )}
      {preview && (
        <div className="stack">
          <div className="detail-grid">
            <div>
              <small>{translate("冻结预览")}</small>
              <strong>
                {engineName(preview.engine)}· {preview.table_count}
                {translate(" 张表")}
              </strong>
              <span>
                {translate("预计种子 ")}
                {bytes(preview.estimated_seed_bytes)}
              </span>
            </div>
            <div>
              <small>{translate("预览有效期")}</small>
              <strong>{fullDate(preview.expires_at)}</strong>
              <span>Pair {preview.pair_id}</span>
            </div>
            <div>
              <small>{translate("实例版本")}</small>
              <strong>
                {preview.source_instance_revision}→{" "}
                {preview.target_instance_revision}
              </strong>
              <span>
                {translate("数据库版本 ")}
                {preview.source_database_revision}→{" "}
                {preview.target_database_revision}
              </span>
            </div>
            <div>
              <small>{translate("种子快照 SHA-256")}</small>
              <strong className="monospace">{preview.snapshot_sha256}</strong>
              <span>Schema {preview.schema_sha256.slice(0, 16)}…</span>
            </div>
          </div>
          <div className="notice warning">
            {translate(
              "目标实例级只读：服务端报告此实例仅包含所选数据库。MySQL 将同时启用",
            )}
            <code>read_only</code>
            {translate(" 与 ")}
            <code>super_read_only</code>
            {translate("；MariaDB 将启用 ")}
            <code>read_only</code>
            {translate("。这会影响目标实例中的全部数据库。")}
          </div>
          <label className="native-field">
            <input
              type="checkbox"
              checked={confirmEmpty}
              onChange={(event) => setConfirmEmpty(event.target.checked)}
              disabled={previewExpired || actionBusy || taskPending}
            />
            {translate(
              "我确认这是一个受管的空目标数据库；预览不会自动创建或覆盖数据库。",
            )}
          </label>
          <label className="native-field">
            <input
              type="checkbox"
              checked={confirmHistory}
              onChange={(event) => setConfirmHistory(event.target.checked)}
              disabled={previewExpired || actionBusy || taskPending}
            />
            {translate(
              "我确认将从本次冻结的快照及其 binlog 文件/位置启动一条新的单向复制流；已有复制通道或位置不会被接管，存在时服务端会拒绝预览或配置。",
            )}
          </label>
          <label className="native-field">
            <input
              type="checkbox"
              checked={confirmReadOnly}
              onChange={(event) => setConfirmReadOnly(event.target.checked)}
              disabled={previewExpired || actionBusy || taskPending}
            />
            {translate(
              "我确认目标实例会整体变为只读，不能在目标上直接写入；该保护影响目标实例的所有数据库。",
            )}
          </label>
          {!configureConfirmationsValid && (
            <div className="notice danger">
              {translate(
                "配置动作的公共 schema 未要求三项常量为 true 的确认字段；安全提交保持禁用。",
              )}
            </div>
          )}
          {!configureCapability && (
            <p className="muted">
              {translate(
                "当前身份没有提交配置任务所需的 databases:admin 授权。",
              )}
            </p>
          )}
          <Button
            label={
              actionBusy
                ? translate("正在核对版本…")
                : translate("按冻结预览提交配置任务")
            }
            variant="primary"
            isDisabled={
              actionBusy ||
              taskPending ||
              previewExpired ||
              !previewInput ||
              !allConfigureChecks ||
              !configureCapability ||
              !configureConfirmationsValid
            }
            onClick={() => void configure()}
          />
        </div>
      )}
      {relevantPairs.length > 0 && (
        <div className="stack">
          <SectionHeader icon="settings" className="card-heading">
            <h4>{translate("已登记复制对")}</h4>
          </SectionHeader>
          <div className="instance-list">
            {relevantPairs.map((pair) => (
              <button
                key={pair.pair_id}
                className="instance-row"
                aria-pressed={pair.pair_id === selectedPairID}
                onClick={() => {
                  setSelectedPairID(pair.pair_id);
                  setConfirmRemove(false);
                  setConfirmRemoveHistory(false);
                  setActionError("");
                }}
              >
                <div>
                  <strong>
                    {pair.source_instance_id}→ {pair.target_instance_id}
                  </strong>
                  <p>
                    {engineName(pair.engine)}· {pair.database}·{" "}
                    <State value={pair.state} />
                  </p>
                  <small className="monospace">
                    {pair.pair_id}· revision {pair.pair_revision}
                  </small>
                </div>
              </button>
            ))}
          </div>
        </div>
      )}
      {pairsCapability && pairList && relevantPairs.length === 0 && (
        <Unavailable
          title={translate("当前数据库没有已登记复制对")}
          detail={translate(
            "仅显示当前 actor 对两端实例和精确数据库都获授权的复制记录。",
          )}
        />
      )}
      {!statusCapability && relevantPairs.length > 0 && (
        <Unavailable
          title={translate("当前身份没有读取复制状态的权限")}
          detail={translate(
            "需要 databases:replication 读取权限和复制对两端的精确资源授权。",
          )}
        />
      )}
      {selectedPair && statusCapability && (
        <div className="stack">
          <SectionHeader icon="settings" className="card-heading">
            <div>
              <h4>{translate("复制对状态")}</h4>
              <p className="muted">
                {translate("状态请求使用当前受授权列表中的复制对版本。")}
              </p>
            </div>
            <Button
              label={translate("重新读取此复制对")}
              variant="ghost"
              isDisabled={pairStatusResource.loading}
              onClick={() => void pairStatusResource.reload()}
            />
          </SectionHeader>
          {!pairStatus && !!pairStatusResource.data && (
            <div className="notice danger" role="alert">
              {translate(
                "服务端状态身份、版本或字段与选中的复制对不匹配；不会显示生命周期操作。",
              )}
            </div>
          )}
          {pairStatus && (
            <>
              <EndpointRows
                source={pairStatus.source_instance_id}
                target={pairStatus.target_instance_id}
                database={pairStatus.database}
              />
              <div className="detail-grid">
                <div>
                  <small>{translate("复制状态 / 阶段")}</small>
                  <State value={pairStatus.state} />
                  <span>{pairStatus.phase}</span>
                </div>
                <div>
                  <small>{translate("Pair 版本")}</small>
                  <strong>{pairStatus.pair_revision}</strong>
                  <span>{pairStatus.channel}</span>
                </div>
                <div>
                  <small>{translate("目标写入保护")}</small>
                  <State value={targetProtected ? "verified" : "unhealthy"} />
                  <span>
                    {pairStatus.engine === "mysql"
                      ? `read_only ${pairStatus.target_read_only ? "on" : "off"} · super_read_only ${pairStatus.target_super_read_only ? "on" : "off"}`
                      : `read_only ${pairStatus.target_read_only ? "on" : "off"}`}
                  </span>
                  {!pairStatus.target_protection_verified && (
                    <span className="muted">
                      {translate("服务端尚未验证受管只读配置。")}
                    </span>
                  )}
                </div>
                <div>
                  <small>{translate("复制线程 / 延迟")}</small>
                  <strong>
                    IO{" "}
                    {pairStatus.io_thread_running
                      ? translate("运行")
                      : translate("停止")}
                    · SQL{" "}
                    {pairStatus.sql_thread_running
                      ? translate("运行")
                      : translate("停止")}
                  </strong>
                  <span>
                    {pairStatus.seconds_behind_source === undefined
                      ? translate("延迟未知")
                      : translate("{0} 秒", [pairStatus.seconds_behind_source])}
                  </span>
                </div>
                <div>
                  <small>{translate("源坐标")}</small>
                  <strong>
                    {pairStatus.source_log_file || translate("未提供")}
                  </strong>
                  <span>
                    {pairStatus.source_log_position
                      ? translate("位置 {0}", [pairStatus.source_log_position])
                      : translate("位置未提供")}
                  </span>
                </div>
                <div>
                  <small>{translate("种子")}</small>
                  <strong>
                    {pairStatus.seed_bytes === undefined
                      ? translate("未提供")
                      : bytes(pairStatus.seed_bytes)}
                  </strong>
                  <span className="monospace">
                    {pairStatus.seed_sha256 || translate("SHA-256 未提供")}
                  </span>
                </div>
              </div>
              {pairStatus.recovery_required && (
                <div className="notice warning" role="alert">
                  {translate(
                    "该复制对有未完成的受保护操作，需要显式恢复。恢复选项仅在后端状态响应明确列出时显示；当前不会根据阶段猜测回滚安全性。",
                  )}
                  {pairStatus.operation && (
                    <span>
                      {translate(" 当前操作：")}
                      {pairStatus.operation}。
                    </span>
                  )}
                </div>
              )}
              {pairStatus.error_class && (
                <div className="notice danger" role="alert">
                  {translate("服务端错误类别：")}
                  <code>{pairStatus.error_class}</code>
                </div>
              )}
              {!targetProtected && !pairStatus.recovery_required && (
                <div className="notice danger" role="alert">
                  {translate(
                    "目标只读保护未得到服务端验证。界面不会将目标标记为安全只读。",
                  )}
                </div>
              )}
              <div className="button-row">
                {startCapability && statusCanStart && targetProtected && (
                  <Button
                    label={translate("启动复制")}
                    isDisabled={actionBusy || taskPending}
                    onClick={() => void executeLifecycle("start")}
                  />
                )}
                {stopCapability && statusCanStop && (
                  <Button
                    label={translate("停止复制并保留状态")}
                    isDisabled={actionBusy || taskPending}
                    onClick={() => void executeLifecycle("stop")}
                  />
                )}
              </div>
              {statusCanRemove && removeCapability && (
                <div className="stack">
                  <div className="notice danger">
                    {translate(
                      "移除会停止复制并删除此托管通道、生成的复制账户及源坐标。目标现有数据库数据会保留，但之后不再接收更新，也不能从此复制历史继续；源实例不变。",
                    )}
                  </div>
                  <label className="native-field">
                    <input
                      type="checkbox"
                      checked={confirmRemove}
                      onChange={(event) =>
                        setConfirmRemove(event.target.checked)
                      }
                      disabled={actionBusy || taskPending}
                    />
                    {translate("我确认移除此托管复制通道和生成的复制账户。")}
                  </label>
                  <label className="native-field">
                    <input
                      type="checkbox"
                      checked={confirmRemoveHistory}
                      onChange={(event) =>
                        setConfirmRemoveHistory(event.target.checked)
                      }
                      disabled={actionBusy || taskPending}
                    />
                    {translate(
                      "我确认目标将停止接收更新，已生成的复制历史与源坐标不会保留为可恢复通道；目标现有数据保留，源不受影响。",
                    )}
                  </label>
                  <Button
                    label={translate("移除复制通道并保留目标数据")}
                    variant="destructive"
                    isDisabled={
                      actionBusy ||
                      taskPending ||
                      !confirmRemove ||
                      !confirmRemoveHistory
                    }
                    onClick={() => void executeLifecycle("remove")}
                  />
                </div>
              )}
              {pairStatus.recovery_required && recoverCapability && (
                <div className="button-row">
                  {(["apply_pending", "restore_previous"] as const).map(
                    (choice) => {
                      const allowed = recoveryChoiceSupported(choice);
                      if (!allowed) return null;
                      return (
                        <Button
                          key={choice}
                          label={
                            choice === "apply_pending"
                              ? translate("继续已记录的安全步骤")
                              : translate("恢复配置前版本")
                          }
                          isDisabled={
                            actionBusy ||
                            taskPending ||
                            !databaseReplicationRecoveryInput(
                              pairStatus,
                              choice,
                            )
                          }
                          onClick={() =>
                            void executeLifecycle("recover", choice)
                          }
                        />
                      );
                    },
                  )}
                  {!allowedRecoveryChoices.length && (
                    <p className="muted">
                      {translate(
                        "后端尚未声明安全恢复选项；刷新或由管理员检查受保护的复制日志后再操作。",
                      )}
                    </p>
                  )}
                </div>
              )}
              {pairStatus.phase !== "settled" &&
                !pairStatus.recovery_required && (
                  <div className="notice warning">
                    {translate(
                      "操作阶段尚未 settled。此时不会显示 start、stop 或 remove；请等待任务完成并重新读取状态。",
                    )}
                  </div>
                )}
            </>
          )}
        </div>
      )}
      {task !== undefined && (
        <TaskResult
          key={JSON.stringify(task)}
          submitted={task}
          capabilities={capabilities}
          onFinished={() => {
            setTaskPending(false);
            taskIntent.current = undefined;
            refresh();
          }}
          onObservationUnavailable={() => {
            setTaskPending(false);
            setNotice(
              translate(
                "复制任务已提交，但当前身份无法读取任务状态；执行结果未知。可以手动刷新复制状态，但不会把任务标记为成功。",
              ),
            );
            refresh();
          }}
        />
      )}
    </section>
  );
}
