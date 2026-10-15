import type { Capability } from "./api";

export type ReplicationEngine = "mysql" | "mariadb";
export type ReplicationOperation = "start" | "stop" | "remove";

export type ReplicationEndpoint = {
  id: string;
  plugin_id: string;
  engine: ReplicationEngine;
  revision: number;
};

export type ReplicationDatabaseReceipt = {
  instance_id: string;
  database: string;
  engine: ReplicationEngine;
  revision: number;
  state: "active";
};

export type DatabaseReplicationCapability = {
  source_instance_id: string;
  target_instance_id: string;
  engine: ReplicationEngine;
  supported: boolean;
  reason?: string;
  actions: Array<"preview" | "configure">;
};

export type DatabaseReplicationPair = {
  pair_id: string;
  pair_revision: number;
  state: string;
  engine: ReplicationEngine;
  source_instance_id: string;
  target_instance_id: string;
  database: string;
};

export type DatabaseReplicationPairs = {
  items: DatabaseReplicationPair[];
  total: number;
};

export type DatabaseReplicationPreview = {
  pair_id: string;
  preview_id: string;
  expires_at: string;
  source_instance_id: string;
  target_instance_id: string;
  database: string;
  engine: ReplicationEngine;
  source_instance_revision: number;
  target_instance_revision: number;
  source_database_revision: number;
  target_database_revision: number;
  source_config_revision: string;
  target_config_revision: string;
  source_server_id: number;
  target_server_id: number;
  estimated_seed_bytes: number;
  table_count: number;
  schema_sha256: string;
  snapshot_sha256: string;
  confirm_empty_target_required: true;
  confirm_replica_history_loss_required: true;
  confirm_target_read_only_required: true;
  target_will_be_read_only: true;
};

export type DatabaseReplicationStatus = {
  pair_id: string;
  pair_revision: number;
  state: string;
  phase: string;
  engine: ReplicationEngine;
  source_instance_id: string;
  target_instance_id: string;
  database: string;
  channel: string;
  source_server_id: number;
  target_server_id: number;
  source_log_file?: string;
  source_log_position?: number;
  seed_bytes?: number;
  seed_sha256?: string;
  target_read_only: boolean;
  target_super_read_only: boolean;
  target_protection_verified: boolean;
  io_thread_running: boolean;
  sql_thread_running: boolean;
  seconds_behind_source?: number;
  error_class?: string;
  recovery_required: boolean;
  operation?: string;
  allowed_recovery_choices: Array<"apply_pending" | "restore_previous">;
};

type RecordValue = Record<string, unknown>;
const isRecord = (value: unknown): value is RecordValue =>
  !!value && typeof value === "object" && !Array.isArray(value);
const safeInteger = (value: unknown, minimum = 0): value is number =>
  typeof value === "number" &&
  Number.isSafeInteger(value) &&
  value >= minimum;
const idPattern = /^[a-z][a-z0-9_-]{0,127}$/;
const pairPattern = /^[a-f0-9]{32}$/;
const digestPattern = /^[a-f0-9]{64}$/;
const databasePattern = /^[A-Za-z_][A-Za-z0-9_-]{0,62}$/;
const serverID = (value: unknown): value is number =>
  safeInteger(value, 1) && value <= 0xffffffff;
const allowedStates = new Set([
  "pending",
  "recovery_required",
  "configuring",
  "seeding",
  "stopped",
  "running",
  "degraded",
  "removing",
  "removed",
]);
const allowedPhases = new Set([
  "config_pending",
  "source_config_pending",
  "source_configured",
  "target_config_pending",
  "target_configured",
  "seed_dump_pending",
  "seed_dumped",
  "seed_import_pending",
  "seed_imported",
  "account_create_pending",
  "account_created",
  "account_grant_pending",
  "account_granted",
  "channel_config_pending",
  "channel_configured",
  "filter_config_pending",
  "channel_created",
  "channel_start_pending",
  "channel_started",
  "channel_stop_pending",
  "channel_stopped",
  "remove_channel_pending",
  "channel_removed",
  "remove_account_pending",
  "account_removed",
  "restore_target_config_pending",
  "restore_source_config_pending",
  "config_restored",
  "seed_cleanup_pending",
  "seed_cleaned",
  "settled",
]);

export function replicationEngine(value: unknown): ReplicationEngine | undefined {
  if (value === "mysql" || value === "mysql-community") return "mysql";
  if (value === "mariadb") return "mariadb";
  return undefined;
}

