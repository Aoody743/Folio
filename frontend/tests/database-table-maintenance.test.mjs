import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

async function loadTypeScript(path) {
  const source = readFileSync(new URL(path, import.meta.url), "utf8");
  const js = ts.transpileModule(source, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
}

const maintenance = await loadTypeScript("../src/lib/database-table-maintenance.ts");
const observation = await loadTypeScript("../src/lib/task-observation.ts");
const hash = "a".repeat(64);
const table = (name, engine, overrides = {}) => ({
  name,
  type: "BASE TABLE",
  engine,
  collation: "utf8mb4_general_ci",
  approx_rows: 10,
  data_bytes: 1024,
  index_bytes: 512,
  free_bytes: 0,
  estimate_known: true,
  ...overrides,
});
const text = { type: "string" };
const revision = { type: "integer", minimum: 1, maximum: 9223372036854775807 };
const digestSchema = { type: "string", pattern: "^[0-9a-f]{64}$" };
const previewSchema = { type: "string", pattern: "^plan_[0-9a-f]{48}$" };

function capability(name, scope, task, read_only, fields, required, extra = {}) {
  const properties = { instance_id: text, database: text, ...fields };
  return {
    name,
    scope,
    task,
    read_only,
    input_schema: { type: "object", required, properties },
  };
}

function capabilities() {
  return [
    capability("managed_database_tables_list", "databases:read", false, true, {
      limit: { type: "integer", minimum: 0, maximum: 200 },
      after: { type: "string", maxLength: 64 },
    }, ["instance_id", "database"]),
    capability("managed_database_table_maintenance_preview", "databases:admin", false, false, {
      operation: { type: "string", enum: ["optimize", "repair"] },
      tables: { type: "array", minItems: 1, maxItems: 25, uniqueItems: true, items: { type: "string", minLength: 1, maxLength: 64 } },
      expected_instance_revision: revision,
      expected_database_revision: revision,
      expected_schema_sha256: digestSchema,
    }, ["instance_id", "database", "operation", "tables", "expected_instance_revision", "expected_database_revision", "expected_schema_sha256"]),
    capability("managed_database_table_maintenance", "databases:admin", true, false, {
      preview_id: previewSchema,
      snapshot_sha256: digestSchema,
      expected_instance_revision: revision,
      expected_database_revision: revision,
      confirm_maintenance: { type: "boolean", const: true },
    }, ["instance_id", "database", "preview_id", "snapshot_sha256", "expected_instance_revision", "expected_database_revision", "confirm_maintenance"]),
    capability("managed_database_table_maintenance_inspect", "databases:read", false, true, {
      preview_id: previewSchema,
    }, ["instance_id", "database", "preview_id"]),
  ];
}

const page = {
  instance_id: "mysql-main",
  database: "app_data",
  items: [table("orders", "InnoDB"), table("archive", "MyISAM")],
  total: 3,
  truncated: true,
  next_after: "archive",
  schema_sha256: hash,
  instance_revision: 7,
  database_revision: 11,
  replication_source_attached: true,
};
const preview = {
  preview_id: `plan_${"b".repeat(48)}`,
  expires_at: new Date(Date.now() + 5 * 60_000).toISOString(),
  instance_id: "mysql-main",
  database: "app_data",
  engine: "mysql-community",
  operation: "optimize",
  tables: [table("orders", "InnoDB")],
  table_count: 1,
  schema_sha256: hash,
  snapshot_sha256: "c".repeat(64),
  instance_revision: 7,
  database_revision: 11,
  estimated_bytes: 1536,
  estimate_known: true,
  may_need_comparable_temporary_space: true,
  lock_impact: "May wait for table locks.",
  replica_impact: "May add replica load.",
  warnings: ["A second administrator can change a definition."],
  confirmation_required: true,
};

test("table-maintenance capabilities require exact read/admin scope and task modes", () => {
  const found = maintenance.databaseTableMaintenanceCapabilities(capabilities());
  assert.equal(found.list.name, "managed_database_tables_list");
  assert.equal(found.preview.name, "managed_database_table_maintenance_preview");
  assert.equal(found.run.name, "managed_database_table_maintenance");
  assert.equal(found.inspect.name, "managed_database_table_maintenance_inspect");
  const wrongBounds = capabilities().map((item) => ({ ...item }));
  wrongBounds[0] = {
    ...wrongBounds[0],
    input_schema: {
      ...wrongBounds[0].input_schema,
      properties: {
        ...wrongBounds[0].input_schema.properties,
        limit: { type: "integer", minimum: 1, maximum: 200 },
      },
    },
  };
  assert.equal(maintenance.databaseTableMaintenanceCapabilities(wrongBounds).list, undefined);
  const nonUniqueSelection = capabilities().map((item) => ({ ...item }));
  nonUniqueSelection[1] = {
    ...nonUniqueSelection[1],
    input_schema: {
      ...nonUniqueSelection[1].input_schema,
      properties: {
        ...nonUniqueSelection[1].input_schema.properties,
        tables: { ...nonUniqueSelection[1].input_schema.properties.tables, uniqueItems: false },
      },
    },
  };
  assert.equal(maintenance.databaseTableMaintenanceCapabilities(nonUniqueSelection).preview, undefined);
  const unsafe = capabilities().map((item) => ({ ...item }));
  unsafe[2] = { ...unsafe[2], scope: "databases:read" };
  unsafe[3] = { ...unsafe[3], read_only: false };
  assert.equal(maintenance.databaseTableMaintenanceCapabilities(unsafe).run, undefined);
  assert.equal(maintenance.databaseTableMaintenanceCapabilities(unsafe).inspect, undefined);
  const wrongConfirmation = capabilities().map((item) => ({ ...item }));
  wrongConfirmation[2] = {
    ...wrongConfirmation[2],
    input_schema: {
      ...wrongConfirmation[2].input_schema,
      properties: {
        ...wrongConfirmation[2].input_schema.properties,
        confirm_maintenance: { type: "boolean" },
      },
    },
  };
  assert.equal(maintenance.databaseTableMaintenanceCapabilities(wrongConfirmation).run, undefined);
});

test("table inventory pages pin identities and operation eligibility", () => {
  const parsed = maintenance.parseDatabaseMaintenancePage(page, "mysql-main", "app_data");
  assert.equal(parsed.items.length, 2);
  assert.equal(parsed.next_after, "archive");
  assert.equal(maintenance.parseDatabaseMaintenancePage({ ...page, instance_revision: 0 }, "mysql-main", "app_data"), undefined);
  assert.equal(maintenance.parseDatabaseMaintenancePage({ ...page, next_after: "orders" }, "mysql-main", "app_data"), undefined);
  assert.equal(maintenance.databaseTableCanMaintain(parsed.items[0], "optimize"), true);
  assert.equal(maintenance.databaseTableCanMaintain(parsed.items[0], "repair"), false);
  assert.equal(maintenance.databaseTableCanMaintain(parsed.items[1], "repair"), true);
  assert.equal(maintenance.databaseTableCanMaintain(table("view_data", "MyISAM", { type: "VIEW" }), "optimize"), false);
  const optimizeInput = maintenance.databaseTablePreviewInput(parsed, "mysql-main", "app_data", "optimize", [parsed.items[0]]);
  assert.deepEqual(optimizeInput, {
    instance_id: "mysql-main",
    database: "app_data",
    operation: "optimize",
    tables: ["orders"],
    expected_instance_revision: 7,
    expected_database_revision: 11,
    expected_schema_sha256: hash,
  });
  assert.equal(maintenance.databaseTablePreviewInput(parsed, "mysql-main", "app_data", "repair", [parsed.items[0]]), undefined);
  assert.equal(maintenance.databaseTablePreviewInput(parsed, "mysql-main", "app_data", "repair", Array(26).fill(parsed.items[1])), undefined);
});

test("table identifiers honor the backend UTF-8 character and byte limits", () => {
  const sixtyFourFourByteCharacters = "😀".repeat(64);
  const unicodePage = {
    ...page,
    items: [table(sixtyFourFourByteCharacters, "InnoDB")],
    total: 2,
    next_after: sixtyFourFourByteCharacters,
  };
  assert.equal(maintenance.parseDatabaseMaintenancePage(unicodePage, "mysql-main", "app_data").items[0].name, sixtyFourFourByteCharacters);
  assert.equal(maintenance.parseDatabaseMaintenancePage({
    ...unicodePage,
    items: [table(`${sixtyFourFourByteCharacters}😀`, "InnoDB")],
    next_after: `${sixtyFourFourByteCharacters}😀`,
  }, "mysql-main", "app_data"), undefined);
  assert.equal(maintenance.parseDatabaseMaintenancePage({
    ...unicodePage,
    items: [table("\uD800", "InnoDB")],
    next_after: "\uD800",
  }, "mysql-main", "app_data"), undefined);
});

test("maintenance preview must match selected tables, revisions and expiry before run", () => {
  const parsedPage = maintenance.parseDatabaseMaintenancePage(page, "mysql-main", "app_data");
  const expected = { page: parsedPage, engine: "mysql-community", operation: "optimize", tables: [parsedPage.items[0]] };
  const parsed = maintenance.parseDatabaseMaintenancePreview(preview, expected);
  assert.equal(parsed.preview_id, preview.preview_id);
  assert.equal(parsed.estimated_bytes, 1536);
  assert.equal(maintenance.parseDatabaseMaintenancePreview({ ...preview, database_revision: 10 }, expected), undefined);
  assert.equal(maintenance.parseDatabaseMaintenancePreview({ ...preview, tables: [table("other", "InnoDB")] }, expected), undefined);
  assert.equal(maintenance.parseDatabaseMaintenancePreview({ ...preview, engine: "mariadb" }, expected), undefined);
  assert.equal(maintenance.parseDatabaseMaintenancePreview(preview, { ...expected, now: Date.parse(preview.expires_at) }), undefined);
  assert.equal(maintenance.databaseTableRunInput(parsed, false), undefined);
  assert.deepEqual(maintenance.databaseTableRunInput(parsed, true), {
    instance_id: "mysql-main",
    database: "app_data",
    preview_id: preview.preview_id,
    snapshot_sha256: preview.snapshot_sha256,
    expected_instance_revision: 7,
    expected_database_revision: 11,
    confirm_maintenance: true,
  });
  assert.equal(maintenance.databaseTableRunInput(parsed, true, Date.parse(preview.expires_at)), undefined);
});

test("inspection identifies uncertain outcomes and never hides mismatched preview records", () => {
  const uncertain = {
    preview_id: preview.preview_id,
    task_id: "d".repeat(32),
    instance_id: "mysql-main",
    database: "app_data",
    engine: "mysql-community",
    operation: "optimize",
    state: "outcome_unknown",
    instance_revision: 7,
    database_revision: 11,
    schema_sha256: hash,
    started_at: new Date().toISOString(),
    finished_at: new Date().toISOString(),
    completed_count: 0,
    failed_count: 0,
    outcome_unknown_count: 1,
    items: [{
      name: "orders",
      engine: "InnoDB",
      state: "outcome_unknown",
      messages: [{ msg_type: "error", msg_text: "Server result could not be confirmed." }],
    }],
    replication_source_attached: true,
  };
  const parsed = maintenance.parseDatabaseMaintenanceInspection(uncertain, "mysql-main", "app_data", preview.preview_id);
  assert.equal(maintenance.databaseMaintenanceHasUnknownOutcome(parsed), true);
  assert.equal(maintenance.parseDatabaseMaintenanceInspection({
    ...uncertain,
    items: [{ ...uncertain.items[0], state: "executing" }],
  }, "mysql-main", "app_data", preview.preview_id), undefined);
  assert.equal(maintenance.parseDatabaseMaintenanceInspection({ ...uncertain, preview_id: `plan_${"e".repeat(48)}` }, "mysql-main", "app_data", preview.preview_id), undefined);
  const ready = maintenance.parseDatabaseMaintenanceInspection({
    ...uncertain,
    task_id: undefined,
    state: "ready",
    started_at: undefined,
    finished_at: undefined,
    outcome_unknown_count: 0,
    items: null,
  }, "mysql-main", "app_data", preview.preview_id);
  assert.equal(ready.state, "ready");
  assert.equal(maintenance.databaseMaintenanceHasUnknownOutcome(ready), false);
  assert.deepEqual(maintenance.databaseTableInspectInput("mysql-main", "app_data", preview.preview_id), {
    instance_id: "mysql-main",
    database: "app_data",
    preview_id: preview.preview_id,
  });
  assert.equal(maintenance.databaseTableInspectInput("mysql-main", "app_data", "bad-id"), undefined);
});

test("task result authorization errors stop observation without implying completion", () => {
  assert.equal(observation.isTaskObservationDenied({ status: 403, code: "forbidden" }), true);
  assert.equal(observation.isTaskObservationDenied({ status: 401, code: "login_required" }), true);
  assert.equal(observation.isTaskObservationDenied({ status: 400, code: "insufficient_scope" }), true);
  assert.equal(observation.isTaskObservationDenied({ status: 502, code: "upstream_error" }), false);
  assert.equal(observation.isTaskObservationDenied(new Error("timeout")), false);
});

test("task observation denial does not consume the later terminal notification", () => {
  let notifications = { unavailable: false, finished: false };
  const unavailable = observation.claimTaskObservationNotification(notifications, "unavailable");
  notifications = unavailable.state;
  assert.equal(unavailable.notify, true);

  const finished = observation.claimTaskObservationNotification(notifications, "finished");
  notifications = finished.state;
  assert.equal(finished.notify, true);

  assert.equal(observation.claimTaskObservationNotification(notifications, "unavailable").notify, false);
  assert.equal(observation.claimTaskObservationNotification(notifications, "finished").notify, false);
});
