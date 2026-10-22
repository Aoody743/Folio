import type { Capability } from "./api";

export type PostgreSQLAccessRule = {
  rule_id: string;
  database: string;
  role: string;
  cidr: string;
  auth_method: "scram-sha-256" | "reject";
  state: "allowed" | "blocked";
};

export type PostgreSQLAccessList = {
  instance_id: string;
  database: string;
  instance_revision: number;
  database_revision: number;
  config_revision: string;
  configuration_state: "unconfigured" | "settled" | "pending_recovery";
  protected_local_admin_preserved: true;
  remote_access_supported: false;
  remote_access_limitation: "managed_tls_not_configured";
  rules: PostgreSQLAccessRule[];
};

export type PostgreSQLSlowlogStatus = {
  instance_id: string;
  database: string;
  instance_revision: number;
  database_revision: number;
  config_revision: string;
  enabled: boolean;
  minimum_duration_ms: number;
  configuration_state:
    | "unconfigured"
    | "settled"
    | "pending_reload"
    | "pending_recovery"
    | "unmanaged"
    | "restored";
  reload_applied: boolean;
};

export type PostgreSQLSlowlogEntry = {
  started_at: string;
  duration_ms: number;
  database: string;
  role: string;
  statement: string;
};

export type PostgreSQLSlowlogRead = PostgreSQLSlowlogStatus & {
  redaction_notice: string;
  items: PostgreSQLSlowlogEntry[];
  truncated: boolean;
  log_available: boolean;
};

const record = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const integer = (value: unknown, min = 0): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= min;
const digest = (value: unknown): value is string =>
  typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
const validInstance = (value: string) => /^[a-z][a-z0-9_-]{0,127}$/.test(value);
const validDatabase = (value: string) =>
  /^[a-zA-Z_][a-zA-Z0-9_$-]{0,62}$/.test(value);
const ruleID = (value: unknown): value is string =>
  typeof value === "string" && /^pga_[a-f0-9]{32}$/.test(value);

export function postgresqlCapability(
  capabilities: Capability[],
  name: string,
  scope: "databases:read" | "databases:admin",
  required: string[],
  task: boolean,
  readOnly: boolean,
): Capability | undefined {
  return capabilities.find((capability) => {
    const properties = capability.input_schema?.properties;
    const schemaRequired = capability.input_schema?.required;
    return (
      capability.name === name &&
      capability.scope === scope &&
      capability.task === task &&
      capability.read_only === readOnly &&
      !!properties &&
      required.every((field) => Object.hasOwn(properties, field)) &&
      Array.isArray(schemaRequired) &&
      required.every((field) => schemaRequired.includes(field))
    );
  });
}

export function parsePostgreSQLAccessList(
  value: unknown,
  instanceID: string,
  database: string,
): PostgreSQLAccessList | undefined {
  if (!record(value) || !validInstance(instanceID) || !validDatabase(database))
    return;
  if (
    value.instance_id !== instanceID ||
    value.database !== database ||
    !integer(value.instance_revision, 1) ||
    !integer(value.database_revision, 1) ||
    !digest(value.config_revision) ||
    !["unconfigured", "settled", "pending_recovery"].includes(
      String(value.configuration_state),
    ) ||
    value.protected_local_admin_preserved !== true ||
    value.remote_access_supported !== false ||
    value.remote_access_limitation !== "managed_tls_not_configured" ||
    !Array.isArray(value.rules) ||
    value.rules.length > 256
  )
    return;
  const seen = new Set<string>();
  const rules: PostgreSQLAccessRule[] = [];
  for (const item of value.rules) {
    if (
      !record(item) ||
      !ruleID(item.rule_id) ||
      item.database !== database ||
      typeof item.role !== "string" ||
      !item.role ||
      typeof item.cidr !== "string" ||
      !item.cidr ||
      !["scram-sha-256", "reject"].includes(String(item.auth_method)) ||
      !["allowed", "blocked"].includes(String(item.state)) ||
      (item.auth_method === "scram-sha-256") !== (item.state === "allowed") ||
      seen.has(item.rule_id)
    )
      return;
    seen.add(item.rule_id);
    rules.push({
      rule_id: item.rule_id,
      database,
      role: item.role,
      cidr: item.cidr,
      auth_method: item.auth_method as PostgreSQLAccessRule["auth_method"],
      state: item.state as PostgreSQLAccessRule["state"],
    });
  }
  return {
    instance_id: instanceID,
    database,
    instance_revision: value.instance_revision,
    database_revision: value.database_revision,
    config_revision: value.config_revision,
    configuration_state:
      value.configuration_state as PostgreSQLAccessList["configuration_state"],
    protected_local_admin_preserved: true,
    remote_access_supported: false,
    remote_access_limitation: "managed_tls_not_configured",
    rules,
  };
}

export function isLoopbackPostgreSQLCIDR(value: string): boolean {
  const cidr = value.trim();
  if (cidr === "::1/128") return true;
  const match =
    /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d|[12]\d|3[0-2])$/.exec(
      cidr,
    );
  if (!match) return false;
  const octetStrings = match.slice(1, 5);
  const octets = octetStrings.map(Number);
  if (
    octets.some(
      (octet, index) => octet > 255 || String(octet) !== octetStrings[index],
    ) ||
    String(Number(match[5])) !== match[5]
  )
    return false;
  if (octets[0] !== 127) return false;
  const prefix = Number(match[5]);
  const address =
    (((octets[0] << 24) >>> 0) |
      (octets[1] << 16) |
      (octets[2] << 8) |
      octets[3]) >>>
    0;
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0;
  return (address & mask) >>> 0 === address && prefix >= 8;
}