function objectSchema(capability: Capability) {
  const schema = capability.input_schema;
  if (!isRecord(schema) || !isRecord(schema.properties)) return;
  const required = schema.required;
  if (!Array.isArray(required) || !required.every((field) => typeof field === "string"))
    return;
  return { properties: schema.properties, required: new Set(required as string[]) };
}

export function databaseReplicationCapability(
  capabilities: Capability[],
  name: string,
  scope: string,
  required: string[],
  task: boolean,
  readOnly: boolean,
): Capability | undefined {
  return capabilities.find((capability) => {
    const schema = objectSchema(capability);
    const fieldsMatch =
      !!schema &&
      required.every((field) => {
        const property = schema.properties[field];
        if (!isRecord(property) || !schema.required.has(field)) return false;
        const expectedType =
          field.startsWith("confirm_")
            ? "boolean"
            : field.startsWith("expected_") && field.endsWith("_revision")
              ? "integer"
              : "string";
        if (property.type !== expectedType) return false;
        if (expectedType === "integer" && property.minimum !== 1) return false;
        if (field.startsWith("confirm_") && property.const !== true) return false;
        if (field === "recovery_choice") {
          const choices = property.enum;
          if (
            !Array.isArray(choices) ||
            !choices.includes("apply_pending") ||
            !choices.includes("restore_previous")
          )
            return false;
        }
        return true;
      });
    return (
      capability.name === name &&
      capability.scope === scope &&
      capability.task === task &&
      capability.read_only === readOnly &&
      fieldsMatch
    );
  });
}

export function databaseReplicationConfirmCapability(
  capability: Capability | undefined,
): boolean {
  const schema = capability && objectSchema(capability);
  if (!schema) return false;
  return [
    "confirm_empty_target",
    "confirm_replica_history_loss",
    "confirm_target_read_only",
  ].every(
    (name) =>
      schema.required.has(name) &&
      isRecord(schema.properties[name]) &&
      schema.properties[name].const === true,
  );
}

export function parseReplicationEndpoint(
  value: unknown,
  expectedID: string,
  expectedEngine: ReplicationEngine,
): ReplicationEndpoint | undefined {
  if (
    !isRecord(value) ||
    value.id !== expectedID ||
    !idPattern.test(expectedID) ||
    value.ownership !== "managed" ||
    typeof value.status !== "string" ||
    value.status === "uninstalled" ||
    typeof value.plugin_id !== "string" ||
    replicationEngine(value.plugin_id) !== expectedEngine ||
    !safeInteger(value.revision, 1)
  )
    return undefined;
  return {
    id: expectedID,
    plugin_id: value.plugin_id,
    engine: expectedEngine,
    revision: value.revision,
  };
}

export function replicationInstanceCandidates(
  value: unknown,
  currentID: string,
  engine: ReplicationEngine,
): Array<{ id: string; plugin_id: string }> {
  if (!isRecord(value) || !Array.isArray(value.items) || value.items.length > 1000)
    return [];
  const seen = new Set<string>();
  const result: Array<{ id: string; plugin_id: string }> = [];
  for (const entry of value.items) {
    if (
      !isRecord(entry) ||
      typeof entry.id !== "string" ||
      !idPattern.test(entry.id) ||
      entry.id === currentID ||
      seen.has(entry.id) ||
      entry.ownership !== "managed" ||
      typeof entry.status !== "string" ||
      entry.status === "uninstalled" ||
      typeof entry.plugin_id !== "string" ||
      replicationEngine(entry.plugin_id) !== engine
    )
      continue;
    seen.add(entry.id);
    result.push({ id: entry.id, plugin_id: entry.plugin_id });
  }
  return result;
}

export function parseReplicationDatabaseReceipt(
  value: unknown,
  instanceID: string,
  database: string,
  engine: ReplicationEngine,
): ReplicationDatabaseReceipt | undefined {
  if (
    !isRecord(value) ||
    value.instance_id !== instanceID ||
    value.database !== database ||
    !databasePattern.test(database) ||
    replicationEngine(value.engine) !== engine ||
    value.state !== "active" ||
    !safeInteger(value.revision, 1)
  )
    return undefined;
  return {
    instance_id: instanceID,
    database,
    engine,
    revision: value.revision,
    state: "active",
  };
}

