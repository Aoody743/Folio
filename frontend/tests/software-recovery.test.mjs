import { i18nURL } from "./i18n-loader.mjs";
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const source = ts.transpileModule(
  readFileSync(
    new URL("../src/lib/software-recovery.ts", import.meta.url),
    "utf8",
  ),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText.replace(/([\"\'])(?:@\/lib\/|\.\/)i18n\1/g, JSON.stringify(i18nURL));
const recovery = await import(
  `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`
);

const inspectCapability = {
  name: "software_recovery_inspect",
  read_only: true,
  input_schema: {
    type: "object",
    properties: { id: { type: "string" } },
    required: ["id"],
  },
};
const recoveryCapability = {
  name: "software_recovery",
  task: true,
  input_schema: {
    type: "object",
    properties: {
      id: { type: "string" },
      command: { type: "string", enum: ["continue", "stop_preserve"] },
      expected_journal_revision: { type: "integer", minimum: 1 },
    },
    required: ["id", "command", "expected_journal_revision"],
  },
};
const inspected = {
  id: "mariadb-default",
  state: "in_progress",
  checkpoint: "health_verified",
  revision: 7,
  available_actions: ["continue"],
  reason: "protected checkpoint",
  receipt_present: false,
};

test("recovery capabilities must advertise the expected schemas and task semantics", () => {
  assert.equal(
    recovery.hasSoftwareRecoveryInspectSchema(inspectCapability),
    true,
  );
  assert.equal(
    recovery.hasSoftwareRecoveryInspectSchema({
      ...inspectCapability,
      read_only: false,
    }),
    false,
  );
  assert.equal(
    recovery.hasSoftwareRecoveryActionSchema(recoveryCapability),
    true,
  );
  assert.equal(
    recovery.hasSoftwareRecoveryActionSchema({
      ...recoveryCapability,
      task: false,
    }),
    false,
  );
  assert.equal(
    recovery.hasSoftwareRecoveryActionSchema({
      ...recoveryCapability,
      input_schema: {
        ...recoveryCapability.input_schema,
        properties: {
          ...recoveryCapability.input_schema.properties,
          expected_journal_revision: { type: "integer", minimum: 0 },
        },
      },
    }),
    false,
  );
});

test("inspection parser supports missing receipts but rejects mismatched or unsafe state", () => {
  assert.deepEqual(
    recovery.parseSoftwareRecoveryInspection(
      {
        id: "mariadb-default",
        state: "journal_missing",
        available_actions: [],
        receipt_present: false,
      },
      "mariadb-default",
    ),
    {
      id: "mariadb-default",
      state: "journal_missing",
      available_actions: [],
      receipt_present: false,
    },
  );
  assert.equal(
    recovery.parseSoftwareRecoveryInspection(inspected, "postgresql-default"),
    undefined,
  );
  assert.equal(
    recovery.parseSoftwareRecoveryInspection(
      { ...inspected, revision: 0 },
      "mariadb-default",
    ),
    undefined,
  );
  assert.equal(
    recovery.parseSoftwareRecoveryInspection(
      { ...inspected, available_actions: ["continue", 42] },
      "mariadb-default",
    ),
    undefined,
  );
  assert.deepEqual(
    recovery.parseSoftwareRecoveryInspection(
      { ...inspected, available_actions: ["continue", "delete"] },
      "mariadb-default",
    ).available_actions,
    ["continue"],
  );
});

test("recovery input uses the current CAS revision and only a fresh advertised action", () => {
  const view = recovery.parseSoftwareRecoveryInspection(
    inspected,
    "mariadb-default",
  );
  assert.deepEqual(
    recovery.softwareRecoveryActionInput(
      recoveryCapability,
      view,
      "mariadb-default",
      "continue",
      true,
    ),
    {
      id: "mariadb-default",
      command: "continue",
      expected_journal_revision: 7,
    },
  );
  assert.equal(
    recovery.softwareRecoveryActionInput(
      recoveryCapability,
      view,
      "mariadb-default",
      "stop_preserve",
      true,
    ),
    undefined,
  );
  assert.equal(
    recovery.softwareRecoveryActionInput(
      recoveryCapability,
      view,
      "mariadb-default",
      "continue",
      false,
    ),
    undefined,
  );
  assert.equal(
    recovery.softwareRecoveryActionInput(
      recoveryCapability,
      view,
      "postgresql-default",
      "continue",
      true,
    ),
    undefined,
  );
});