export function postgresqlAccessMutationInput(
  access: PostgreSQLAccessList | undefined,
  instanceID: string,
  database: string,
  operation: "add" | "remove" | "recover",
  value?: string,
): Record<string, unknown> | undefined {
  if (
    !access ||
    access.instance_id !== instanceID ||
    access.database !== database ||
    !digest(access.config_revision) ||
    (operation !== "recover" &&
      access.configuration_state === "pending_recovery")
  )
    return;
  const base = {
    instance_id: instanceID,
    database,
    expected_config_revision: access.config_revision,
    expected_instance_revision: access.instance_revision,
    expected_database_revision: access.database_revision,
  };
  if (operation === "add") {
    if (!value || !isLoopbackPostgreSQLCIDR(value)) return;
    return { ...base, cidr: value.trim(), auth_method: "scram-sha-256" };
  }
  if (operation === "remove") {
    if (
      !value ||
      !access.rules.some(
        (rule) => rule.rule_id === value && rule.state === "allowed",
      )
    )
      return;
    return { ...base, rule_id: value };
  }
  return access.configuration_state === "pending_recovery" &&
    (value === "apply_pending" || value === "restore_previous")
    ? { ...base, recovery_choice: value }
    : undefined;
}

const slowStates = [
  "unconfigured",
  "settled",
  "pending_reload",
  "pending_recovery",
  "unmanaged",
  "restored",
] as const;

function parseSlowStatus(
  value: unknown,
  instanceID: string,
  database: string,
): PostgreSQLSlowlogStatus | undefined {
  if (!record(value)) return;
  const state = value.configuration_state;
  if (
    value.instance_id !== instanceID ||
    value.database !== database ||
    !integer(value.instance_revision, 1) ||
    !integer(value.database_revision, 1) ||
    !digest(value.config_revision) ||
    typeof value.enabled !== "boolean" ||
    !integer(value.minimum_duration_ms, 0) ||
    value.minimum_duration_ms > 600000 ||
    !slowStates.includes(state as (typeof slowStates)[number]) ||
    typeof value.reload_applied !== "boolean"
  )
    return;
  return {
    instance_id: instanceID,
    database,
    instance_revision: value.instance_revision,
    database_revision: value.database_revision,
    config_revision: value.config_revision,
    enabled: value.enabled,
    minimum_duration_ms: value.minimum_duration_ms,
    configuration_state:
      state as PostgreSQLSlowlogStatus["configuration_state"],
    reload_applied: value.reload_applied,
  };
}

export function parsePostgreSQLSlowlogStatus(
  value: unknown,
  instanceID: string,
  database: string,
): PostgreSQLSlowlogStatus | undefined {
  return parseSlowStatus(value, instanceID, database);
}

export function parsePostgreSQLSlowlogRead(
  value: unknown,
  instanceID: string,
  database: string,
): PostgreSQLSlowlogRead | undefined {
  const status = parseSlowStatus(value, instanceID, database);
  if (
    !status ||
    !record(value) ||
    !Array.isArray(value.items) ||
    value.items.length > 200 ||
    typeof value.redaction_notice !== "string" ||
    value.redaction_notice.length === 0 ||
    value.redaction_notice.length > 512 ||
    typeof value.truncated !== "boolean" ||
    typeof value.log_available !== "boolean"
  )
    return;
  const items: PostgreSQLSlowlogEntry[] = [];
  for (const item of value.items) {
    if (
      !record(item) ||
      typeof item.started_at !== "string" ||
      !Number.isFinite(Date.parse(item.started_at)) ||
      typeof item.duration_ms !== "number" ||
      !Number.isFinite(item.duration_ms) ||
      item.duration_ms < 0 ||
      item.database !== database ||
      typeof item.role !== "string" ||
      typeof item.statement !== "string" ||
      item.statement.length > 65536
    )
      return;
    items.push({
      started_at: item.started_at,
      duration_ms: item.duration_ms,
      database,
      role: item.role,
      statement: item.statement,
    });
  }
  return {
    ...status,
    redaction_notice: value.redaction_notice,
    items,
    truncated: value.truncated,
    log_available: value.log_available,
  };
}

export function postgresqlSlowlogInput(
  status: PostgreSQLSlowlogStatus | undefined,
  operation: "configure" | "recover",
  enabled?: boolean,
  minimumDuration?: number,
  recoveryChoice?: "apply_pending" | "restore_previous",
): Record<string, unknown> | undefined {
  if (!status || !digest(status.config_revision)) return;
  const base = {
    instance_id: status.instance_id,
    database: status.database,
    expected_config_revision: status.config_revision,
    expected_instance_revision: status.instance_revision,
    expected_database_revision: status.database_revision,
  };
  if (operation === "configure") {
    if (
      typeof enabled !== "boolean" ||
      !integer(minimumDuration, 1) ||
      minimumDuration > 600000 ||
      ["pending_reload", "pending_recovery", "unmanaged"].includes(
        status.configuration_state,
      )
    )
      return;
    return { ...base, enabled, minimum_duration_ms: minimumDuration };
  }
  if (status.configuration_state !== "pending_recovery" || !recoveryChoice)
    return;
  return { ...base, recovery_choice: recoveryChoice };
}

export function postgresqlSlowlogMayConfigure(
  status: PostgreSQLSlowlogStatus | undefined,
): boolean {
  return (
    !!status &&
    !["pending_reload", "pending_recovery", "unmanaged"].includes(
      status.configuration_state,
    )
  );
}
