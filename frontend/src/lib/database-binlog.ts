import type { Capability } from "./api";

export type DatabaseBinlogStatus = {
  enabled: boolean;
  format: string;
  basename: string;
  active_log: string;
  server_id: string;
  expiration_seconds: number;
  config_revision: string;
  instance_revision: number;
  configuration_managed: boolean;
  configured_enabled?: boolean;
  configuration_state:
    | "settled"
    | "runtime_config_mismatch"
    | "configuration_unmanaged"
    | "recovery_required";
};

export type DatabaseBinlog = { name: string; size_bytes: number };

export type DatabaseBinlogList = {
  items: DatabaseBinlog[];
  total: number;
  truncated: boolean;
  next_after?: string;
  active_log: string;
  config_revision: string;
  instance_revision: number;
};

export type DatabaseBinlogPreview = {
  preview_id: string;
  expires_at: string;
  retain_from: string;
  active_log: string;
  binlogs_to_purge: DatabaseBinlog[];
  bytes_to_purge: number;
  snapshot_sha256: string;
  expected_config_revision: string;
  expected_instance_revision: number;
  replica_safety: "unproven_offline_or_unregistered_replicas";
  confirmation_required: true;
};

export function databaseBinlogCanPreview(
  status: DatabaseBinlogStatus | undefined,
  list: DatabaseBinlogList | undefined,
): boolean {
  return !!(
    status?.configuration_managed &&
    hash(status.config_revision) &&
    list &&
    list.config_revision === status.config_revision &&
    list.instance_revision === status.instance_revision &&
    !list.truncated &&
    list.total === list.items.length &&
    list.items.length <= 128 &&
    list.items.some((entry) => entry.name === list.active_log)
  );
}

type Row = Record<string, unknown>;
const row = (value: unknown): value is Row =>
  !!value && typeof value === "object" && !Array.isArray(value);
const integer = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const hash = (value: unknown): value is string =>
  typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
const binlogName = (value: unknown): value is string =>
  typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/.test(value);

export function phase2Capability(
  capabilities: Capability[],
  name: string,
  scope: string,
  fields: string[],
  task: boolean,
  readOnly?: boolean,
): Capability | undefined {
  return capabilities.find((capability) => {
    const properties = capability.input_schema?.properties;
    return (
      capability.name === name &&
      capability.scope === scope &&
      (task ? capability.task === true : capability.task !== true) &&
      (readOnly === undefined || capability.read_only === readOnly) &&
      !!properties &&
      fields.every((field) => Object.prototype.hasOwnProperty.call(properties, field))
    );
  });
}

export function databaseBinlogStatus(value: unknown): DatabaseBinlogStatus | undefined {
  if (!row(value)) return undefined;
  const {
    enabled,
    format,
    basename,
    active_log,
    server_id,
    expiration_seconds,
    config_revision,
    instance_revision,
    configuration_state,
  } = value;
  if (
    typeof enabled !== "boolean" ||
    typeof format !== "string" ||
    typeof basename !== "string" ||
    typeof active_log !== "string" ||
    typeof server_id !== "string" ||
    !integer(expiration_seconds) ||
    !(hash(config_revision) ||
      (configuration_state === "configuration_unmanaged" && config_revision === "")) ||
    !integer(instance_revision) ||
    instance_revision < 1 ||
    typeof value.configuration_managed !== "boolean" ||
    ![
      "settled",
      "runtime_config_mismatch",
      "configuration_unmanaged",
      "recovery_required",
    ].includes(String(configuration_state)) ||
    (value.configured_enabled !== undefined &&
      value.configured_enabled !== null &&
      typeof value.configured_enabled !== "boolean")
  )
    return undefined;
  return {
    enabled,
    format,
    basename,
    active_log,
    server_id,
    expiration_seconds,
    config_revision,
    instance_revision,
    configuration_managed: value.configuration_managed,
    ...(typeof value.configured_enabled === "boolean"
      ? { configured_enabled: value.configured_enabled }
      : {}),
    configuration_state: configuration_state as DatabaseBinlogStatus["configuration_state"],
  };
}

