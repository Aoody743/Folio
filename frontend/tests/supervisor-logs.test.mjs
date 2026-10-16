import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const source = readFileSync(
  new URL("../src/lib/supervisor-logs.ts", import.meta.url),
  "utf8",
);
const js = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText;
const {
  supervisorCapability,
  supervisorFileIdentity,
  supervisorJobs,
  supervisorLogClearInput,
  supervisorLogClearPreview,
  supervisorLogRead,
} = await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);

const identity = { exists: true, size: "9007199254740993123", mod_time_ns: "1781462400123456789", device: "8", inode: "1844674407370955" };
const absent = { exists: false, size: "0", mod_time_ns: "0", device: "0", inode: "0" };

test("Supervisor job inventory admits only registered unique names", () => {
  assert.deepEqual(
    supervisorJobs({ items: [{ name: "worker", program_id: "worker", state: "running" }], revision: 7 }),
    { items: [{ name: "worker", program_id: "worker", state: "running" }], revision: 7 },
  );
  assert.equal(supervisorJobs({ items: [{ name: "worker" }, { name: "worker" }], revision: 7 }), undefined);
  assert.equal(supervisorJobs({ items: [{ name: "" }], revision: 7 }), undefined);
});

test("file identities retain exact decimal strings and reject malformed absent or present records", () => {
  assert.deepEqual(supervisorFileIdentity(identity), identity);
  assert.deepEqual(supervisorFileIdentity(absent), absent);
  assert.deepEqual(supervisorFileIdentity({ ...identity, device: "0" }), { ...identity, device: "0" });
  assert.equal(supervisorFileIdentity({ ...identity, mod_time_ns: 1781462400123456789 }), undefined);
  assert.equal(supervisorFileIdentity({ ...identity, size: 9007199254740993 }), undefined);
  assert.equal(supervisorFileIdentity({ ...absent, inode: "1" }), undefined);
  assert.equal(supervisorFileIdentity({ ...identity, inode: "0" }), undefined);
});

test("log read is pinned to the selected registered job and stream", () => {
  const response = {
    id: "supervisor-1",
    name: "worker",
    stream: "stdout",
    text: "ready",
    bytes: 5,
    truncated: false,
    file_identity: identity,
  };
  assert.deepEqual(supervisorLogRead(response, "supervisor-1", "worker", "stdout"), response);
  assert.equal(supervisorLogRead(response, "supervisor-1", "other", "stdout"), undefined);
  assert.equal(supervisorLogRead({ ...response, bytes: 65537 }, "supervisor-1", "worker", "stdout"), undefined);
});

test("clear preview and task input always pin both streams", () => {
  const raw = {
    id: "supervisor-1",
    name: "worker",
    revision: 7,
    stdout: identity,
    stderr: absent,
    clears_streams: ["stdout", "stderr"],
  };
  const preview = supervisorLogClearPreview(raw, "supervisor-1", "worker");
  assert.deepEqual(supervisorLogClearInput(preview), {
    id: "supervisor-1",
    name: "worker",
    expected_revision: 7,
    expected_stdout: identity,
    expected_stderr: absent,
  });
  assert.equal(
    supervisorLogClearPreview({ ...raw, clears_streams: ["stdout"] }, "supervisor-1", "worker"),
    undefined,
  );
});

test("Supervisor capabilities require the dedicated scope, task flag and both identity schemas", () => {
  const clear = {
    name: "supervisor_log_clear",
    scope: "supervisor:manage",
    read_only: false,
    task: true,
    input_schema: {
      properties: {
        id: {},
        name: {},
        expected_revision: {},
        expected_stdout: { type: "object", properties: { exists: { type: "boolean" }, size: { type: "string", pattern: "^(0|[1-9][0-9]{0,18})$" }, mod_time_ns: { type: "string", pattern: "^(0|-?[1-9][0-9]{0,18})$" }, device: { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$" }, inode: { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$" } }, required: ["exists", "size", "mod_time_ns", "device", "inode"] },
        expected_stderr: { type: "object", properties: { exists: { type: "boolean" }, size: { type: "string", pattern: "^(0|[1-9][0-9]{0,18})$" }, mod_time_ns: { type: "string", pattern: "^(0|-?[1-9][0-9]{0,18})$" }, device: { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$" }, inode: { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$" } }, required: ["exists", "size", "mod_time_ns", "device", "inode"] },
      },
    },
  };
  const fields = ["id", "name", "expected_revision", "expected_stdout", "expected_stderr"];
  assert.equal(supervisorCapability([clear], clear.name, "supervisor:manage", fields, true), clear);
  assert.equal(supervisorCapability([{ ...clear, scope: "supervisor:read" }], clear.name, "supervisor:manage", fields, true), undefined);
  assert.equal(supervisorCapability([{ ...clear, input_schema: { properties: { ...clear.input_schema.properties, expected_stderr: { type: "object", properties: { exists: { type: "boolean" }, size: { type: "string", pattern: "^(0|[1-9][0-9]{0,18})$" }, mod_time_ns: { type: "string", pattern: "^(0|-?[1-9][0-9]{0,18})$" }, inode: { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$" } }, required: ["exists", "size", "mod_time_ns", "inode"] } } } }], clear.name, "supervisor:manage", fields, true), undefined);
});