export function parseDatabaseReplicationCapability(
  value: unknown,
  sourceID: string,
  targetID: string,
  expectedEngine?: ReplicationEngine,
): DatabaseReplicationCapability | undefined {
  if (
    !isRecord(value) ||
    value.source_instance_id !== sourceID ||
    value.target_instance_id !== targetID ||
    !idPattern.test(sourceID) ||
    !idPattern.test(targetID) ||
    sourceID === targetID ||
    typeof value.supported !== "boolean"
  )
    return undefined;
  const engine = replicationEngine(value.engine);
  if (!engine || (expectedEngine && engine !== expectedEngine)) return undefined;
  const actions = Array.isArray(value.actions)
    ? value.actions.filter(
        (item): item is "preview" | "configure" =>
          item === "preview" || item === "configure",
      )
    : [];
  if (new Set(actions).size !== actions.length) return undefined;
  if (value.supported && !(actions.includes("preview") && actions.includes("configure")))
    return undefined;
  if (value.reason !== undefined && (typeof value.reason !== "string" || value.reason.length > 500))
    return undefined;
  return {
    source_instance_id: sourceID,
    target_instance_id: targetID,
    engine,
    supported: value.supported,
    ...(typeof value.reason === "string" && value.reason
      ? { reason: value.reason }
      : {}),
    actions,
  };
}

export function parseDatabaseReplicationPairs(
  value: unknown,
  instanceID: string,
): DatabaseReplicationPairs | undefined {
  if (
    !isRecord(value) ||
    !Array.isArray(value.items) ||
    value.items.length > 1000 ||
    !safeInteger(value.total) ||
    value.total !== value.items.length
  )
    return undefined;
  const seen = new Set<string>();
  const items: DatabaseReplicationPair[] = [];
  for (const entry of value.items) {
    if (
      !isRecord(entry) ||
      typeof entry.pair_id !== "string" ||
      !pairPattern.test(entry.pair_id) ||
      seen.has(entry.pair_id) ||
      !safeInteger(entry.pair_revision, 1) ||
      typeof entry.state !== "string" ||
      !allowedStates.has(entry.state) ||
      typeof entry.source_instance_id !== "string" ||
      !idPattern.test(entry.source_instance_id) ||
      typeof entry.target_instance_id !== "string" ||
      !idPattern.test(entry.target_instance_id) ||
      entry.source_instance_id === entry.target_instance_id ||
      (entry.source_instance_id !== instanceID && entry.target_instance_id !== instanceID) ||
      typeof entry.database !== "string" ||
      !databasePattern.test(entry.database)
    )
      return undefined;
    const engine = replicationEngine(entry.engine);
    if (!engine) return undefined;
    seen.add(entry.pair_id);
    items.push({
      pair_id: entry.pair_id,
      pair_revision: entry.pair_revision,
      state: entry.state,
      engine,
      source_instance_id: entry.source_instance_id,
      target_instance_id: entry.target_instance_id,
      database: entry.database,
    });
  }
  return { items, total: value.total };
}

export function parseDatabaseReplicationPreview(
  value: unknown,
  expected: {
    source: ReplicationEndpoint;
    target: ReplicationEndpoint;
    database: string;
    sourceDatabase: ReplicationDatabaseReceipt;
    targetDatabase: ReplicationDatabaseReceipt;
  },
): DatabaseReplicationPreview | undefined {
  if (!isRecord(value)) return undefined;
  const engine = replicationEngine(value.engine);
  if (
    !engine ||
    engine !== expected.source.engine ||
    engine !== expected.target.engine ||
    value.source_instance_id !== expected.source.id ||
    value.target_instance_id !== expected.target.id ||
    value.database !== expected.database ||
    !pairPattern.test(String(value.pair_id)) ||
    !pairPattern.test(String(value.preview_id)) ||
    typeof value.expires_at !== "string" ||
    !Number.isFinite(Date.parse(value.expires_at)) ||
    value.source_instance_revision !== expected.source.revision ||
    value.target_instance_revision !== expected.target.revision ||
    value.source_database_revision !== expected.sourceDatabase.revision ||
    value.target_database_revision !== expected.targetDatabase.revision ||
    !digestPattern.test(String(value.source_config_revision)) ||
    !digestPattern.test(String(value.target_config_revision)) ||
    !serverID(value.source_server_id) ||
    !serverID(value.target_server_id) ||
    !safeInteger(value.estimated_seed_bytes) ||
    !safeInteger(value.table_count, 1) ||
    value.table_count > 1000 ||
    !digestPattern.test(String(value.schema_sha256)) ||
    !digestPattern.test(String(value.snapshot_sha256)) ||
    value.confirm_empty_target_required !== true ||
    value.confirm_replica_history_loss_required !== true ||
    value.confirm_target_read_only_required !== true ||
    value.target_will_be_read_only !== true
  )
    return undefined;
  return {
    pair_id: value.pair_id as string,
    preview_id: value.preview_id as string,
    expires_at: value.expires_at,
    source_instance_id: expected.source.id,
    target_instance_id: expected.target.id,
    database: expected.database,
    engine,
    source_instance_revision: expected.source.revision,
    target_instance_revision: expected.target.revision,
    source_database_revision: expected.sourceDatabase.revision,
    target_database_revision: expected.targetDatabase.revision,
    source_config_revision: value.source_config_revision as string,
    target_config_revision: value.target_config_revision as string,
    source_server_id: value.source_server_id,
    target_server_id: value.target_server_id,
    estimated_seed_bytes: value.estimated_seed_bytes,
    table_count: value.table_count,
    schema_sha256: value.schema_sha256 as string,
    snapshot_sha256: value.snapshot_sha256 as string,
    confirm_empty_target_required: true,
    confirm_replica_history_loss_required: true,
    confirm_target_read_only_required: true,
    target_will_be_read_only: true,
  };
}

