import type { Capability } from "./api";

export type DatabaseMaintenanceOperation = "optimize" | "repair";
export type DatabaseMaintenanceTable = {
  name: string;
  type: string;
  engine: string;
  collation?: string;
  approx_rows?: number;
  data_bytes?: number;
  index_bytes?: number;
  free_bytes?: number;
  estimate_known: boolean;
};
export type DatabaseMaintenancePage = {
  instance_id: string;
  database: string;
  items: DatabaseMaintenanceTable[];
  total: number;
  truncated: boolean;
  next_after?: string;
  schema_sha256: string;
  instance_revision: number;
  database_revision: number;
  replication_source_attached: boolean;
};
export type DatabaseMaintenancePreview = {
  preview_id: string;
  expires_at: string;
  instance_id: string;
  database: string;
  engine: string;
  operation: DatabaseMaintenanceOperation;
  tables: DatabaseMaintenanceTable[];
  table_count: number;
  schema_sha256: string;
  snapshot_sha256: string;
  instance_revision: number;
  database_revision: number;
  estimated_bytes: number;
  estimate_known: boolean;
  may_need_comparable_temporary_space: boolean;
  lock_impact: string;
  replica_impact: string;
  warnings: string[];
  confirmation_required: true;
};
export type DatabaseMaintenanceItemResult = {
  name: string;
  engine: string;
  state: "not_started" | "completed" | "failed" | "outcome_unknown";
  messages: Array<{ msg_type: string; msg_text: string }>;
};
export type DatabaseMaintenanceInspection = {
  preview_id: string;
  task_id?: string;
  instance_id: string;
  database: string;
  engine: string;
  operation: DatabaseMaintenanceOperation;
  state:
    | "ready"
    | "expired"
    | "not_started"
    | "outcome_unknown"
    | "completed"
    | "partial"
    | "failed"
    | "canceled";
  instance_revision: number;
  database_revision: number;
  schema_sha256: string;
  started_at?: string;
  finished_at?: string;
  completed_count: number;
  failed_count: number;
  outcome_unknown_count: number;
  items: DatabaseMaintenanceItemResult[];
  replication_source_attached: boolean;
};

const digest = /^[a-f0-9]{64}$/;
const previewID = /^plan_[a-f0-9]{48}$/;
const taskID = /^[a-f0-9]{32}$/;
const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const positive = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value > 0;
const nonnegative = (value: unknown): value is number =>
  typeof value === "number" &&
  Number.isFinite(value) &&
  Number.isInteger(value) &&
  value >= 0;
function tableName(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0) return false;
  let characters = 0;
  let utf8Bytes = 0;
  for (const character of value) {
    const point = character.codePointAt(0);
    if (
      point === undefined ||
      point === 0 ||
      (point >= 0xd800 && point <= 0xdfff)
    )
      return false;
    characters++;
    utf8Bytes +=
      point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
  }
  return characters <= 64 && utf8Bytes <= 256;
}
const time = (value: unknown): value is string =>
  typeof value === "string" &&
  value.length > 0 &&
  Number.isFinite(Date.parse(value));

function schemaObject(
  capability: Capability,
): Record<string, unknown> | undefined {
  const schema = capability.input_schema;
  return object(schema) && object(schema.properties)
    ? (schema as Record<string, unknown>)
    : undefined;
}

function propertyMatches(
  properties: Record<string, unknown>,
  name: string,
  type: string,
  options: {
    minimum?: number;
    maximum?: number;
    maxLength?: number;
    pattern?: string;
    enumValues?: string[];
    constValue?: unknown;
  } = {},
): boolean {
  const property = properties[name];
  if (!object(property) || property.type !== type) return false;
  if (options.minimum !== undefined && property.minimum !== options.minimum)
    return false;
  if (options.maximum !== undefined && property.maximum !== options.maximum)
    return false;
  if (
    options.maxLength !== undefined &&
    property.maxLength !== options.maxLength
  )
    return false;
  if (options.pattern !== undefined && property.pattern !== options.pattern)
    return false;
  if (options.constValue !== undefined && property.const !== options.constValue)
    return false;
  const values = property.enum;
  if (
    options.enumValues &&
    (!Array.isArray(values) ||
      options.enumValues.some((value) => !values.includes(value)) ||
      values.length !== options.enumValues.length)
  )
    return false;
  return true;
}

