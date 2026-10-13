import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

async function loadTypeScript(path) {
  const js = ts.transpileModule(
    readFileSync(new URL(path, import.meta.url), "utf8"),
    {
      compilerOptions: {
        target: ts.ScriptTarget.ES2022,
        module: ts.ModuleKind.ESNext,
      },
    },
  ).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
}

const {
  isActiveManagedDatabaseState,
  canRecreateDeletedManagedDatabase,
  isDeletedManagedDatabaseState,
  isMissingManagedDatabaseReceipt,
  managedDatabaseCreateErrorMessage,
  managedDatabaseCreatePreset,
  parseManagedDatabaseReceipt,
} = await loadTypeScript("../src/lib/database-recreation.ts");
const { applyLockedPreset } = await loadTypeScript("../src/lib/schema.ts");
const { APIError, apiErrorMessage } = await loadTypeScript("../src/lib/api.ts");
const actionFormSource = readFileSync(
  new URL("../src/components/actions.tsx", import.meta.url),
  "utf8",
);

const receipt = {
  instance_id: "db-1",
  database: "app_main",
  username: "app_user",
  engine: "postgresql",
  permission: "read_write",
  state: "deleted",
  revision: 7,
  password: "must-not-enter-ui-state",
};

test("only active receipts expose account and backup controls", () => {
  for (const state of [
    undefined,
    "deleted",
    "creating",
    "updating",
    "deleting",
    "recovery_required",
  ]) {
    assert.equal(isActiveManagedDatabaseState(state), false, state);
  }
  assert.equal(isActiveManagedDatabaseState("active"), true);
  assert.equal(isDeletedManagedDatabaseState("deleted"), true);
  assert.equal(isDeletedManagedDatabaseState("active"), false);
  assert.equal(isDeletedManagedDatabaseState(undefined), false);
  assert.equal(
    canRecreateDeletedManagedDatabase({ engine: "postgresql", state: "deleted" }),
    true,
  );
  assert.equal(
    canRecreateDeletedManagedDatabase({ engine: "mongodb", state: "deleted" }),
    false,
  );
  assert.equal(
    canRecreateDeletedManagedDatabase({ engine: "postgresql", state: "active" }),
    false,
  );
});

test("receipt parsing keeps only public fields and validates the exact target", () => {
  const parsed = parseManagedDatabaseReceipt(receipt, "db-1", "app_main");
  assert.deepEqual(parsed, {
    instance_id: "db-1",
    database: "app_main",
    username: "app_user",
    engine: "postgresql",
    permission: "read_write",
    state: "deleted",
    revision: 7,
  });
  assert.equal("password" in parsed, false);
  assert.throws(
    () => parseManagedDatabaseReceipt(receipt, "db-2", "app_main"),
    /管理记录无效/,
  );
  assert.throws(
    () =>
      parseManagedDatabaseReceipt(
        { ...receipt, revision: Number.MAX_SAFE_INTEGER + 1 },
        "db-1",
        "app_main",
      ),
    /管理记录无效/,
  );
});

test("creation presets use the exact tombstone revision or zero for a fresh name", () => {
  assert.deepEqual(
    managedDatabaseCreatePreset("db-1", "app_main", receipt),
    {
      instance_id: "db-1",
      database: "app_main",
      username: "app_user",
      permission: "read_write",
      expected_revision: 7,
    },
  );
  assert.deepEqual(managedDatabaseCreatePreset("db-1", "brand_new"), {
    instance_id: "db-1",
    database: "brand_new",
    expected_revision: 0,
  });
  assert.throws(
    () =>
      managedDatabaseCreatePreset("db-1", "app_main", {
        ...receipt,
        state: "active",
      }),
    /已删除记录版本/,
  );
  assert.throws(
    () =>
      managedDatabaseCreatePreset("db-1", "app_main", {
        ...receipt,
        engine: "mongodb",
      }),
    /MongoDB 暂不支持重建已删除数据库/,
  );
});

test("locked form values preserve the checked target and revision in JSON mode", () => {
  assert.deepEqual(
    applyLockedPreset(
      {
        instance_id: "other-instance",
        database: "other_database",
        expected_revision: 0,
        permission: "read",
      },
      { instance_id: "db-1", database: "app_main", expected_revision: 7 },
      ["instance_id", "database", "expected_revision"],
    ),
    {
      instance_id: "db-1",
      database: "app_main",
      expected_revision: 7,
      permission: "read",
    },
  );
});

test("only the known missing-metadata 404 is fresh and API errors use the formatter", () => {
  assert.equal(
    isMissingManagedDatabaseReceipt({
      status: 404,
      code: "database_receipt_not_found",
      message: "Managed database receipt does not exist for this instance and database",
    }),
    true,
  );
  assert.equal(
    isMissingManagedDatabaseReceipt({
      status: 404,
      code: "database_receipt_not_found",
      message: "wording is not used for classification",
    }),
    true,
  );
  assert.equal(
    isMissingManagedDatabaseReceipt({
      status: 404,
      code: "not_found",
      message: "Managed software record does not exist",
    }),
    false,
  );
  assert.equal(
    isMissingManagedDatabaseReceipt({
      status: 404,
      code: "route_missing",
      message: "Managed database receipt does not exist for this instance and database",
    }),
    false,
  );
  assert.equal(
    isMissingManagedDatabaseReceipt({
      status: 403,
      code: "database_receipt_not_found",
    }),
    false,
  );
  assert.equal(
    apiErrorMessage(
      new APIError(
        "ownership_conflict",
        "Existing server database is not owned",
        409,
      ),
      (error) =>
        managedDatabaseCreateErrorMessage(
          error.code,
          error.status,
          error.message,
        ),
    ),
    "发现同名数据库或账户不属于当前面板，服务已拒绝创建（ownership_conflict）：Existing server database is not owned",
  );
  assert.match(
    actionFormSource,
    /catch \(e\) \{\s*setError\(apiErrorMessage\(e, formatError\)\);\s*if \(e instanceof APIError\)/,
  );
  assert.match(
    managedDatabaseCreateErrorMessage(
      "ownership_conflict",
      409,
      "Existing server database is not owned",
    ),
    /不属于当前面板/,
  );
  assert.equal(
    managedDatabaseCreateErrorMessage("some_error", 400, "original"),
    undefined,
  );
});