export function databaseReplicationPreviewInput(
  preview: DatabaseReplicationPreview | undefined,
  acknowledgments: {
    emptyTarget: boolean;
    replicaHistory: boolean;
    targetReadOnly: boolean;
  },
  now = Date.now(),
): Record<string, unknown> | undefined {
  if (
    !preview ||
    Date.parse(preview.expires_at) <= now ||
    !acknowledgments.emptyTarget ||
    !acknowledgments.replicaHistory ||
    !acknowledgments.targetReadOnly
  )
    return undefined;
  return {
    pair_id: preview.pair_id,
    preview_id: preview.preview_id,
    snapshot_sha256: preview.snapshot_sha256,
    source_instance_id: preview.source_instance_id,
    target_instance_id: preview.target_instance_id,
    database: preview.database,
    expected_source_instance_revision: preview.source_instance_revision,
    expected_target_instance_revision: preview.target_instance_revision,
    expected_source_database_revision: preview.source_database_revision,
    expected_target_database_revision: preview.target_database_revision,
    confirm_empty_target: true,
    confirm_replica_history_loss: true,
    confirm_target_read_only: true,
  };
}

export function parseDatabaseReplicationStatus(
  value: unknown,
  expected: DatabaseReplicationPair,
): DatabaseReplicationStatus | undefined {
  if (
    !isRecord(value) ||
    value.pair_id !== expected.pair_id ||
    !safeInteger(value.pair_revision, 1) ||
    value.pair_revision !== expected.pair_revision ||
    typeof value.state !== "string" ||
    !allowedStates.has(value.state) ||
    typeof value.phase !== "string" ||
    !allowedPhases.has(value.phase) ||
    replicationEngine(value.engine) !== expected.engine ||
    value.source_instance_id !== expected.source_instance_id ||
    value.target_instance_id !== expected.target_instance_id ||
    value.database !== expected.database ||
    typeof value.channel !== "string" ||
    value.channel !== `pl_${expected.pair_id}` ||
    !serverID(value.source_server_id) ||
    !serverID(value.target_server_id) ||
    typeof value.target_read_only !== "boolean" ||
    typeof value.target_super_read_only !== "boolean" ||
    typeof value.target_protection_verified !== "boolean" ||
    typeof value.io_thread_running !== "boolean" ||
    typeof value.sql_thread_running !== "boolean" ||
    typeof value.recovery_required !== "boolean" ||
    (value.operation !== undefined &&
      !["configure", "start", "stop", "remove", "recover"].includes(String(value.operation))) ||
    (value.allowed_recovery_choices !== undefined &&
      (!Array.isArray(value.allowed_recovery_choices) ||
        value.allowed_recovery_choices.some(
          (choice) => choice !== "apply_pending" && choice !== "restore_previous",
        ) ||
        new Set(value.allowed_recovery_choices).size !== value.allowed_recovery_choices.length))
  )
    return undefined;
  if (
    (value.source_log_file !== undefined &&
      (typeof value.source_log_file !== "string" ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/.test(value.source_log_file))) ||
    (value.source_log_position !== undefined &&
      !safeInteger(value.source_log_position, 1)) ||
    (value.seed_bytes !== undefined && !safeInteger(value.seed_bytes, 1)) ||
    (value.seed_sha256 !== undefined && !digestPattern.test(String(value.seed_sha256))) ||
    (value.seconds_behind_source !== undefined &&
      !safeInteger(value.seconds_behind_source)) ||
    (value.error_class !== undefined &&
      (typeof value.error_class !== "string" ||
        !/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/.test(value.error_class)))
  )
    return undefined;
  return {
    pair_id: expected.pair_id,
    pair_revision: value.pair_revision,
    state: value.state,
    phase: value.phase,
    engine: expected.engine,
    source_instance_id: expected.source_instance_id,
    target_instance_id: expected.target_instance_id,
    database: expected.database,
    channel: value.channel,
    source_server_id: value.source_server_id,
    target_server_id: value.target_server_id,
    ...(typeof value.source_log_file === "string"
      ? { source_log_file: value.source_log_file }
      : {}),
    ...(safeInteger(value.source_log_position, 1)
      ? { source_log_position: value.source_log_position }
      : {}),
    ...(safeInteger(value.seed_bytes, 1) ? { seed_bytes: value.seed_bytes } : {}),
    ...(typeof value.seed_sha256 === "string"
      ? { seed_sha256: value.seed_sha256 }
      : {}),
    target_read_only: value.target_read_only,
    target_super_read_only: value.target_super_read_only,
    target_protection_verified: value.target_protection_verified,
    io_thread_running: value.io_thread_running,
    sql_thread_running: value.sql_thread_running,
    ...(safeInteger(value.seconds_behind_source)
      ? { seconds_behind_source: value.seconds_behind_source }
      : {}),
    ...(typeof value.error_class === "string"
      ? { error_class: value.error_class }
      : {}),
    recovery_required: value.recovery_required,
    ...(typeof value.operation === "string" ? { operation: value.operation } : {}),
    allowed_recovery_choices: Array.isArray(value.allowed_recovery_choices)
      ? (value.allowed_recovery_choices as Array<"apply_pending" | "restore_previous">)
      : [],
  };
}