function hasRequired(
  schema: Record<string, unknown>,
  required: string[],
): boolean {
  const fields = schema.required;
  return (
    Array.isArray(fields) && required.every((field) => fields.includes(field))
  );
}

function exactCapability(
  capabilities: Capability[],
  name: string,
  scope: string,
  task: boolean,
  readOnly: boolean,
  required: string[],
  propertiesValid: (properties: Record<string, unknown>) => boolean,
): Capability | undefined {
  return capabilities.find((capability) => {
    if (
      capability.name !== name ||
      capability.scope !== scope ||
      capability.task !== task ||
      capability.read_only !== readOnly
    )
      return false;
    const schema = schemaObject(capability);
    return (
      !!schema &&
      hasRequired(schema, required) &&
      propertiesValid(schema.properties as Record<string, unknown>)
    );
  });
}

export function databaseTableMaintenanceCapabilities(
  capabilities: Capability[],
) {
  const revisionMaximum = 9223372036854775807;
  const list = exactCapability(
    capabilities,
    "managed_database_tables_list",
    "databases:read",
    false,
    true,
    ["instance_id", "database"],
    (properties) =>
      propertyMatches(properties, "instance_id", "string") &&
      propertyMatches(properties, "database", "string") &&
      propertyMatches(properties, "limit", "integer", {
        minimum: 0,
        maximum: 200,
      }) &&
      propertyMatches(properties, "after", "string", { maxLength: 64 }),
  );
  const preview = exactCapability(
    capabilities,
    "managed_database_table_maintenance_preview",
    "databases:admin",
    false,
    false,
    [
      "instance_id",
      "database",
      "operation",
      "tables",
      "expected_instance_revision",
      "expected_database_revision",
      "expected_schema_sha256",
    ],
    (properties) =>
      propertyMatches(properties, "instance_id", "string") &&
      propertyMatches(properties, "database", "string") &&
      propertyMatches(properties, "operation", "string", {
        enumValues: ["optimize", "repair"],
      }) &&
      propertyMatches(properties, "tables", "array") &&
      (properties.tables as Record<string, unknown>).minItems === 1 &&
      (properties.tables as Record<string, unknown>).maxItems === 25 &&
      (properties.tables as Record<string, unknown>).uniqueItems === true &&
      object(properties.tables) &&
      object((properties.tables as Record<string, unknown>).items) &&
      (
        (properties.tables as Record<string, unknown>).items as Record<
          string,
          unknown
        >
      ).type === "string" &&
      (
        (properties.tables as Record<string, unknown>).items as Record<
          string,
          unknown
        >
      ).minLength === 1 &&
      (
        (properties.tables as Record<string, unknown>).items as Record<
          string,
          unknown
        >
      ).maxLength === 64 &&
      propertyMatches(properties, "expected_instance_revision", "integer", {
        minimum: 1,
        maximum: revisionMaximum,
      }) &&
      propertyMatches(properties, "expected_database_revision", "integer", {
        minimum: 1,
        maximum: revisionMaximum,
      }) &&
      propertyMatches(properties, "expected_schema_sha256", "string", {
        pattern: "^[0-9a-f]{64}$",
      }),
  );
  const run = exactCapability(
    capabilities,
    "managed_database_table_maintenance",
    "databases:admin",
    true,
    false,
    [
      "instance_id",
      "database",
      "preview_id",
      "snapshot_sha256",
      "expected_instance_revision",
      "expected_database_revision",
      "confirm_maintenance",
    ],
    (properties) =>
      propertyMatches(properties, "instance_id", "string") &&
      propertyMatches(properties, "database", "string") &&
      propertyMatches(properties, "preview_id", "string", {
        pattern: "^plan_[0-9a-f]{48}$",
      }) &&
      propertyMatches(properties, "snapshot_sha256", "string", {
        pattern: "^[0-9a-f]{64}$",
      }) &&
      propertyMatches(properties, "expected_instance_revision", "integer", {
        minimum: 1,
        maximum: revisionMaximum,
      }) &&
      propertyMatches(properties, "expected_database_revision", "integer", {
        minimum: 1,
        maximum: revisionMaximum,
      }) &&
      propertyMatches(properties, "confirm_maintenance", "boolean", {
        constValue: true,
      }),
  );
  const inspect = exactCapability(
    capabilities,
    "managed_database_table_maintenance_inspect",
    "databases:read",
    false,
    true,
    ["instance_id", "database", "preview_id"],
    (properties) =>
      propertyMatches(properties, "instance_id", "string") &&
      propertyMatches(properties, "database", "string") &&
      propertyMatches(properties, "preview_id", "string", {
        pattern: "^plan_[0-9a-f]{48}$",
      }),
  );
  return { list, preview, run, inspect };
}

