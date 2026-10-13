import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
const compiled = async (name) => {
  const source = readFileSync(
    new URL(`../src/lib/${name}.ts`, import.meta.url),
    "utf8",
  );
  const javascript = ts.transpileModule(source, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  }).outputText;
  return import(
    `data:text/javascript;base64,${Buffer.from(javascript).toString("base64")}`
  );
};
const { sqlCell, sqlColumns, validateSQLParameters } =
  await compiled("sql-wire");
const { normalizeInput } = await compiled("schema");
const { action, setCSRF } = await compiled("api");
const cursorSchema = {
  oneOf: [
    { type: "string", pattern: "^(0|[1-9][0-9]{0,19})$", maxLength: 20 },
    { type: "integer", minimum: 0, maximum: Number.MAX_SAFE_INTEGER },
  ],
};
test("SQL signed and unsigned integers display original decimal text; rounded or malformed values fail closed", () => {
  for (const value of [
    "9007199254740993",
    "-9007199254740993",
    "-9223372036854775808",
    "18446744073709551615",
  ])
    assert.deepEqual(sqlCell({ encoding: "integer", value }), {
      text: value,
      kind: "integer",
      detail: "精确整数",
    });
  assert.equal(sqlCell(42).text, "42");
  assert.equal(sqlCell(null).kind, "null");
  assert.equal(sqlCell(JSON.parse("9007199254740993")).kind, "unsafe");
  assert.equal(
    sqlCell({ encoding: "integer", value: "18446744073709551616" }).kind,
    "unsafe",
  );
  assert.equal(
    sqlCell({ encoding: "integer", value: 9007199254740992 }).kind,
    "unsafe",
  );
});
test("binary SQL values preserve bytes rather than attempting UTF-8 decoding", () => {
  const result = sqlCell({ encoding: "base64", data: "AP8C" });
  assert.equal(result.kind, "binary");
  assert.equal(result.text, "00 ff 02");
  assert.equal(result.detail, "3 字节 · 二进制");
  assert.equal(
    sqlCell({ encoding: "base64", data: "" }).detail,
    "0 字节 · 二进制",
  );
  assert.match(
    sqlCell({
      encoding: "base64",
      data: Buffer.alloc(65, 255).toString("base64"),
    }).text,
    / …$/,
  );
  assert.deepEqual(sqlColumns(["x", "x", "x_1"]), ["x", "x_1", "x_1_2"]);
});
test("Redis continuation strings retain all uint64 digits and reject overflow, non-decimal and rounded legacy numbers", () => {
  for (const value of ["0", "42", "9007199254740993", "18446744073709551615"])
    assert.equal(normalizeInput(cursorSchema, value), value);
  assert.equal(normalizeInput(cursorSchema, 42), 42);
  for (const value of [
    "18446744073709551616",
    "01",
    "-1",
    "1e3",
    " 42",
    JSON.parse("9007199254740993"),
  ])
    assert.throws(() => normalizeInput(cursorSchema, value), /游标/);
  assert.throws(
    () => normalizeInput({ type: "integer" }, "9007199254740993"),
    /整数/,
  );
});
test("SQL parameter normalization keeps decimal strings and typed integer objects; unsafe JSON numbers cannot be submitted", () => {
  const schema = {
    type: "object",
    properties: { parameters: { type: "array", items: {} } },
    additionalProperties: false,
  };
  const parameters = [
    "9007199254740993",
    { encoding: "integer", value: "18446744073709551615" },
    42,
    null,
  ];
  const normalized = normalizeInput(schema, {
    parameters: JSON.stringify(parameters),
  });
  validateSQLParameters(normalized);
  assert.deepEqual(normalized.parameters, parameters);
  assert.throws(
    () =>
      validateSQLParameters({ parameters: JSON.parse("[9007199254740993]") }),
    /大整数参数/,
  );
});
test("HTTP API round trip preserves tagged integers and posts exact SQL parameters and Redis cursor strings", async () => {
  const realFetch = globalThis.fetch;
  setCSRF("precision-test");
  const seen = [];
  try {
    globalThis.fetch = async (url, options) => {
      assert.equal(options.headers["X-CSRF-Token"], "precision-test");
      const input = JSON.parse(options.body);
      seen.push([url, input]);
      if (url.endsWith("database_query"))
        return new Response(
          JSON.stringify({
            data: {
              columns: ["value"],
              items: [
                { value: { encoding: "integer", value: "9007199254740993" } },
              ],
            },
          }),
        );
      return new Response(
        JSON.stringify({ data: { cursor: "18446744073709551615", items: [] } }),
      );
    };
    const parameters = [
      "9007199254740993",
      { encoding: "integer", value: "18446744073709551615" },
    ];
    const result = await action("database_query", {
      id: "profile",
      sql: "SELECT ?",
      parameters,
    });
    assert.equal(sqlCell(result.items[0].value).text, "9007199254740993");
    assert.deepEqual(seen[0][1].parameters, parameters);
    const scanned = await action("cache_keys_scan", {
      id: "namespace",
      cursor: normalizeInput(cursorSchema, "9007199254740993"),
    });
    assert.equal(seen[1][1].cursor, "9007199254740993");
    assert.equal(scanned.cursor, "18446744073709551615");
  } finally {
    globalThis.fetch = realFetch;
    setCSRF();
  }
});
