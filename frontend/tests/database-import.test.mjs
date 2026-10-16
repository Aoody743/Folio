import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";

const compiled = ts.transpileModule(
  readFileSync(new URL("../src/lib/database-import.ts", import.meta.url), "utf8"),
  { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } },
).outputText;
const { verifiedDatabaseImport } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`);
const target = { instance: "mariadb-owned", database: "example", revision: 3 };
const uploaded = {
  id: "dbimport_" + "a".repeat(32), instance_id: target.instance, database: target.database,
  engine: "mariadb", database_revision: 3, revision: 1, state: "uploaded",
  bytes: 1234, bytes_received: 1234, sha256: "b".repeat(64), created_at: "2026-10-14T12:00:00Z",
};

test("only a complete import bound to the selected database revision can be reviewed", () => {
  assert.deepEqual(verifiedDatabaseImport(uploaded, target), uploaded);
  for (const change of [
    { instance_id: "mariadb-other" }, { database: "other" }, { database_revision: 2 },
    { state: "uploading" }, { state: "applying" }, { state: "recovery_required" },
    { bytes_received: 1200 }, { sha256: "bad-hash" }, { revision: 0 },
    { bytes: 4 * 1024 ** 3 + 1, bytes_received: 4 * 1024 ** 3 + 1 },
  ]) assert.equal(verifiedDatabaseImport({ ...uploaded, ...change }, target), undefined);
});

test("a committed backup or unknown file reference cannot masquerade as an external import", () => {
  for (const id of ["dbbackup_" + "a".repeat(32), "../dump.sql", "dbimport_" + "A".repeat(32)])
    assert.equal(verifiedDatabaseImport({ ...uploaded, id }, target), undefined);
  assert.equal(verifiedDatabaseImport(null, target), undefined);
  assert.equal(verifiedDatabaseImport({}, target), undefined);
});