function parseTable(value: unknown): DatabaseMaintenanceTable | undefined {
  if (
    !object(value) ||
    !tableName(value.name) ||
    typeof value.type !== "string" ||
    (value.engine !== undefined && typeof value.engine !== "string") ||
    (value.collation !== undefined && typeof value.collation !== "string") ||
    typeof value.estimate_known !== "boolean"
  )
    return undefined;
  for (const field of [
    "approx_rows",
    "data_bytes",
    "index_bytes",
    "free_bytes",
  ] as const) {
    if (value[field] !== undefined && !nonnegative(value[field]))
      return undefined;
  }
  return {
    name: value.name,
    type: value.type,
    engine: typeof value.engine === "string" ? value.engine : "",
    ...(typeof value.collation === "string"
      ? { collation: value.collation }
      : {}),
    ...(nonnegative(value.approx_rows)
      ? { approx_rows: value.approx_rows }
      : {}),
    ...(nonnegative(value.data_bytes) ? { data_bytes: value.data_bytes } : {}),
    ...(nonnegative(value.index_bytes)
      ? { index_bytes: value.index_bytes }
      : {}),
    ...(nonnegative(value.free_bytes) ? { free_bytes: value.free_bytes } : {}),
    estimate_known: value.estimate_known,
  };
}

export function parseDatabaseMaintenancePage(
  value: unknown,
  instanceID: string,
  database: string,
): DatabaseMaintenancePage | undefined {
  if (
    !object(value) ||
    value.instance_id !== instanceID ||
    value.database !== database ||
    !Array.isArray(value.items) ||
    value.items.length > 200 ||
    !nonnegative(value.total) ||
    value.total > 2000 ||
    value.total < value.items.length ||
    typeof value.truncated !== "boolean" ||
    !digest.test(String(value.schema_sha256)) ||
    !positive(value.instance_revision) ||
    !positive(value.database_revision) ||
    typeof value.replication_source_attached !== "boolean"
  )
    return undefined;
  const items = value.items.map(parseTable);
  if (items.some((item) => !item)) return undefined;
  const rows = items as DatabaseMaintenanceTable[];
  if (new Set(rows.map((item) => item.name)).size !== rows.length)
    return undefined;
  if (value.truncated) {
    if (!tableName(value.next_after) || rows.at(-1)?.name !== value.next_after)
      return undefined;
  } else if (value.next_after !== undefined && value.next_after !== "") {
    return undefined;
  }
  return {
    instance_id: instanceID,
    database,
    items: rows,
    total: value.total,
    truncated: value.truncated,
    ...(typeof value.next_after === "string" && value.next_after
      ? { next_after: value.next_after }
      : {}),
    schema_sha256: value.schema_sha256 as string,
    instance_revision: value.instance_revision,
    database_revision: value.database_revision,
    replication_source_attached: value.replication_source_attached,
  };
}

export function databaseTableCanMaintain(
  table: DatabaseMaintenanceTable,
  operation: DatabaseMaintenanceOperation,
): boolean {
  if (table.type !== "BASE TABLE") return false;
  const engine = table.engine.toLowerCase();
  if (operation === "optimize")
    return engine === "innodb" || engine === "myisam";
  return operation === "repair" && engine === "myisam";
}

export function databaseMaintenanceHasUnknownOutcome(
  result: DatabaseMaintenanceInspection | undefined,
): boolean {
  return (
    !!result &&
    (result.state === "outcome_unknown" ||
      result.outcome_unknown_count > 0 ||
      result.items.some((item) => item.state === "outcome_unknown"))
  );
}