export function databaseReplicationPairStatusInput(
  pair: DatabaseReplicationPair,
): Record<string, unknown> {
  return {
    pair_id: pair.pair_id,
    source_instance_id: pair.source_instance_id,
    target_instance_id: pair.target_instance_id,
    database: pair.database,
    expected_pair_revision: pair.pair_revision,
  };
}

export function databaseReplicationLifecycleInput(
  status: DatabaseReplicationStatus | undefined,
  operation: ReplicationOperation,
  explicitRemoveConfirmation = false,
): Record<string, unknown> | undefined {
  if (
    !status ||
    status.phase !== "settled" ||
    status.recovery_required ||
    status.state === "removed"
  )
    return undefined;
  if (operation === "start" && status.state !== "stopped" && status.state !== "degraded")
    return undefined;
  if (operation === "start" && !databaseReplicationTargetProtectionVerified(status))
    return undefined;
  if (operation === "stop" && status.state !== "running" && status.state !== "degraded")
    return undefined;
  if (
    operation === "remove" &&
    (!explicitRemoveConfirmation ||
      !["running", "stopped", "degraded"].includes(status.state))
  )
    return undefined;
  return {
    pair_id: status.pair_id,
    source_instance_id: status.source_instance_id,
    target_instance_id: status.target_instance_id,
    database: status.database,
    expected_pair_revision: status.pair_revision,
    ...(operation === "remove"
      ? { confirm_remove: true, confirm_replica_history_loss: true }
      : {}),
  };
}

export function databaseReplicationRecoveryInput(
  status: DatabaseReplicationStatus | undefined,
  choice: "apply_pending" | "restore_previous",
): Record<string, unknown> | undefined {
  if (
    !status ||
    !status.recovery_required ||
    !status.allowed_recovery_choices.includes(choice)
  )
    return undefined;
  return {
    pair_id: status.pair_id,
    source_instance_id: status.source_instance_id,
    target_instance_id: status.target_instance_id,
    database: status.database,
    expected_pair_revision: status.pair_revision,
    recovery_choice: choice,
  };
}

export function databaseReplicationTargetProtectionVerified(
  status: DatabaseReplicationStatus,
): boolean {
  return (
    status.target_protection_verified &&
    status.target_read_only &&
    (status.engine === "mariadb" || status.target_super_read_only)
  );
}