export function databaseBinlogList(value: unknown): DatabaseBinlogList | undefined {
  if (!row(value) || !Array.isArray(value.items)) return undefined;
  const items: DatabaseBinlog[] = [];
  const seen = new Set<string>();
  for (const entry of value.items) {
    if (
      !row(entry) ||
      !binlogName(entry.name) ||
      !integer(entry.size_bytes) ||
      seen.has(entry.name)
    )
      return undefined;
    seen.add(entry.name);
    items.push({ name: entry.name, size_bytes: entry.size_bytes });
  }
  if (
    !integer(value.total) ||
    typeof value.truncated !== "boolean" ||
    typeof value.active_log !== "string" ||
    !(hash(value.config_revision) || value.config_revision === "") ||
    (value.truncated &&
      (!binlogName(value.next_after) ||
        items.at(-1)?.name !== value.next_after)) ||
    (!value.truncated && value.next_after !== undefined) ||
    !integer(value.instance_revision) ||
    value.instance_revision < 1
  )
    return undefined;
  return {
    items,
    total: value.total,
    truncated: value.truncated,
    ...(typeof value.next_after === "string"
      ? { next_after: value.next_after }
      : {}),
    active_log: value.active_log,
    config_revision: value.config_revision,
    instance_revision: value.instance_revision,
  };
}

export function databaseBinlogPreview(
  value: unknown,
  retainFrom: string,
): DatabaseBinlogPreview | undefined {
  if (!row(value) || !Array.isArray(value.binlogs_to_purge)) return undefined;
  const binlogs: DatabaseBinlog[] = [];
  const seen = new Set<string>();
  let sum = 0;
  for (const entry of value.binlogs_to_purge) {
    if (
      !row(entry) ||
      !binlogName(entry.name) ||
      !integer(entry.size_bytes) ||
      seen.has(entry.name)
    )
      return undefined;
    seen.add(entry.name);
    sum += entry.size_bytes;
    if (!Number.isSafeInteger(sum)) return undefined;
    binlogs.push({ name: entry.name, size_bytes: entry.size_bytes });
  }
  if (
    typeof value.preview_id !== "string" ||
    !value.preview_id ||
    typeof value.expires_at !== "string" ||
    !Number.isFinite(Date.parse(value.expires_at)) ||
    value.retain_from !== retainFrom ||
    typeof value.active_log !== "string" ||
    !integer(value.bytes_to_purge) ||
    sum !== value.bytes_to_purge ||
    typeof value.snapshot_sha256 !== "string" ||
    !/^[a-f0-9]{64}$/.test(value.snapshot_sha256) ||
    !hash(value.expected_config_revision) ||
    !integer(value.expected_instance_revision) ||
    value.expected_instance_revision < 1 ||
    value.replica_safety !== "unproven_offline_or_unregistered_replicas" ||
    value.confirmation_required !== true ||
    seen.has(retainFrom) ||
    (value.active_log !== "" && seen.has(value.active_log))
  )
    return undefined;
  return {
    preview_id: value.preview_id,
    expires_at: value.expires_at,
    retain_from: value.retain_from,
    active_log: value.active_log,
    binlogs_to_purge: binlogs,
    bytes_to_purge: value.bytes_to_purge,
    snapshot_sha256: value.snapshot_sha256,
    expected_config_revision: value.expected_config_revision,
    expected_instance_revision: value.expected_instance_revision,
    replica_safety: "unproven_offline_or_unregistered_replicas",
    confirmation_required: true,
  };
}

export function databaseBinlogPurgeInput(
  instanceID: string,
  preview: DatabaseBinlogPreview,
  now = Date.now(),
) {
  if (!instanceID || Date.parse(preview.expires_at) <= now)
    throw new Error("Binlog 清理预览已过期，请重新读取并预览。");
  if (!preview.binlogs_to_purge.length)
    throw new Error("预览中没有可清理的日志文件。");
  return {
    instance_id: instanceID,
    preview_id: preview.preview_id,
    snapshot_sha256: preview.snapshot_sha256,
    expected_config_revision: preview.expected_config_revision,
    expected_instance_revision: preview.expected_instance_revision,
    binlogs_to_purge: preview.binlogs_to_purge.map((log) => log.name),
    confirm_replica_history_loss: true,
  };
}