export function databaseTablePreviewInput(
  page: DatabaseMaintenancePage | undefined,
  instanceID: string,
  database: string,
  operation: DatabaseMaintenanceOperation,
  tables: DatabaseMaintenanceTable[],
): Record<string, unknown> | undefined {
  if (
    !page ||
    page.instance_id !== instanceID ||
    page.database !== database ||
    tables.length < 1 ||
    tables.length > 25 ||
    new Set(tables.map((table) => table.name)).size !== tables.length ||
    tables.some((table) => !databaseTableCanMaintain(table, operation))
  )
    return undefined;
  return {
    instance_id: instanceID,
    database,
    operation,
    tables: tables.map((table) => table.name),
    expected_instance_revision: page.instance_revision,
    expected_database_revision: page.database_revision,
    expected_schema_sha256: page.schema_sha256,
  };
}

export function parseDatabaseMaintenancePreview(
  value: unknown,
  expected: {
    page: DatabaseMaintenancePage;
    engine: string;
    operation: DatabaseMaintenanceOperation;
    tables: DatabaseMaintenanceTable[];
    now?: number;
  },
): DatabaseMaintenancePreview | undefined {
  if (
    !object(value) ||
    !previewID.test(String(value.preview_id)) ||
    !time(value.expires_at) ||
    value.instance_id !== expected.page.instance_id ||
    value.database !== expected.page.database ||
    value.engine !== expected.engine ||
    !["mysql", "mysql-community", "mariadb"].includes(String(value.engine)) ||
    value.operation !== expected.operation ||
    !Array.isArray(value.tables) ||
    value.table_count !== expected.tables.length ||
    value.tables.length !== expected.tables.length ||
    !digest.test(String(value.schema_sha256)) ||
    value.schema_sha256 !== expected.page.schema_sha256 ||
    !digest.test(String(value.snapshot_sha256)) ||
    value.instance_revision !== expected.page.instance_revision ||
    value.database_revision !== expected.page.database_revision ||
    !nonnegative(value.estimated_bytes) ||
    typeof value.estimate_known !== "boolean" ||
    typeof value.may_need_comparable_temporary_space !== "boolean" ||
    typeof value.lock_impact !== "string" ||
    value.lock_impact.length === 0 ||
    typeof value.replica_impact !== "string" ||
    value.replica_impact.length === 0 ||
    !Array.isArray(value.warnings) ||
    value.warnings.some((warning) => typeof warning !== "string") ||
    value.confirmation_required !== true
  )
    return undefined;
  const items = value.tables.map(parseTable);
  if (items.some((item) => !item)) return undefined;
  const parsedTables = items as DatabaseMaintenanceTable[];
  const names = parsedTables.map((item) => item.name);
  if (
    new Set(names).size !== names.length ||
    expected.tables.some((table) => !names.includes(table.name)) ||
    parsedTables.some(
      (table) => !databaseTableCanMaintain(table, expected.operation),
    )
  )
    return undefined;
  const now = expected.now ?? Date.now();
  if (Date.parse(value.expires_at) <= now) return undefined;
  return {
    preview_id: value.preview_id as string,
    expires_at: value.expires_at,
    instance_id: value.instance_id as string,
    database: value.database as string,
    engine: value.engine,
    operation: expected.operation,
    tables: parsedTables,
    table_count: value.table_count as number,
    schema_sha256: value.schema_sha256 as string,
    snapshot_sha256: value.snapshot_sha256 as string,
    instance_revision: value.instance_revision as number,
    database_revision: value.database_revision as number,
    estimated_bytes: value.estimated_bytes,
    estimate_known: value.estimate_known,
    may_need_comparable_temporary_space:
      value.may_need_comparable_temporary_space,
    lock_impact: value.lock_impact,
    replica_impact: value.replica_impact,
    warnings: value.warnings as string[],
    confirmation_required: true,
  };
}

export function databaseTableRunInput(
  preview: DatabaseMaintenancePreview | undefined,
  acknowledged: boolean,
  now = Date.now(),
): Record<string, unknown> | undefined {
  if (!preview || !acknowledged || Date.parse(preview.expires_at) <= now)
    return undefined;
  return {
    instance_id: preview.instance_id,
    database: preview.database,
    preview_id: preview.preview_id,
    snapshot_sha256: preview.snapshot_sha256,
    expected_instance_revision: preview.instance_revision,
    expected_database_revision: preview.database_revision,
    confirm_maintenance: true,
  };
}

