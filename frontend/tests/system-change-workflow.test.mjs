import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const source = ts.transpileModule(
  readFileSync(new URL("../src/lib/system-change-workflow.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText;
const workflow = await import(
  `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`
);

class MemoryStorage {
  values = new Map();
  getItem(key) {
    return this.values.get(key) ?? null;
  }
  setItem(key, value) {
    this.values.set(key, String(value));
  }
  removeItem(key) {
    this.values.delete(key);
  }
}

const planCapability = {
  name: "system_change_plan",
  input_schema: {
    type: "object",
    properties: {
      change: {
        type: "object",
        properties: { kind: { type: "string", enum: ["timezone", "power"] } },
        required: ["kind"],
      },
    },
    required: ["change"],
  },
};
const idCapability = (name) => ({
  name,
  input_schema: {
    type: "object",
    properties: { id: { type: "string", maxLength: 96 } },
    required: ["id"],
  },
});
const plan = (state, overrides = {}) => ({
  id: "chg_0123456789abcdef",
  state,
  change: { kind: "service_state", service_id: "nginx", service_state: "restart" },
  created_at: "2026-10-13T12:00:00Z",
  expires_at: "2026-10-13T12:15:00Z",
  ...overrides,
});

test("system workflow validates current capability schemas before constructing inputs", () => {
  assert.equal(workflow.hasSystemChangePlanSchema(planCapability), true);
  assert.equal(
    workflow.hasSystemChangePlanSchema({
      ...planCapability,
      input_schema: { ...planCapability.input_schema, required: [] },
    }),
    false,
  );
  const get = idCapability("system_change_get");
  assert.equal(workflow.hasSystemChangeGetSchema(get), true);
  assert.deepEqual(
    workflow.systemChangeIDInput(get, "system_change_get", "chg_1"),
    { id: "chg_1" },
  );
  assert.equal(
    workflow.systemChangeIDInput(get, "system_change_apply", "chg_1"),
    undefined,
  );
  assert.equal(
    workflow.hasSystemChangeIDSchema(
      { ...idCapability("system_change_apply"), input_schema: { properties: {} } },
      "system_change_apply",
    ),
    false,
  );
});

test("recovery key isolates host and actor and persists only plan/task intent identifiers", () => {
  const storage = new MemoryStorage();
  const key = workflow.systemChangeRecoveryKey("https://panel.example", "human-a");
  assert.notEqual(
    key,
    workflow.systemChangeRecoveryKey("https://other.example", "human-a"),
  );
  assert.notEqual(
    key,
    workflow.systemChangeRecoveryKey("https://panel.example", "human-b"),
  );
  const record = {
    planId: "chg_1",
    taskId: "task_1",
    pendingAction: "system_change_apply",
    intentKey: "intent_1",
    change: { kind: "ssh_config", private_key: "must not persist" },
  };
  workflow.writeSystemChangeRecovery(storage, key, record);
  assert.deepEqual(JSON.parse(storage.getItem(key)), {
    planId: "chg_1",
    taskId: "task_1",
    pendingAction: "system_change_apply",
    intentKey: "intent_1",
  });
  assert.deepEqual(workflow.readSystemChangeRecovery(storage, key), {
    planId: "chg_1",
    taskId: "task_1",
    pendingAction: "system_change_apply",
    intentKey: "intent_1",
  });
  workflow.writeSystemChangeRecovery(storage, key, undefined);
  assert.equal(workflow.readSystemChangeRecovery(storage, key), undefined);
});

test("failed browser storage probe blocks recovery-dependent mutation setup", () => {
  const broken = {
    getItem() {
      return null;
    },
    setItem() {
      throw new Error("storage blocked");
    },
    removeItem() {},
  };
  assert.equal(workflow.canUseSystemChangeStorage(broken, "key"), false);
  assert.equal(workflow.canUseSystemChangeStorage(new MemoryStorage(), "key"), true);
});

test("plan actions require a fresh plan and backend-supported state/deadline", () => {
  const now = Date.parse("2026-10-13T12:05:00Z");
  const options = { fresh: true, blocked: false, now };
  assert.equal(
    workflow.systemChangeActionAllowed(plan("planned"), "system_change_apply", options),
    true,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(
      plan("planned", { expires_at: "2026-10-13T12:01:00Z" }),
      "system_change_apply",
      options,
    ),
    false,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(
      plan("awaiting_confirmation", {
        confirmation_deadline: "2026-10-13T12:10:00Z",
      }),
      "system_change_confirm",
      options,
    ),
    true,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(
      plan("awaiting_confirmation", {
        confirmation_deadline: "2026-10-13T12:04:00Z",
      }),
      "system_change_confirm",
      options,
    ),
    false,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(
      plan("recovery_required"),
      "system_change_rollback",
      options,
    ),
    true,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(plan("applied"), "system_change_rollback", {
      ...options,
      blocked: true,
    }),
    false,
  );
  assert.equal(
    workflow.systemChangeActionAllowed(plan("planned"), "system_change_apply", {
      ...options,
      fresh: false,
    }),
    false,
  );
});

test("task submissions and terminal states are read from actual response envelopes", () => {
  assert.equal(workflow.systemChangeTaskID({ task_id: "task-a" }), "task-a");
  assert.equal(workflow.systemChangeTaskID({ task: { id: "task-b" } }), "task-b");
  assert.equal(workflow.systemChangeTaskID({ status: "submitted" }), undefined);
  assert.equal(workflow.systemChangeTaskFinished("failed"), true);
  assert.equal(workflow.systemChangeTaskFinished("running"), false);
});

test("plan preview parser rejects incomplete service responses instead of inventing state", () => {
  assert.deepEqual(workflow.parseSystemChangePlan(plan("planned")), plan("planned"));
  assert.equal(
    workflow.parseSystemChangePlan({ id: "chg_missing", change: { kind: "timezone" } }),
    undefined,
  );
  assert.equal(
    workflow.parseSystemChangePlan({ id: "chg_missing", state: "planned", change: [] }),
    undefined,
  );
});

test("recovery intents clear only after a matching backend state transition", () => {
  const now = Date.parse("2026-10-13T12:05:00Z");
  assert.equal(
    workflow.systemChangeIntentResolved(plan("planned"), "system_change_apply", now),
    false,
  );
  assert.equal(
    workflow.systemChangeIntentResolved(plan("awaiting_confirmation"), "system_change_apply", now),
    true,
  );
  assert.equal(
    workflow.systemChangeIntentResolved(plan("awaiting_confirmation"), "system_change_confirm", now),
    false,
  );
  assert.equal(
    workflow.systemChangeIntentResolved(plan("applied"), "system_change_confirm", now),
    true,
  );
  assert.equal(
    workflow.systemChangeIntentResolved(plan("applied"), "system_change_rollback", now),
    false,
  );
  assert.equal(
    workflow.systemChangeIntentResolved(plan("rolled_back"), "system_change_rollback", now),
    true,
  );
});