const resultStates = new Set([
  "ready",
  "expired",
  "not_started",
  "outcome_unknown",
  "completed",
  "partial",
  "failed",
  "canceled",
]);
const itemStates = new Set([
  "not_started",
  "completed",
  "failed",
  "outcome_unknown",
]);

export function parseDatabaseMaintenanceInspection(
  value: unknown,
  instanceID: string,
  database: string,
  expectedPreviewID: string,
): DatabaseMaintenanceInspection | undefined {
  if (
    !object(value) ||
    value.preview_id !== expectedPreviewID ||
    !previewID.test(expectedPreviewID) ||
    value.instance_id !== instanceID ||
    value.database !== database ||
    !["mysql", "mysql-community", "mariadb"].includes(String(value.engine)) ||
    (value.operation !== "optimize" && value.operation !== "repair") ||
    typeof value.state !== "string" ||
    !resultStates.has(value.state) ||
    !positive(value.instance_revision) ||
    !positive(value.database_revision) ||
    !digest.test(String(value.schema_sha256)) ||
    !nonnegative(value.completed_count) ||
    !nonnegative(value.failed_count) ||
    !nonnegative(value.outcome_unknown_count) ||
    (value.task_id !== undefined && !taskID.test(String(value.task_id))) ||
    (value.started_at !== undefined && !time(value.started_at)) ||
    (value.finished_at !== undefined && !time(value.finished_at)) ||
    (value.items !== null && !Array.isArray(value.items)) ||
    typeof value.replication_source_attached !== "boolean"
  )
    return undefined;
  const rawItems = Array.isArray(value.items) ? value.items : [];
  const items: DatabaseMaintenanceItemResult[] = [];
  for (const raw of rawItems) {
    if (
      !object(raw) ||
      !tableName(raw.name) ||
      typeof raw.engine !== "string" ||
      typeof raw.state !== "string" ||
      !itemStates.has(raw.state) ||
      !Array.isArray(raw.messages)
    )
      return undefined;
    const messages: Array<{ msg_type: string; msg_text: string }> = [];
    for (const message of raw.messages) {
      if (
        !object(message) ||
        typeof message.msg_type !== "string" ||
        typeof message.msg_text !== "string"
      )
        return undefined;
      messages.push({ msg_type: message.msg_type, msg_text: message.msg_text });
    }
    items.push({
      name: raw.name,
      engine: raw.engine,
      state: raw.state as DatabaseMaintenanceItemResult["state"],
      messages,
    });
  }
  if (
    new Set(items.map((item) => item.name)).size !== items.length ||
    value.completed_count !==
      items.filter((item) => item.state === "completed").length ||
    value.failed_count !==
      items.filter((item) => item.state === "failed").length ||
    value.outcome_unknown_count !==
      items.filter((item) => item.state === "outcome_unknown").length
  )
    return undefined;
  return {
    preview_id: value.preview_id,
    ...(typeof value.task_id === "string" ? { task_id: value.task_id } : {}),
    instance_id: instanceID,
    database,
    engine: value.engine as string,
    operation: value.operation as DatabaseMaintenanceOperation,
    state: value.state as DatabaseMaintenanceInspection["state"],
    instance_revision: value.instance_revision,
    database_revision: value.database_revision,
    schema_sha256: value.schema_sha256 as string,
    ...(typeof value.started_at === "string"
      ? { started_at: value.started_at }
      : {}),
    ...(typeof value.finished_at === "string"
      ? { finished_at: value.finished_at }
      : {}),
    completed_count: value.completed_count,
    failed_count: value.failed_count,
    outcome_unknown_count: value.outcome_unknown_count,
    items,
    replication_source_attached: value.replication_source_attached,
  };
}

export function databaseTableInspectInput(
  instanceID: string,
  database: string,
  id: string,
): Record<string, unknown> | undefined {
  return previewID.test(id)
    ? { instance_id: instanceID, database, preview_id: id }
    : undefined;
}
